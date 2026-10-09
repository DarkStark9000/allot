package httpapi_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/DarkStark9000/allot/internal/httpapi"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/testdb"
)

var secret = []byte("test-secret")

func newServer(t *testing.T) (*httptest.Server, *store.DB) {
	t.Helper()
	db := testdb.New(t)
	srv := httptest.NewServer(httpapi.New(httpapi.Config{DB: db, WebhookSecret: secret, KeyLease: 30 * time.Second}).Handler())
	t.Cleanup(srv.Close)
	return srv, db
}

type reply struct {
	code   int
	body   []byte
	header http.Header
}

func post(t *testing.T, url, key, body string) reply {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/orders", bytes.NewBufferString(body))
	req.Header.Set("X-User-ID", "user_1")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, b, resp.Header}
}

const validBody = `{"scheme_code":"INF000K01ABC","category":"other","amount_paise":500000}`

func orderID(t *testing.T, r reply) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.body, &v); err != nil || v.ID == "" {
		t.Fatalf("no order id in %s", r.body)
	}
	return v.ID
}

func countOrders(t *testing.T, db *store.DB) int {
	t.Helper()
	rows, err := db.Query(t.Context(), `SELECT count(*) FROM orders`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// F1: the app retries after a timeout and gets the stored response; one order exists.
func TestCreateOrder_ReplaysStoredResponse(t *testing.T) {
	srv, db := newServer(t)
	first := post(t, srv.URL, "key-1", validBody)
	if first.code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.code, first.body)
	}
	second := post(t, srv.URL, "key-1", validBody)
	if second.code != http.StatusCreated || !bytes.Equal(first.body, second.body) {
		t.Fatalf("retry: %d %s, want the stored 201", second.code, second.body)
	}
	if second.header.Get("Idempotent-Replayed") != "true" {
		t.Error("retry is not marked as replayed")
	}
	if n := countOrders(t, db); n != 1 {
		t.Errorf("%d orders, want 1", n)
	}
}

// F2: the same key with a different body is refused.
func TestIdempotency_KeyReuseDifferentBody(t *testing.T) {
	srv, _ := newServer(t)
	post(t, srv.URL, "key-1", validBody)
	r := post(t, srv.URL, "key-1", `{"scheme_code":"INF000K01ABC","category":"other","amount_paise":999}`)
	if r.code != http.StatusUnprocessableEntity || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("got %d %s, want 422 problem", r.code, r.body)
	}
}

// F3: requests that race with one key create one order. Each gets 201 with that order,
// or 409 while the first is still running. TestIdempotency_InFlightIs409 pins the 409 path.
func TestIdempotency_ConcurrentSameKey(t *testing.T) {
	srv, db := newServer(t)
	var wg sync.WaitGroup
	replies := make([]reply, 8)
	for i := range replies {
		wg.Go(func() { replies[i] = post(t, srv.URL, "race", validBody) })
	}
	wg.Wait()
	ids := map[string]bool{}
	for _, r := range replies {
		switch r.code {
		case http.StatusCreated:
			ids[orderID(t, r)] = true
		case http.StatusConflict:
		default:
			t.Errorf("racing request got %d %s", r.code, r.body)
		}
	}
	if len(ids) != 1 {
		t.Errorf("racing requests saw %d order IDs, want 1", len(ids))
	}
	if n := countOrders(t, db); n != 1 {
		t.Errorf("%d orders, want 1", n)
	}
}

// TestIdempotency_InFlightIs409 holds a key as if a first request were mid-way, then retries.
func TestIdempotency_InFlightIs409(t *testing.T) {
	srv, db := newServer(t)
	first := post(t, srv.URL, "k", validBody)
	if first.code != http.StatusCreated {
		t.Fatal(first.code)
	}
	// Re-open the completed key as in flight, as if a second request were mid-way.
	rows, err := db.Query(t.Context(), `UPDATE idempotency_keys SET completed = false, locked_until = now() + interval '1 minute'`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	r := post(t, srv.URL, "k", validBody)
	if r.code != http.StatusConflict || r.header.Get("Retry-After") == "" {
		t.Fatalf("got %d, want 409 with Retry-After", r.code)
	}
}

func TestCreateOrder_Validation(t *testing.T) {
	srv, db := newServer(t)
	if r := post(t, srv.URL, "", validBody); r.code != http.StatusBadRequest {
		t.Errorf("missing key: %d, want 400", r.code)
	}
	bad := post(t, srv.URL, "bad", `{"scheme_code":"X","category":"other","amount_paise":0}`)
	if bad.code != http.StatusUnprocessableEntity {
		t.Errorf("zero amount: %d, want 422", bad.code)
	}
	again := post(t, srv.URL, "bad", `{"scheme_code":"X","category":"other","amount_paise":0}`)
	if again.code != bad.code || !bytes.Equal(again.body, bad.body) {
		t.Error("a retried invalid request did not get the stored answer")
	}
	if r := post(t, srv.URL, "unknown-field", `{"scheme_code":"X","category":"other","amount_paise":1,"extra":1}`); r.code != http.StatusBadRequest {
		t.Errorf("unknown field: %d, want 400", r.code)
	}
	if n := countOrders(t, db); n != 0 {
		t.Errorf("%d orders, want 0", n)
	}
}

func sendWebhook(t *testing.T, url string, ev httpapi.GatewayEvent, sign bool) reply {
	t.Helper()
	body, _ := json.Marshal(ev)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/webhooks/payments", bytes.NewReader(body))
	if sign {
		req.Header.Set(httpapi.SignatureHeader, httpapi.Sign(secret, body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, b, resp.Header}
}

func getOrder(t *testing.T, db *store.DB, id string) order.Order {
	t.Helper()
	o, err := db.Order(t.Context(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// F4: the gateway delivers the success webhook three times; it applies once.
func TestInbox_DuplicateDeliveries(t *testing.T) {
	srv, db := newServer(t)
	id := orderID(t, post(t, srv.URL, "k", validBody))
	ev := httpapi.GatewayEvent{ID: "evt_1", Type: "payment.succeeded", OrderID: id, PaymentID: "pay_1", OccurredAt: time.Now()}
	want := []string{`{"outcome":"applied"}`, `{"outcome":"duplicate"}`, `{"outcome":"duplicate"}`}
	for i, w := range want {
		r := sendWebhook(t, srv.URL, ev, true)
		if r.code != http.StatusOK || string(r.body) != w {
			t.Errorf("delivery %d: %d %s, want 200 %s", i+1, r.code, r.body, w)
		}
	}
	o := getOrder(t, db, id)
	if o.State != order.Submitted || o.Version != 1 {
		t.Errorf("order = %s v%d, want submitted v1", o.State, o.Version)
	}
	if n, _ := db.UnsentMessages(t.Context()); n != 1 {
		t.Errorf("%d outbox messages, want 1", n)
	}
}

// F5: a stale failure webhook after success is recorded and ignored.
func TestPayment_StaleFailureAfterSuccess(t *testing.T) {
	srv, db := newServer(t)
	id := orderID(t, post(t, srv.URL, "k", validBody))
	sendWebhook(t, srv.URL, httpapi.GatewayEvent{ID: "evt_ok", Type: "payment.succeeded", OrderID: id, PaymentID: "pay_1", OccurredAt: time.Now()}, true)
	r := sendWebhook(t, srv.URL, httpapi.GatewayEvent{ID: "evt_fail", Type: "payment.failed", OrderID: id, PaymentID: "pay_1", Reason: "timeout"}, true)
	if string(r.body) != `{"outcome":"stale"}` {
		t.Fatalf("stale failure: %s", r.body)
	}
	if o := getOrder(t, db, id); o.State != order.Submitted {
		t.Errorf("state = %s, want submitted", o.State)
	}
}

func TestWebhook_RejectsBadSignature(t *testing.T) {
	srv, _ := newServer(t)
	r := sendWebhook(t, srv.URL, httpapi.GatewayEvent{ID: "evt", Type: "payment.succeeded", OrderID: uuid.NewV7().String()}, false)
	if r.code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: %d, want 401", r.code)
	}
}

func TestWebhook_SecondPaymentOpensFinding(t *testing.T) {
	srv, db := newServer(t)
	id := orderID(t, post(t, srv.URL, "k", validBody))
	sendWebhook(t, srv.URL, httpapi.GatewayEvent{ID: "e1", Type: "payment.succeeded", OrderID: id, PaymentID: "pay_1", OccurredAt: time.Now()}, true)
	r := sendWebhook(t, srv.URL, httpapi.GatewayEvent{ID: "e2", Type: "payment.succeeded", OrderID: id, PaymentID: "pay_2", OccurredAt: time.Now()}, true)
	if string(r.body) != `{"outcome":"conflict"}` {
		t.Fatalf("second payment: %s", r.body)
	}
	f, err := db.Findings(t.Context())
	if err != nil || len(f) != 1 || f[0].Kind != "conflicting_event" {
		t.Errorf("findings = %+v, %v", f, err)
	}
}

// F13: with the database gone, the API answers 503 with Retry-After and writes nothing.
func TestAPI_DatabaseDown(t *testing.T) {
	db := testdb.New(t)
	srv := httptest.NewServer(httpapi.New(httpapi.Config{DB: db, WebhookSecret: secret, KeyLease: time.Minute}).Handler())
	defer srv.Close()
	db.Close()
	r := post(t, srv.URL, "k", validBody)
	if r.code != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" {
		t.Fatalf("got %d, want 503 with Retry-After", r.code)
	}
}

func TestGetOrder(t *testing.T) {
	srv, _ := newServer(t)
	id := orderID(t, post(t, srv.URL, "k", validBody))
	get := func(user string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/orders/"+id, nil)
		req.Header.Set("X-User-ID", user)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("user_1"); c != http.StatusOK {
		t.Errorf("owner: %d", c)
	}
	if c := get("someone_else"); c != http.StatusNotFound {
		t.Errorf("other user: %d, want 404", c)
	}
}
