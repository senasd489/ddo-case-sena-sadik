// Package bench contains reproducible measurements for the hard tasks.
package bench

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metering"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/seed"
)

type T1Options struct {
	Meters  int // 104 200 sayaç × 96 okuma ≈ 10 milyon/gün
	Workers int
	Batch   int
	Keep    bool // bench veritabanını silme
}

// RunT1 creates a scratch database next to the main one, seeds Meters
// subscribers, pushes one full day of readings through the production ingest
// path (COPY → staging → INSERT … ON CONFLICT) with parallel workers, then
// recomputes all consumption with the window-function query and reports
// throughput plus EXPLAIN ANALYZE of the hot read queries.
func RunT1(ctx context.Context, mainURL string, o T1Options, out io.Writer) error {
	if o.Meters <= 0 {
		o.Meters = 104_200
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Batch <= 0 {
		o.Batch = 20_000
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	u, err := url.Parse(mainURL)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("enerjihat_bench_%d", time.Now().Unix())
	admin, err := pgx.Connect(ctx, mainURL)
	if err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		return err
	}
	defer func() {
		if !o.Keep {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		}
		admin.Close(context.Background())
	}()
	u.Path = "/" + name
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return err
	}
	cfg.MaxConns = int32(o.Workers + 2)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	fmt.Fprintf(out, "T-1 benchmark — %d sayaç × 96 slot (+1 sınır okuması) ≈ %.1f M okuma, %d işçi, parti %d\n",
		o.Meters, float64(o.Meters*97)/1e6, o.Workers, o.Batch)
	t0 := time.Now()
	if err := db.Migrate(ctx, pool, log); err != nil {
		return err
	}
	if err := seed.Run(ctx, pool, log, seed.Options{Seed: 1, Subscribers: o.Meters}); err != nil {
		return err
	}
	fmt.Fprintf(out, "  şema + seed: %v\n", time.Since(t0).Round(time.Millisecond))

	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.FixedZone("TRT", 3*3600))
	jobs := make(chan []metering.Reading, o.Workers*2)
	var inserted, dups, rejected atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	start := time.Now()
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				res, err := metering.Ingest(ctx, pool, "bench", b)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					continue
				}
				inserted.Add(int64(res.Inserted))
				dups.Add(int64(res.Duplicates))
				rejected.Add(int64(res.Rejected))
			}
		}()
	}
	// Okumalar saat sırasıyla gelir (gerçek head-end gibi): her slotta tüm sayaçlar.
	rng := rand.New(rand.NewPCG(7, 7))
	idx := make([]int64, o.Meters)
	sent := 0
	batch := make([]metering.Reading, 0, o.Batch)
	for slot := 0; slot <= 96; slot++ {
		ts := day.Add(time.Duration(slot) * metering.Slot)
		for m := 0; m < o.Meters; m++ {
			idx[m] += int64(rng.IntN(300))
			if rng.IntN(1000) < 5 { // %0.5 eksik okuma
				continue
			}
			r := metering.Reading{MeterID: int64(m + 1), ReadAt: ts, ActiveWh: idx[m], ReactiveVarh: idx[m] / 10}
			batch = append(batch, r)
			if rng.IntN(1000) < 3 { // %0.3 tekrar
				batch = append(batch, r)
			}
			if len(batch) == o.Batch {
				jobs <- batch
				sent += len(batch)
				batch = make([]metering.Reading, 0, o.Batch)
			}
		}
	}
	if len(batch) > 0 {
		jobs <- batch
		sent += len(batch)
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	ingestDur := time.Since(start)
	fmt.Fprintf(out, "  okuma alma: %d gönderildi, %d yazıldı, %d tekrar, %d red — %v (%.0f okuma/sn)\n",
		sent, inserted.Load(), dups.Load(), rejected.Load(), ingestDur.Round(time.Millisecond), float64(sent)/ingestDur.Seconds())

	var parts int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'readings'::regclass`).Scan(&parts)
	fmt.Fprintf(out, "  readings partisyon sayısı: %d\n", parts)

	start = time.Now()
	// Gerçek kuyruk işleme yolu; 4 işçi paralel (SKIP LOCKED).
	var processed atomic.Int64
	var wg2 sync.WaitGroup
	for w := 0; w < o.Workers; w++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			n, err := metering.DrainDirty(ctx, pool, 2000)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
			}
			processed.Add(int64(n))
		}()
	}
	wg2.Wait()
	if firstErr != nil {
		return firstErr
	}
	compDur := time.Since(start)
	var slots, interp int64
	_ = pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE quality='interpolated') FROM consumption_15m`).Scan(&slots, &interp)
	fmt.Fprintf(out, "  tüketim hesabı: %d sayaç-gün, %d slot (%d interpolasyon) — %v (%.0f slot/sn)\n",
		processed.Load(), slots, interp, compDur.Round(time.Millisecond), float64(slots)/compDur.Seconds())

	_, _ = pool.Exec(ctx, "ANALYZE")
	for _, q := range []struct{ title, sql string }{
		{"Bir sayacın günlük ham okumaları (partition pruning + PK)", `SELECT * FROM readings WHERE meter_id = 777 AND read_at >= '2026-06-01 00:00+03' AND read_at < '2026-06-02 00:00+03'`},
		{"Abonenin 15 dk tüketimi (consumption_15m_sub_idx)", `SELECT * FROM consumption_15m WHERE subscriber_id = 777 AND slot_start >= '2026-06-01 00:00+03' AND slot_start < '2026-06-02 00:00+03'`},
	} {
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) "+q.sql)
		if err != nil {
			return err
		}
		var lines []string
		for rows.Next() {
			var l string
			_ = rows.Scan(&l)
			lines = append(lines, "    "+l)
		}
		rows.Close()
		fmt.Fprintf(out, "\n  EXPLAIN ANALYZE — %s\n%s\n", q.title, strings.Join(lines, "\n"))
	}
	fmt.Fprintf(out, "\n  toplam süre: %v\n", time.Since(t0).Round(time.Millisecond))
	return nil
}
