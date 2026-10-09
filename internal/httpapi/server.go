// Package httpapi serves the allot HTTP API: orders, payment webhooks, and health.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"time"

	"github.com/DarkStark9000/allot/internal/store"
)

// Config holds what the server needs. Every field is required except Now and Logger.
type Config struct {
	DB            *store.DB
	WebhookSecret []byte
	// KeyLease is how long a request may hold an idempotency key before another may take it over.
	KeyLease time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
}

// Server handles HTTP requests.
type Server struct {
	db     *store.DB
	secret []byte
	lease  time.Duration
	log    *slog.Logger
	now    func() time.Time
}

// New returns a server for cfg.
func New(cfg Config) *Server {
	s := &Server{db: cfg.DB, secret: cfg.WebhookSecret, lease: cfg.KeyLease, log: cfg.Logger, now: cfg.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orders", s.createOrder)
	mux.HandleFunc("GET /v1/orders/{id}", s.getOrder)
	mux.HandleFunc("POST /v1/webhooks/payments", s.paymentWebhook)
	mux.HandleFunc("GET /healthz", s.health)
	return s.logRequests(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		s.problem(w, http.StatusServiceUnavailable, "unavailable", "Database unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// problemBase is where each problem type is documented.
const problemBase = "https://github.com/DarkStark9000/allot#problem-"

// problemBody returns an RFC 9457 problem document.
func problemBody(status int, kind, title, detail string) []byte {
	b, err := json.Marshal(struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail,omitempty"`
	}{problemBase + kind, title, status, detail})
	if err != nil {
		panic(err) // a struct of strings and an int always encodes
	}
	return b
}

func (s *Server) problem(w http.ResponseWriter, status int, kind, title, detail string) {
	write(w, status, "application/problem+json", problemBody(status, kind, title, detail))
}

func write(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body) // the client may be gone; nothing useful to do about it
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
		return
	}
	write(w, status, "application/json", b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("request_id", id),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("took", time.Since(start)))
	})
}
