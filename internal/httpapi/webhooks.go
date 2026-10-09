package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
)

// SignatureHeader carries the HMAC-SHA256 of the webhook body, as "sha256=<hex>".
const SignatureHeader = "Allot-Signature"

// Sign returns the signature header value for body. The fake gateway uses it too.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (s *Server) verified(header string, body []byte) bool {
	got, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, s.secret)
	m.Write(body)
	return hmac.Equal(sig, m.Sum(nil))
}

// GatewayEvent is the body of a payment gateway webhook.
type GatewayEvent struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OrderID    string    `json:"order_id"`
	PaymentID  string    `json:"payment_id,omitzero"`
	RefundID   string    `json:"refund_id,omitzero"`
	Reason     string    `json:"reason,omitzero"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (e GatewayEvent) toOrderEvent() (order.Event, error) {
	switch e.Type {
	case "payment.succeeded":
		return order.PaymentSucceeded{PaymentID: e.PaymentID, At: e.OccurredAt}, nil
	case "payment.failed":
		return order.PaymentDeclined{PaymentID: e.PaymentID, Reason: e.Reason}, nil
	case "refund.succeeded":
		return order.RefundSucceeded{RefundID: e.RefundID}, nil
	}
	return nil, fmt.Errorf("unknown event type %q", e.Type)
}

// paymentWebhook applies a gateway event at most once. Every delivery of a known event gets
// 200, so the gateway stops retrying; only a database failure gets 503, so it retries.
func (s *Server) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		s.problem(w, http.StatusRequestEntityTooLarge, "body-too-large", "Request body is too large", "")
		return
	}
	if !s.verified(r.Header.Get(SignatureHeader), body) {
		s.problem(w, http.StatusUnauthorized, "bad-signature", "Webhook signature is not valid", "")
		return
	}
	var ev GatewayEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		s.problem(w, http.StatusBadRequest, "invalid-body", "Webhook body is not a valid event", "")
		return
	}
	outcome, err := s.applyGatewayEvent(r.Context(), ev, body)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"outcome": outcome})
}

func (s *Server) applyGatewayEvent(ctx context.Context, ev GatewayEvent, body []byte) (string, error) {
	const source = "gateway"
	oid, idErr := uuid.Parse(ev.OrderID)
	e, typeErr := ev.toOrderEvent()
	var outcome string
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		fresh, err := tx.RecordInbound(ctx, source, ev.ID, oid, ev.Type, body)
		if err != nil {
			return err
		}
		if !fresh {
			outcome = "duplicate"
			return nil
		}
		switch {
		case idErr != nil:
			outcome = "unknown_order"
		case typeErr != nil:
			outcome = "ignored"
		default:
			outcome, err = tx.ApplyOrFlag(ctx, oid, e, store.Source{Name: source, EventID: ev.ID})
			if err != nil {
				return err
			}
		}
		return tx.SetInboundOutcome(ctx, source, ev.ID, outcome)
	})
	return outcome, err
}
