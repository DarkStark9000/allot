package fake

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/DarkStark9000/allot/internal/gateway"
	"github.com/DarkStark9000/allot/internal/httpapi"
)

// GatewayFaults shape how the fake gateway delivers webhooks.
type GatewayFaults struct {
	// Decline is the chance a payment fails.
	Decline float64
	// MaxCopies is the most times one webhook is delivered. Each event gets 1 to MaxCopies.
	MaxCopies int
	// MaxDelay is the longest wait before a delivery. Copies wait independently, so they reorder.
	MaxDelay time.Duration
	// StaleFailure is the chance that a successful payment is followed by a failure event.
	StaleFailure float64
	// Late is the chance that a successful payment's webhook waits LateBy first.
	Late   float64
	LateBy time.Duration
}

// Gateway is a fake payment gateway. It sends signed webhooks at least once, with faults.
type Gateway struct {
	webhookURL string
	secret     []byte
	faults     GatewayFaults
	client     *http.Client
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	mu       sync.Mutex
	rnd      *rand.Rand
	seq      int
	refunds  map[string]gateway.RefundRequest
	sent     int
	payments int
	inFlight int
}

// NewGateway returns a fake gateway that delivers webhooks to webhookURL, signed with secret.
func NewGateway(seed uint64, webhookURL string, secret []byte, f GatewayFaults) *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	if f.MaxCopies < 1 {
		f.MaxCopies = 1
	}
	return &Gateway{
		webhookURL: webhookURL,
		secret:     secret,
		faults:     f,
		client:     &http.Client{Timeout: 5 * time.Second},
		ctx:        ctx,
		cancel:     cancel,
		rnd:        rand.New(rand.NewPCG(seed, 0x47415445)),
		refunds:    make(map[string]gateway.RefundRequest),
	}
}

// Pay collects a payment for an order and reports the result by webhook, with faults.
func (g *Gateway) Pay(orderID string) {
	g.mu.Lock()
	g.payments++
	paymentID := fmt.Sprintf("pay_%d", g.payments)
	declined := g.rnd.Float64() < g.faults.Decline
	stale := !declined && g.rnd.Float64() < g.faults.StaleFailure
	late := !declined && g.rnd.Float64() < g.faults.Late
	g.mu.Unlock()

	now := time.Now()
	if declined {
		g.emit(httpapi.GatewayEvent{Type: "payment.failed", OrderID: orderID, PaymentID: paymentID, Reason: "declined by bank", OccurredAt: now}, 0)
		return
	}
	var wait time.Duration
	if late {
		wait = g.faults.LateBy
	}
	g.emit(httpapi.GatewayEvent{Type: "payment.succeeded", OrderID: orderID, PaymentID: paymentID, OccurredAt: now.Add(wait)}, wait)
	if stale {
		// A gateway bug: a failure for the same payment, sent after the success.
		g.emit(httpapi.GatewayEvent{Type: "payment.failed", OrderID: orderID, PaymentID: paymentID, Reason: "timeout", OccurredAt: now}, wait+g.faults.MaxDelay)
	}
}

// ServeHTTP serves POST /refunds. A reference is refunded once, however often it is asked.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/refunds" {
		http.NotFound(w, r)
		return
	}
	var req gateway.RefundRequest
	if err := json.UnmarshalRead(r.Body, &req); err != nil || req.Ref == "" {
		http.Error(w, "bad refund", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	_, seen := g.refunds[req.Ref]
	if !seen {
		g.refunds[req.Ref] = req
	}
	g.mu.Unlock()
	if !seen {
		g.emit(httpapi.GatewayEvent{Type: "refund.succeeded", OrderID: req.OrderID, RefundID: req.Ref, OccurredAt: time.Now()}, 0)
	}
	w.WriteHeader(http.StatusAccepted)
}

// emit gives the event an ID and delivers 1 to MaxCopies copies, each after its own delay.
func (g *Gateway) emit(ev httpapi.GatewayEvent, after time.Duration) {
	g.mu.Lock()
	g.seq++
	ev.ID = fmt.Sprintf("evt_%d", g.seq)
	copies := 1 + g.rnd.IntN(g.faults.MaxCopies)
	delays := make([]time.Duration, copies)
	for i := range delays {
		delays[i] = after
		if g.faults.MaxDelay > 0 {
			delays[i] += time.Duration(g.rnd.Int64N(int64(g.faults.MaxDelay)))
		}
	}
	g.inFlight += copies
	g.mu.Unlock()
	body, err := json.Marshal(ev)
	if err != nil {
		panic(err) // a GatewayEvent always encodes
	}
	for _, d := range delays {
		g.wg.Go(func() { g.deliver(body, d) })
	}
}

// deliver posts one webhook until allot answers 200, as real gateways do.
func (g *Gateway) deliver(body []byte, wait time.Duration) {
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()
	if !g.sleep(wait) {
		return
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(g.ctx, http.MethodPost, g.webhookURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(httpapi.SignatureHeader, httpapi.Sign(g.secret, body))
		resp, err := g.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				g.mu.Lock()
				g.sent++
				g.mu.Unlock()
				return
			}
		}
		if !g.sleep(time.Duration(min(attempt+1, 20)) * 50 * time.Millisecond) {
			return
		}
	}
}

func (g *Gateway) sleep(d time.Duration) bool {
	select {
	case <-g.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Wait blocks until every scheduled delivery has been accepted.
func (g *Gateway) Wait() { g.wg.Wait() }

// Idle reports whether no delivery is scheduled or in progress.
func (g *Gateway) Idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight == 0
}

// Close stops all deliveries.
func (g *Gateway) Close() {
	g.cancel()
	g.wg.Wait()
}

// Refunds returns every refund the gateway made, by reference.
func (g *Gateway) Refunds() map[string]gateway.RefundRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]gateway.RefundRequest, len(g.refunds))
	for k, v := range g.refunds {
		out[k] = v
	}
	return out
}

// Delivered returns how many webhook deliveries allot accepted, copies included.
func (g *Gateway) Delivered() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sent
}
