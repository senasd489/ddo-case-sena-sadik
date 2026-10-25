// Package demo builds the demo dataset that goes beyond the base seed.
package demo

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/loss"
)

// History writes 28 days of daily consumption ending two days before
// anchor (the period before the simulator's live stream starts), including
// the deliberately suspicious patterns, and records the simulator's ground
// truth in sim_profiles. The simulator continues the same profiles.
func History(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, anchor time.Time, seed uint64) error {
	start := time.Now()
	end := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -2)
	truth, err := loss.Plant(ctx, pool, end, seed)
	if err != nil {
		return err
	}
	since := end.AddDate(0, 0, -7)
	rows := [][2]any{}
	add := func(ids []int64, p string) {
		for _, id := range ids {
			rows = append(rows, [2]any{id, p})
		}
	}
	add(truth.Thieves, "thief")
	add(truth.NightUsers, "night")
	add(truth.OutageSubs, "outage_bait")
	add(truth.MissingSubs, "missing_bait")
	add(truth.Noisy, "noisy")
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO sim_profiles (subscriber_id, profile, since) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, r[0], r[1], since); err != nil {
			return err
		}
	}
	_, _ = pool.Exec(ctx, `INSERT INTO app_info(key, value) VALUES ('history_end', $1) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, end.Format("2006-01-02"))
	log.Info("seed history complete", "until", end.Format("2006-01-02"), "thieves", len(truth.Thieves), "night", len(truth.NightUsers),
		"ms", time.Since(start).Milliseconds())
	return nil
}
