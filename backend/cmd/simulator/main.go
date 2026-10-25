// Command simulator is the data producer: it plays the role of the meter
// head-end system and the call centre. It streams 15-minute readings for
// every meter (first catching up from SIM_BACKFILL_HOURS ago as fast as the
// API accepts them, then in real time), and keeps injecting random faults:
// affected meters send "last gasp" messages and stop reporting, a few
// customers call (some twice), and when the fault clears the meters send
// "power restored". Everything random is derived from SIM_SEED.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/sim"
)

type config struct {
	API         string
	Token       string
	Seed        uint64
	Backfill    time.Duration
	Batch       int
	OutageEvery time.Duration
	OutageMin   time.Duration
	OutageMax   time.Duration
	Anchor      time.Time
}

func loadConfig() config {
	c := config{API: env("API_URL", "http://localhost:8080"), Token: os.Getenv("INGEST_TOKEN"), Batch: 20000,
		Backfill: 48 * time.Hour, OutageEvery: 3 * time.Minute, OutageMin: 4 * time.Minute, OutageMax: 15 * time.Minute}
	c.Seed, _ = strconv.ParseUint(env("SIM_SEED", "42"), 10, 64)
	if d, err := time.ParseDuration(os.Getenv("SIM_BACKFILL")); err == nil {
		c.Backfill = d
	}
	if d, err := time.ParseDuration(os.Getenv("SIM_OUTAGE_EVERY")); err == nil {
		c.OutageEvery = d
	}
	if n, err := strconv.Atoi(os.Getenv("SIM_BATCH")); err == nil && n > 0 {
		c.Batch = n
	}
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		loc = time.FixedZone("TRT", 3*3600)
	}
	n := time.Now().In(loc)
	c.Anchor = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	if t, err := time.ParseInLocation("2006-01-02", os.Getenv("SIM_ANCHOR"), loc); err == nil {
		c.Anchor = t
	}
	return c
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type client struct {
	cfg  config
	http *http.Client
	log  *slog.Logger
}

func (c *client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.API+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ingest-Token", c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, truncate(string(b), 300))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// retry keeps trying while the API is down (chaos test: the simulator must
// survive an API or database restart and resume).
func (c *client) retry(ctx context.Context, what string, fn func() error) error {
	backoff := time.Second
	for {
		err := fn()
		if err == nil || ctx.Err() != nil {
			return err
		}
		c.log.Warn("request failed, retrying", "what", what, "err", err.Error(), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *client) meters(ctx context.Context) ([]sim.Meter, error) {
	var all []sim.Meter
	cursor := ""
	for {
		var page struct {
			Items      []sim.Meter `json:"items"`
			NextCursor *string     `json:"next_cursor"`
		}
		path := "/api/v1/ingest/meters?limit=20000"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		if err := c.do(ctx, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			return all, nil
		}
		cursor = *page.NextCursor
	}
}

type outageSignal struct {
	Source       string    `json:"source"`
	ExternalID   string    `json:"external_id"`
	SubscriberID int64     `json:"subscriber_id"`
	MeterID      int64     `json:"meter_id,omitempty"`
	ReceivedAt   time.Time `json:"received_at"`
	Note         string    `json:"note,omitempty"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "simulator")
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &client{cfg: cfg, http: &http.Client{Timeout: 60 * time.Second}, log: log}
	log.Info("simulator starting", "api", cfg.API, "seed", cfg.Seed, "backfill", cfg.Backfill.String(), "outage_every", cfg.OutageEvery.String())

	if err := c.retry(ctx, "health", func() error { return c.do(ctx, "GET", "/health", nil, nil) }); err != nil {
		return
	}
	var meters []sim.Meter
	if err := c.retry(ctx, "meters", func() error {
		var err error
		meters, err = c.meters(ctx)
		return err
	}); err != nil {
		return
	}
	// Akış, seed'in yazdığı geçmişin bittiği yerden (bugünden 2 gün önceki yerel
	// gece yarısı) başlar; böylece geçmiş ile canlı veri arasında boşluk kalmaz.
	// SIM_BACKFILL verilmişse o kadar geriden başlanır.
	start := cfg.Anchor.AddDate(0, 0, -2)
	if os.Getenv("SIM_BACKFILL") != "" {
		start = time.Now().Add(-cfg.Backfill).Truncate(15 * time.Minute)
	}
	st := sim.NewState(sim.Generator{Seed: cfg.Seed}, meters, start)
	log.Info("meters loaded", "count", len(meters), "stream_start", start.Format(time.RFC3339))

	var mu sync.Mutex // State, Dead haritasını kesinti goroutine'i ile paylaşır
	go outages(ctx, c, st, &mu, cfg, log)

	// Akış: geçmiş slotları hızla, sonra gerçek zamanlı.
	earliest := start
	for _, m := range meters {
		if n := st.Next[m.ID]; n.Before(earliest) || earliest.IsZero() {
			earliest = n
		}
	}
	t := earliest
	sent := 0
	var batch []sim.Reading
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b := batch
		batch = nil
		_ = c.retry(ctx, "readings", func() error {
			var res map[string]any
			err := c.do(ctx, "POST", "/api/v1/readings/batch", map[string]any{"readings": b}, &res)
			if err == nil {
				log.Info("batch sent", "readings", len(b), "inserted", res["inserted"], "duplicates", res["duplicates"],
					"corrections", res["corrections"], "rejected", res["rejected"])
			}
			return err
		})
		sent += len(b)
	}
	for ctx.Err() == nil {
		if t.After(time.Now()) {
			flush()
			wait := time.Until(t)
			log.Info("caught up, waiting for next slot", "next", t.Format(time.RFC3339), "sent_total", sent)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		mu.Lock()
		rs := st.Slot(t)
		mu.Unlock()
		batch = append(batch, rs...)
		if len(batch) >= cfg.Batch {
			flush()
		}
		t = t.Add(15 * time.Minute)
	}
	flush()
	log.Info("simulator stopped", "sent_total", sent)
}

// outages injects a fault every OutageEvery: a pole (70%), a feeder (25%)
// or a whole transformer (5%) loses supply for OutageMin–OutageMax.
func outages(ctx context.Context, c *client, st *sim.State, mu *sync.Mutex, cfg config, log *slog.Logger) {
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x07a6e))
	var poles, feeders, trs []int64
	seen := map[int64]bool{}
	for _, m := range st.Meters {
		if m.Kind != "subscriber" || len(m.Path) < 3 {
			continue
		}
		pole, feeder, tr := m.Path[0], m.Path[len(m.Path)-2], m.Path[len(m.Path)-1]
		for _, x := range []struct {
			id  int64
			dst *[]int64
		}{{pole, &poles}, {feeder, &feeders}, {tr, &trs}} {
			if !seen[x.id] {
				seen[x.id] = true
				*x.dst = append(*x.dst, x.id)
			}
		}
	}
	if len(poles) == 0 {
		return
	}
	event := 0
	tick := time.NewTicker(cfg.OutageEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		event++
		var node int64
		kind := "pole"
		switch x := rng.IntN(100); {
		case x < 5:
			node, kind = trs[rng.IntN(len(trs))], "transformer"
		case x < 30:
			node, kind = feeders[rng.IntN(len(feeders))], "feeder"
		default:
			node = poles[rng.IntN(len(poles))]
		}
		mu.Lock()
		affected := st.Under(node)
		already := false
		for _, m := range affected {
			if st.Dead[m.ID] {
				already = true
			}
		}
		if already || len(affected) == 0 {
			mu.Unlock()
			continue
		}
		for _, m := range affected {
			st.Dead[m.ID] = true
		}
		mu.Unlock()
		now := time.Now()
		var sigs []outageSignal
		for _, m := range affected {
			if rng.IntN(100) < 70 { // son nefes her sayaçtan ulaşmaz
				sigs = append(sigs, outageSignal{Source: "last_gasp", ExternalID: fmt.Sprintf("lg-%d-%d-%d", cfg.Seed, event, m.ID),
					SubscriberID: *m.SubscriberID, MeterID: m.ID, ReceivedAt: now.Add(time.Duration(rng.IntN(20)) * time.Second)})
			}
		}
		calls := 1 + rng.IntN(3)
		for i := 0; i < calls && i < len(affected); i++ {
			m := affected[rng.IntN(len(affected))]
			s := outageSignal{Source: "call", ExternalID: fmt.Sprintf("cc-%d-%d-%d", cfg.Seed, event, i), SubscriberID: *m.SubscriberID,
				ReceivedAt: now.Add(time.Duration(30+rng.IntN(240)) * time.Second), Note: "abone: elektrik yok"}
			sigs = append(sigs, s)
			if rng.IntN(3) == 0 { // aynı abone tekrar arar
				s.ExternalID += "-again"
				s.ReceivedAt = s.ReceivedAt.Add(2 * time.Minute)
				sigs = append(sigs, s)
			}
		}
		dur := cfg.OutageMin + time.Duration(rng.Int64N(int64(cfg.OutageMax-cfg.OutageMin)))
		log.Info("fault injected", "event", event, "kind", kind, "node", node, "meters", len(affected), "signals", len(sigs), "duration", dur.String())
		_ = c.retry(ctx, "signals", func() error {
			return c.do(ctx, "POST", "/api/v1/outage-signals/batch", map[string]any{"signals": sigs}, nil)
		})
		go func(ev int, ms []sim.Meter, d time.Duration) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			mu.Lock()
			for _, m := range ms {
				delete(st.Dead, m.ID)
			}
			mu.Unlock()
			var rs []outageSignal
			at := time.Now()
			for _, m := range ms {
				rs = append(rs, outageSignal{Source: "restore", ExternalID: fmt.Sprintf("rs-%d-%d-%d", cfg.Seed, ev, m.ID), SubscriberID: *m.SubscriberID,
					MeterID: m.ID, ReceivedAt: at})
			}
			for i := 0; i < len(rs); i += 1000 {
				j := min(i+1000, len(rs))
				_ = c.retry(ctx, "restore", func() error {
					return c.do(ctx, "POST", "/api/v1/outage-signals/batch", map[string]any{"signals": rs[i:j]}, nil)
				})
			}
			log.Info("fault cleared", "event", ev, "meters", len(ms))
		}(event, affected, dur)
	}
}
