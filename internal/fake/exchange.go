// Package fake provides an exchange, a payment gateway, and a registrar that misbehave on
// purpose. They speak HTTP, so faults cross a real network boundary. Every fault is drawn
// from a seeded generator, so a failing run can be replayed from its seed.
//
// They are modeled on public descriptions of these systems, not on any private API.
package fake

import (
	"encoding/json/v2"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/DarkStark9000/allot/internal/exchange"
)

// ExchangeFaults are the chances, from 0 to 1, that the exchange misbehaves on a new order.
type ExchangeFaults struct {
	// Reject: the exchange rejects the order.
	Reject float64
	// TimeoutAfterAccept: the exchange records the order, then never answers the call.
	TimeoutAfterAccept float64
	// Drop: the exchange never sees the order and never answers.
	Drop float64
}

// ExchangeOrder is one order as the exchange recorded it.
type ExchangeOrder struct {
	exchange.SubmitRequest
	Status exchange.Status
	Reason string
}

// Exchange is a fake exchange order platform. It records each reference once.
type Exchange struct {
	faults ExchangeFaults
	stop   chan struct{}

	mu          sync.Mutex
	rnd         *rand.Rand
	orders      map[string]*ExchangeOrder
	submissions map[string]int
	stopped     bool
}

// NewExchange returns a fake exchange with faults drawn from seed.
func NewExchange(seed uint64, f ExchangeFaults) *Exchange {
	return &Exchange{
		faults:      f,
		stop:        make(chan struct{}),
		rnd:         rand.New(rand.NewPCG(seed, 0x45584348)),
		orders:      make(map[string]*ExchangeOrder),
		submissions: make(map[string]int),
	}
}

// SetFaults changes the faults for orders the exchange has not seen yet.
func (e *Exchange) SetFaults(f ExchangeFaults) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.faults = f
}

// Close releases every call the exchange is holding without an answer.
func (e *Exchange) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stopped {
		e.stopped = true
		close(e.stop)
	}
}

// ServeHTTP serves POST /orders and GET /orders/{ref}.
func (e *Exchange) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/orders":
		e.submit(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/orders/"):
		e.status(w, strings.TrimPrefix(r.URL.Path, "/orders/"))
	default:
		http.NotFound(w, r)
	}
}

func (e *Exchange) submit(w http.ResponseWriter, r *http.Request) {
	var req exchange.SubmitRequest
	if err := json.UnmarshalRead(r.Body, &req); err != nil || req.Ref == "" {
		http.Error(w, "bad order", http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	e.submissions[req.Ref]++
	if o, ok := e.orders[req.Ref]; ok {
		a := exchange.Answer{Ref: o.Ref, Status: o.Status, Reason: o.Reason}
		e.mu.Unlock()
		writeAnswer(w, a)
		return
	}
	roll, f := e.rnd.Float64(), e.faults
	switch {
	case roll < f.Drop:
		e.mu.Unlock()
		e.hang(r)
		return
	case roll < f.Drop+f.Reject:
		e.orders[req.Ref] = &ExchangeOrder{SubmitRequest: req, Status: exchange.Rejected, Reason: "scheme not open for purchase"}
	default:
		e.orders[req.Ref] = &ExchangeOrder{SubmitRequest: req, Status: exchange.Accepted}
	}
	o := e.orders[req.Ref]
	hangAfter := e.rnd.Float64() < f.TimeoutAfterAccept
	e.mu.Unlock()
	if hangAfter {
		e.hang(r)
		return
	}
	writeAnswer(w, exchange.Answer{Ref: o.Ref, Status: o.Status, Reason: o.Reason})
}

// hang holds a call open until the caller gives up or the exchange closes.
func (e *Exchange) hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-e.stop:
	case <-time.After(time.Minute):
	}
}

func (e *Exchange) status(w http.ResponseWriter, ref string) {
	e.mu.Lock()
	o, ok := e.orders[ref]
	var a exchange.Answer
	if ok {
		a = exchange.Answer{Ref: o.Ref, Status: o.Status, Reason: o.Reason}
	}
	e.mu.Unlock()
	if !ok {
		http.NotFound(w, nil)
		return
	}
	writeAnswer(w, a)
}

func writeAnswer(w http.ResponseWriter, a exchange.Answer) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, a)
}

// Orders returns every order the exchange recorded, sorted by reference.
func (e *Exchange) Orders() []ExchangeOrder {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ExchangeOrder, 0, len(e.orders))
	for _, o := range e.orders {
		out = append(out, *o)
	}
	slices.SortFunc(out, func(a, b ExchangeOrder) int { return strings.Compare(a.Ref, b.Ref) })
	return out
}

// Submissions returns how many calls named each reference, including retries.
func (e *Exchange) Submissions() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.submissions))
	for k, v := range e.submissions {
		out[k] = v
	}
	return out
}
