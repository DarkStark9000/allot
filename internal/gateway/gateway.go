// Package gateway is an HTTP client for the payment gateway's refund API.
package gateway

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
)

// RefundRequest returns a payment to the investor. The gateway refunds a reference once.
type RefundRequest struct {
	Ref         string `json:"ref"`
	OrderID     string `json:"order_id"`
	PaymentID   string `json:"payment_id"`
	AmountPaise int64  `json:"amount_paise"`
}

// Client calls one gateway.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the gateway at base.
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: base, http: hc}
}

// Refund asks the gateway to return a payment. The result arrives later as a webhook.
func (c *Client) Refund(ctx context.Context, req RefundRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/refunds", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway: refund status %d", resp.StatusCode)
	}
	return nil
}
