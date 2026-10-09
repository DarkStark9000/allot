// Package exchange is an HTTP client for the exchange order platform.
//
// Every call names the order by its stable reference, so a retry or a status query can
// never create a second order at the exchange.
package exchange

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Status is the exchange's answer for an order.
type Status string

// The exchange decides each order once.
const (
	Accepted Status = "accepted"
	Rejected Status = "rejected"
)

var (
	// ErrTimeout means the exchange did not answer in time. It may or may not have the order.
	ErrTimeout = errors.New("exchange: no answer in time")
	// ErrNotFound means the exchange has no order with this reference.
	ErrNotFound = errors.New("exchange: order not found")
)

// SubmitRequest is one order sent to the exchange.
type SubmitRequest struct {
	Ref             string    `json:"ref"`
	SchemeCode      string    `json:"scheme_code"`
	Category        string    `json:"category"`
	AmountPaise     int64     `json:"amount_paise"`
	OrderedAt       time.Time `json:"ordered_at"`
	FundsRealisedAt time.Time `json:"funds_realised_at"`
}

// Answer is the exchange's decision on an order.
type Answer struct {
	Ref    string `json:"ref"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitzero"`
}

// Client calls one exchange. Callers set deadlines through the context.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the exchange at base, such as "http://127.0.0.1:9001".
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: base, http: hc}
}

// Submit sends an order. Sending the same reference again returns the first answer.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (Answer, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Answer{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/orders", bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	return c.do(r)
}

// Status asks for the decision on an order by its reference.
func (c *Client) Status(ctx context.Context, ref string) (Answer, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/orders/"+url.PathEscape(ref), nil)
	if err != nil {
		return Answer{}, err
	}
	return c.do(r)
}

func (c *Client) do(r *http.Request) (Answer, error) {
	resp, err := c.http.Do(r)
	if err != nil {
		if isTimeout(err) {
			return Answer{}, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		return Answer{}, fmt.Errorf("exchange: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Answer{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return Answer{}, fmt.Errorf("exchange: status %d", resp.StatusCode)
	}
	var a Answer
	if err := json.UnmarshalRead(resp.Body, &a); err != nil {
		if isTimeout(err) {
			return Answer{}, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		return Answer{}, fmt.Errorf("exchange: decode: %w", err)
	}
	if a.Status != Accepted && a.Status != Rejected {
		return Answer{}, fmt.Errorf("exchange: unknown status %q", a.Status)
	}
	return a, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
