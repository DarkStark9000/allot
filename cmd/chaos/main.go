// Command chaos runs allot against misbehaving fakes and checks every guarantee.
//
//	go run ./cmd/chaos -runs 10 -orders 200 -seed 1
//
// It needs a PostgreSQL database in ALLOT_DATABASE_URL. Each run uses its own schema.
// The exit status is 1 if any run breaks a guarantee.
package main

import (
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/DarkStark9000/allot/internal/chaos"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chaos:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		runs    = flag.Int("runs", 5, "number of runs; run i uses seed+i")
		seed    = flag.Uint64("seed", 1, "seed of the first run")
		orders  = flag.Int("orders", 200, "orders per run")
		asJSON  = flag.Bool("json", false, "print reports as JSON lines")
		keep    = flag.Bool("keep", false, "keep each run's schema for inspection")
		verbose = flag.Bool("v", false, "log every request and relay retry")
	)
	flag.Parse()
	url := os.Getenv("ALLOT_DATABASE_URL")
	if url == "" {
		return fmt.Errorf("set ALLOT_DATABASE_URL, for example postgres://allot:allot@127.0.0.1:55432/allot?sslmode=disable")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logger := slog.New(slog.DiscardHandler)
	if *verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	var total chaos.Report
	total.Requests = map[string]int{}
	failed := 0
	start := time.Now()
	for i := range *runs {
		cfg := chaos.Defaults(url, *seed+uint64(i))
		cfg.Orders, cfg.Keep, cfg.Logger = *orders, *keep, logger
		rep, err := chaos.Run(ctx, cfg)
		if err != nil {
			return fmt.Errorf("seed %d: %w", cfg.Seed, err)
		}
		if len(rep.Violations) > 0 {
			failed++
		}
		if *asJSON {
			if err := json.MarshalWrite(os.Stdout, rep); err != nil {
				return err
			}
			fmt.Println()
		} else {
			printRun(os.Stdout, rep)
		}
		add(&total, rep)
	}
	if !*asJSON {
		printTotal(os.Stdout, total, *runs, failed, time.Since(start))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d runs broke a guarantee", failed, *runs)
	}
	return nil
}

func add(t *chaos.Report, r chaos.Report) {
	t.Orders += r.Orders
	t.Webhooks += r.Webhooks
	t.Submits += r.Submits
	t.AtExchange += r.AtExchange
	t.Refunds += r.Refunds
	t.Violations = append(t.Violations, r.Violations...)
	for k, v := range r.Requests {
		t.Requests[k] += v
	}
	if t.Final.States == nil {
		t.Final.States, t.Final.Findings = map[string]int{}, map[string]int{}
	}
	for k, v := range r.Final.States {
		t.Final.States[k] += v
	}
	for k, v := range r.Final.Findings {
		t.Final.Findings[k] += v
	}
}

func printRun(w io.Writer, r chaos.Report) {
	status := "ok"
	if len(r.Violations) > 0 {
		status = fmt.Sprintf("%d VIOLATIONS", len(r.Violations))
	}
	fmt.Fprintf(w, "seed %-4d %4d orders  %5d requests  %5d webhooks  %4d exchange calls  %3d refunds  %3d findings  %6s  %s\n",
		r.Seed, r.Orders, sum(r.Requests), r.Webhooks, r.Submits, r.Refunds, sum(r.Final.Findings), r.Took.Round(time.Millisecond), status)
	for _, v := range r.Violations {
		fmt.Fprintf(w, "    %s %s %s\n", v.Guarantee, v.OrderID, v.Detail)
	}
}

func printTotal(w io.Writer, t chaos.Report, runs, failed int, took time.Duration) {
	fmt.Fprintf(w, "\n%d runs, %d orders, %s\n", runs, t.Orders, took.Round(time.Second))
	fmt.Fprintf(w, "requests by status    %s\n", line(t.Requests))
	fmt.Fprintf(w, "final states          %s\n", line(t.Final.States))
	fmt.Fprintf(w, "findings              %s\n", line(t.Final.Findings))
	fmt.Fprintf(w, "webhooks delivered    %d (duplicates included)\n", t.Webhooks)
	fmt.Fprintf(w, "exchange calls        %d for %d exchange orders\n", t.Submits, t.AtExchange)
	fmt.Fprintf(w, "refunds               %d\n", t.Refunds)
	fmt.Fprintf(w, "guarantee violations  %d (runs failed: %d)\n", len(t.Violations), failed)
}

func line(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, "  ")
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
