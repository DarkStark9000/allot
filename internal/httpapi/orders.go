package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/money"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
)

const (
	maxBody   = 16 << 10
	maxKeyLen = 255
)

type createOrderRequest struct {
	SchemeCode  string           `json:"scheme_code"`
	Category    navdate.Category `json:"category"`
	AmountPaise int64            `json:"amount_paise"`
}

type orderResponse struct {
	ID          string             `json:"id"`
	State       order.State        `json:"state"`
	SchemeCode  string             `json:"scheme_code"`
	Category    navdate.Category   `json:"category"`
	AmountPaise int64              `json:"amount_paise"`
	ExchangeRef string             `json:"exchange_ref"`
	Version     int                `json:"version"`
	PaymentID   string             `json:"payment_id,omitzero"`
	NAVDate     string             `json:"nav_date,omitzero"`
	NAV         string             `json:"nav,omitzero"`
	Units       string             `json:"units,omitzero"`
	CreatedAt   time.Time          `json:"created_at"`
	History     []store.Transition `json:"history,omitzero"`
}

func toResponse(o order.Order, h []store.Transition) orderResponse {
	r := orderResponse{
		ID:          o.ID.String(),
		State:       o.State,
		SchemeCode:  o.SchemeCode,
		Category:    o.Category,
		AmountPaise: int64(o.Amount),
		ExchangeRef: o.ExchangeRef(),
		Version:     o.Version,
		PaymentID:   o.PaymentID,
		CreatedAt:   o.CreatedAt.UTC(),
		History:     h,
	}
	if o.State == order.Allotted {
		r.NAVDate, r.NAV, r.Units = o.NAVDate.String(), o.NAV.String(), o.Units.String()
	}
	return r
}

// createOrder places an order. It follows the IETF Idempotency-Key draft: a retry with the
// same key and body gets the stored response, the same key with a different body gets 422,
// and the same key while the first request still runs gets 409.
func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-User-ID") // demo authentication; a real service reads a verified token
	if user == "" {
		s.problem(w, http.StatusUnauthorized, "unauthenticated", "X-User-ID header is required", "")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > maxKeyLen {
		s.problem(w, http.StatusBadRequest, "idempotency-key-required",
			"Idempotency-Key header is required", "Send a unique key of at most 255 characters with every POST.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		s.problem(w, http.StatusRequestEntityTooLarge, "body-too-large", "Request body is too large", "")
		return
	}
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))

	claim, err := s.db.ClaimKey(r.Context(), user, key, sum[:], s.lease)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	switch claim.Outcome {
	case store.Mismatch:
		s.problem(w, http.StatusUnprocessableEntity, "idempotency-key-reused",
			"Idempotency-Key was used with a different request", "Use a new key for a new request.")
		return
	case store.InFlight:
		w.Header().Set("Retry-After", "1")
		s.problem(w, http.StatusConflict, "idempotency-key-in-use",
			"A request with this Idempotency-Key is still being processed", "Retry after the first request completes.")
		return
	case store.Replay:
		w.Header().Set("Idempotent-Replayed", "true")
		write(w, claim.Code, contentTypeFor(claim.Code), claim.Body)
		return
	}

	code, resp, err := s.place(r.Context(), user, key, body)
	if err != nil {
		if rerr := s.db.ReleaseKey(context.WithoutCancel(r.Context()), user, key); rerr != nil {
			s.log.Error("release idempotency key", "error", rerr)
		}
		s.unavailable(w, err)
		return
	}
	write(w, code, contentTypeFor(code), resp)
}

// place validates the request and creates the order. Validation failures are stored as the
// key's response, so a retry of the same bad request gets the same answer.
func (s *Server) place(ctx context.Context, user, key string, body []byte) (int, []byte, error) {
	var req createOrderRequest
	if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(true)); err != nil {
		resp := problemBody(http.StatusBadRequest, "invalid-body", "Request body is not a valid order", err.Error())
		return http.StatusBadRequest, resp, s.db.CompleteKey(ctx, user, key, http.StatusBadRequest, resp)
	}
	o, err := order.New(uuid.NewV7(), user, req.SchemeCode, req.Category, money.Paise(req.AmountPaise), s.now())
	if err != nil {
		resp := problemBody(http.StatusUnprocessableEntity, "invalid-order", "Order is not valid", err.Error())
		return http.StatusUnprocessableEntity, resp, s.db.CompleteKey(ctx, user, key, http.StatusUnprocessableEntity, resp)
	}
	var resp []byte
	err = s.db.WithTx(ctx, func(tx *store.Tx) error {
		err := tx.InsertOrder(ctx, o, key)
		if errors.Is(err, store.ErrDuplicateKey) {
			// A crashed holder committed the order before its lease ran out; answer with that order.
			o, err = tx.OrderByKey(ctx, user, key)
		}
		if err != nil {
			return err
		}
		if resp, err = json.Marshal(toResponse(o, nil)); err != nil {
			return err
		}
		return tx.CompleteKey(ctx, user, key, http.StatusCreated, resp)
	})
	return http.StatusCreated, resp, err
}

func contentTypeFor(code int) string {
	if code >= 400 {
		return "application/problem+json"
	}
	return "application/json"
}

func (s *Server) unavailable(w http.ResponseWriter, err error) {
	s.log.Error("database", "error", err)
	w.Header().Set("Retry-After", "1")
	s.problem(w, http.StatusServiceUnavailable, "unavailable", "Service temporarily unavailable", "Retry with the same Idempotency-Key.")
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.problem(w, http.StatusNotFound, "not-found", "Order not found", "")
		return
	}
	o, err := s.db.Order(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && o.UserID != r.Header.Get("X-User-ID")) {
		s.problem(w, http.StatusNotFound, "not-found", "Order not found", "")
		return
	}
	if err != nil {
		s.unavailable(w, err)
		return
	}
	h, err := s.db.History(r.Context(), id)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(o, h))
}
