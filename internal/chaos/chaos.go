// Package chaos runs allot end to end against misbehaving fakes and checks every guarantee.
//
// One run places orders through the HTTP API with retried, racing, and reused keys; pays
// them through a gateway that duplicates, delays, and reorders webhooks; submits them to an
// exchange that rejects, drops, and goes silent after accepting; restarts relays mid-flight;
// applies a registrar file with missing and wrong rows; and then checks G1 to G6.
// The same seed draws the same faults.
package chaos

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/DarkStark9000/allot/internal/exchange"
	"github.com/DarkStark9000/allot/internal/fake"
	"github.com/DarkStark9000/allot/internal/gateway"
	"github.com/DarkStark9000/allot/internal/httpapi"
	"github.com/DarkStark9000/allot/internal/invariant"
	"github.com/DarkStark9000/allot/internal/navdate"
	"github.com/DarkStark9000/allot/internal/order"
	"github.com/DarkStark9000/allot/internal/outbox"
	"github.com/DarkStark9000/allot/internal/recon"
	"github.com/DarkStark9000/allot/internal/store"
	"github.com/DarkStark9000/allot/internal/sweep"
)

// Config sets the size and the faults of a run.
type Config struct {
	DatabaseURL string
	Seed        uint64
	Orders      int
	Concurrency int
	Relays      int
	// RelayRestarts is how many times a running relay is killed and replaced.
	RelayRestarts int
	PaymentWindow time.Duration
	Exchange      fake.ExchangeFaults
	Gateway       fake.GatewayFaults
	RTA           fake.RTAFaults
	// Keep leaves the run's schema in the database for inspection.
	Keep   bool
	Logger *slog.Logger
}

// Defaults returns a run with every fault turned on.
func Defaults(url string, seed uint64) Config {
	return Config{
		DatabaseURL:   url,
		Seed:          seed,
		Orders:        200,
		Concurrency:   16,
		Relays:        3,
		RelayRestarts: 6,
		PaymentWindow: 1500 * time.Millisecond,
		Exchange:      fake.ExchangeFaults{Reject: 0.08, TimeoutAfterAccept: 0.1, Drop: 0.05},
		Gateway: fake.GatewayFaults{
			Decline: 0.05, MaxCopies: 4, MaxDelay: 150 * time.Millisecond,
			StaleFailure: 0.1, Late: 0.04, LateBy: 2500 * time.Millisecond,
		},
		RTA: fake.RTAFaults{DropRow: 0.03, WrongUnits: 0.02},
	}
}

// Report is what one run did and found.
type Report struct {
	Seed       uint64                `json:"seed"`
	Orders     int                   `json:"orders"`
	Requests   map[string]int        `json:"requests"`
	Webhooks   int                   `json:"webhooks_delivered"`
	Submits    int                   `json:"exchange_calls"`
	AtExchange int                   `json:"exchange_orders"`
	Refunds    int                   `json:"refunds"`
	RTA        recon.Report          `json:"rta"`
	Final      invariant.Summary     `json:"final"`
	Violations []invariant.Violation `json:"violations"`
	Took       time.Duration         `json:"took"`
}

const secret = "chaos-webhook-secret"

// Run executes one run and checks it.
func Run(ctx context.Context, cfg Config) (Report, error) {
	start := time.Now()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rep := Report{Seed: cfg.Seed, Requests: map[string]int{}}
	schema := fmt.Sprintf("chaos_%d_%d", cfg.Seed, time.Now().UnixNano()%1_000_000)
	url, err := store.CreateSchema(ctx, cfg.DatabaseURL, schema)
	if err != nil {
		return rep, err
	}
	if !cfg.Keep {
		defer func() { _ = store.DropSchema(context.WithoutCancel(ctx), cfg.DatabaseURL, schema) }()
	}
	db, err := store.Open(ctx, url)
	if err != nil {
		return rep, err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return rep, err
	}

	api := httptest.NewServer(httpapi.New(httpapi.Config{DB: db, WebhookSecret: []byte(secret), KeyLease: 2 * time.Second, Logger: log}).Handler())
	defer api.Close()
	ex := fake.NewExchange(cfg.Seed, cfg.Exchange)
	exSrv := httptest.NewServer(ex)
	defer exSrv.Close()
	defer ex.Close()
	gw := fake.NewGateway(cfg.Seed, api.URL+"/v1/webhooks/payments", []byte(secret), cfg.Gateway)
	gwSrv := httptest.NewServer(gw)
	defer gwSrv.Close()
	defer gw.Close()

	work, stop := context.WithCancel(ctx)
	defer stop()
	var bg sync.WaitGroup
	relays := newRelayPool(db, exSrv.URL, gwSrv.URL, log)
	for range cfg.Relays {
		relays.start(work, &bg)
	}
	expirer := sweep.Expirer{DB: db, Window: cfg.PaymentWindow, Every: 100 * time.Millisecond, Logger: log}
	bg.Go(func() { _ = expirer.Run(work) })

	rnd := rand.New(rand.NewPCG(cfg.Seed, 0x43484153))
	bg.Go(func() { relays.restartLoop(work, &bg, cfg.RelayRestarts, rand.New(rand.NewPCG(cfg.Seed, 7))) })

	ids, err := placeOrders(ctx, cfg, api.URL, gw, rnd, rep.Requests)
	if err != nil {
		return rep, err
	}
	rep.Orders = len(idCounts(ids))

	if err := quiesce(ctx, db, gw, cfg.PaymentWindow+cfg.Gateway.LateBy+5*time.Second); err != nil {
		return rep, err
	}
	file, err := fake.AllotmentFile(ex.Orders(), navdate.Default(), navdate.NewCalendar(), cfg.Seed, cfg.RTA)
	if err != nil {
		return rep, err
	}
	rows, err := recon.ParseFile(bytes.NewReader(file))
	if err != nil {
		return rep, err
	}
	rc := recon.Reconciler{DB: db, Rules: navdate.Default(), Calendar: navdate.NewCalendar()}
	if rep.RTA, err = rc.Run(ctx, "eod-1", rows, time.Now()); err != nil {
		return rep, err
	}
	if err := quiesce(ctx, db, gw, 5*time.Second); err != nil {
		return rep, err
	}
	stop()
	bg.Wait()

	ext := invariant.External{Exchange: map[string]exchange.Status{}, Refunds: map[string]bool{}, RefundRef: outbox.RefundRef}
	for _, o := range ex.Orders() {
		ext.Exchange[o.Ref] = o.Status
	}
	for ref := range gw.Refunds() {
		ext.Refunds[ref] = true
	}
	for _, n := range ex.Submissions() {
		rep.Submits += n
	}
	rep.AtExchange, rep.Refunds, rep.Webhooks = len(ext.Exchange), len(ext.Refunds), gw.Delivered()
	rep.Violations, rep.Final, err = invariant.Check(ctx, db, ext, navdate.Default(), navdate.NewCalendar())
	for key, n := range idCounts(ids) {
		if n > 1 {
			rep.Violations = append(rep.Violations, invariant.Violation{Guarantee: "G1", Detail: fmt.Sprintf("key %s answered with %d order IDs", key, n)})
		}
	}
	rep.Took = time.Since(start)
	return rep, err
}

// relayPool runs relays and kills them on demand, as a crash would.
type relayPool struct {
	db           *store.DB
	exURL, gwURL string
	log          *slog.Logger
	mu           sync.Mutex
	cancels      []context.CancelFunc
	started      int
}

func newRelayPool(db *store.DB, exURL, gwURL string, log *slog.Logger) *relayPool {
	return &relayPool{db: db, exURL: exURL, gwURL: gwURL, log: log}
}

func (p *relayPool) start(ctx context.Context, bg *sync.WaitGroup) {
	p.mu.Lock()
	p.started++
	name := fmt.Sprintf("relay-%d", p.started)
	rctx, cancel := context.WithCancel(ctx)
	p.cancels = append(p.cancels, cancel)
	p.mu.Unlock()
	r := outbox.New(outbox.Config{
		DB:          p.db,
		Exchange:    exchange.NewClient(p.exURL, nil),
		Gateway:     gateway.NewClient(p.gwURL, nil),
		Logger:      p.log,
		Name:        name,
		Batch:       8,
		Lease:       time.Second,
		Poll:        20 * time.Millisecond,
		CallTimeout: 250 * time.Millisecond,
		BackoffBase: 20 * time.Millisecond,
		BackoffMax:  300 * time.Millisecond,
	})
	bg.Go(func() { _ = r.Run(rctx) })
}

// restartLoop kills a random relay and starts a new one, n times.
func (p *relayPool) restartLoop(ctx context.Context, bg *sync.WaitGroup, n int, rnd *rand.Rand) {
	for range n {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(100+rnd.IntN(400)) * time.Millisecond):
		}
		p.mu.Lock()
		i := rnd.IntN(len(p.cancels))
		p.cancels[i]()
		p.cancels = append(p.cancels[:i], p.cancels[i+1:]...)
		p.mu.Unlock()
		p.start(ctx, bg)
	}
}

type created struct {
	key string
	id  string
}

var schemes = []struct {
	code     string
	category navdate.Category
}{
	{"INF000K01EQ1", navdate.Other},
	{"INF000K01DB2", navdate.Other},
	{"INF000K01LQ3", navdate.Liquid},
	{"INF000K01ON4", navdate.Overnight},
}

// placeOrders acts as the investor app: it posts orders with retries, races, and reused
// keys, then pays most of them. It returns every order ID each key was answered with.
func placeOrders(ctx context.Context, cfg Config, base string, gw *fake.Gateway, rnd *rand.Rand, counts map[string]int) ([]created, error) {
	type plan struct {
		user, key, body, otherBody string
		racers, retries            int
		reuse, pay                 bool
	}
	plans := make([]plan, cfg.Orders)
	for i := range plans {
		s := schemes[rnd.IntN(len(schemes))]
		amount := int64(100_00 + rnd.IntN(10_000)*100)
		plans[i] = plan{
			user:      fmt.Sprintf("user_%d", i%37),
			key:       fmt.Sprintf("order-%d", i),
			body:      fmt.Sprintf(`{"scheme_code":%q,"category":%q,"amount_paise":%d}`, s.code, s.category, amount),
			otherBody: fmt.Sprintf(`{"scheme_code":%q,"category":%q,"amount_paise":%d}`, s.code, s.category, amount+1),
			racers:    rnd.IntN(3),
			retries:   rnd.IntN(3),
			reuse:     rnd.Float64() < 0.05,
			pay:       rnd.Float64() < 0.95,
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var (
		mu  sync.Mutex
		out []created
		sem = make(chan struct{}, cfg.Concurrency)
		wg  sync.WaitGroup
	)
	record := func(code int, key, id string) {
		mu.Lock()
		defer mu.Unlock()
		counts[fmt.Sprint(code)]++
		if id != "" {
			out = append(out, created{key: key, id: id})
		}
	}
	var firstErr error
	var errOnce sync.Once
	for _, p := range plans {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			var inner sync.WaitGroup
			var id string
			var idMu sync.Mutex
			send := func(body string) {
				code, got, err := postOrder(ctx, client, base, p.user, p.key, body)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
				record(code, p.user+"/"+p.key, got)
				if got != "" {
					idMu.Lock()
					id = got
					idMu.Unlock()
				}
			}
			for range 1 + p.racers {
				inner.Go(func() { send(p.body) })
			}
			inner.Wait()
			for range p.retries {
				send(p.body)
			}
			if p.reuse {
				send(p.otherBody) // must get 422
			}
			if p.pay && id != "" {
				gw.Pay(id)
			}
		})
	}
	wg.Wait()
	return out, firstErr
}

func postOrder(ctx context.Context, c *http.Client, base, user, key, body string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/orders", bytes.NewBufferString(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("X-User-ID", user)
	req.Header.Set("Idempotency-Key", key)
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return resp.StatusCode, "", nil
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, "", err
	}
	return resp.StatusCode, v.ID, nil
}

func idCounts(cs []created) map[string]int {
	ids := map[string]map[string]bool{}
	for _, c := range cs {
		if ids[c.key] == nil {
			ids[c.key] = map[string]bool{}
		}
		ids[c.key][c.id] = true
	}
	out := map[string]int{}
	for k, v := range ids {
		out[k] = len(v)
	}
	return out
}

// quiesce waits until nothing is moving: no webhook in flight, no unsent message, and no
// order waiting on payment or the exchange.
func quiesce(ctx context.Context, db *store.DB, gw *fake.Gateway, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	calm := 0
	for time.Now().Before(deadline) {
		unsent, err := db.UnsentMessages(ctx)
		if err != nil {
			return err
		}
		moving, err := db.OrdersIn(ctx, order.PaymentPending, order.Submitted, order.SubmitUncertain, order.RefundPending)
		if err != nil {
			return err
		}
		if unsent == 0 && len(moving) == 0 && gw.Idle() {
			if calm++; calm == 3 {
				return nil
			}
		} else {
			calm = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("chaos: the system did not settle in time")
}
