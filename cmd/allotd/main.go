// Command allotd runs the allot API, the outbox relay, and the payment expirer.
//
// Configuration comes from the environment:
//
//	ALLOT_DATABASE_URL     PostgreSQL connection string (required)
//	ALLOT_WEBHOOK_SECRET   HMAC secret shared with the payment gateway (required)
//	ALLOT_EXCHANGE_URL     base URL of the exchange order platform (required)
//	ALLOT_GATEWAY_URL      base URL of the payment gateway (required)
//	ALLOT_ADDR             listen address, default 127.0.0.1:8080
//	ALLOT_PAYMENT_WINDOW   how long an order waits for payment, default 15m
//	ALLOT_RELAYS           relays to run in this process, default 2
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/gateway"
	"github.com/DarkStark9000/allot/internal/httpapi"
	"github.com/DarkStark9000/allot/internal/outbox"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/sweep"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

type config struct {
	databaseURL, secret, exchangeURL, gatewayURL, addr string
	paymentWindow                                      time.Duration
	relays                                             int
}

func load() (config, error) {
	c := config{
		databaseURL:   os.Getenv("ALLOT_DATABASE_URL"),
		secret:        os.Getenv("ALLOT_WEBHOOK_SECRET"),
		exchangeURL:   os.Getenv("ALLOT_EXCHANGE_URL"),
		gatewayURL:    os.Getenv("ALLOT_GATEWAY_URL"),
		addr:          orDefault(os.Getenv("ALLOT_ADDR"), "127.0.0.1:8080"),
		paymentWindow: 15 * time.Minute,
		relays:        2,
	}
	for name, v := range map[string]string{
		"ALLOT_DATABASE_URL": c.databaseURL, "ALLOT_WEBHOOK_SECRET": c.secret,
		"ALLOT_EXCHANGE_URL": c.exchangeURL, "ALLOT_GATEWAY_URL": c.gatewayURL,
	} {
		if v == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	if v := os.Getenv("ALLOT_PAYMENT_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("ALLOT_PAYMENT_WINDOW: %q is not a positive duration", v)
		}
		c.paymentWindow = d
	}
	if v := os.Getenv("ALLOT_RELAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("ALLOT_RELAYS: %q is not a count", v)
		}
		c.relays = n
	}
	return c, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func run(log *slog.Logger) error {
	cfg, err := load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           httpapi.New(httpapi.Config{DB: db, WebhookSecret: []byte(cfg.secret), KeyLease: 30 * time.Second, Logger: log}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("listening", "addr", cfg.addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	})
	ex := exchange.NewClient(cfg.exchangeURL, &http.Client{})
	gw := gateway.NewClient(cfg.gatewayURL, &http.Client{})
	for i := range cfg.relays {
		r := outbox.New(outbox.Config{DB: db, Exchange: ex, Gateway: gw, Logger: log, Name: fmt.Sprintf("relay-%d", i+1)})
		g.Go(func() error { return r.Run(ctx) })
	}
	expirer := sweep.Expirer{DB: db, Window: cfg.paymentWindow, Every: 10 * time.Second, Logger: log}
	g.Go(func() error { return expirer.Run(ctx) })
	return g.Wait()
}
