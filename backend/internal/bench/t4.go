package bench

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/loss"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/seed"
)

// RunT4 plants known anomalies and confounders into a scratch database and
// prints detection quality with the false-alarm filters off and on.
func RunT4(ctx context.Context, mainURL string, subscribers int, out io.Writer) error {
	if subscribers <= 0 {
		subscribers = 5000
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	u, err := url.Parse(mainURL)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("enerjihat_bench_t4_%d", time.Now().Unix())
	admin, err := pgx.Connect(ctx, mainURL)
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	defer admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, log); err != nil {
		return err
	}
	if err := seed.Run(ctx, pool, log, seed.Options{Seed: 42, Subscribers: subscribers}); err != nil {
		return err
	}
	asOf := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	truth, err := loss.Plant(ctx, pool, asOf, 1)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "T-4 değerlendirmesi — %d abone, 28 gün; yerleştirilen: %d kaçak (ani düşüş), %d gece anomalisi;\n",
		subscribers, len(truth.Thieves), len(truth.NightUsers))
	fmt.Fprintf(out, "tuzaklar: %d kesinti yaşayan, %d eksik verili abone, %d yüksek değişkenlikli normal abone, TR-0003'te 1 eksik veri günü\n\n",
		len(truth.OutageSubs), len(truth.MissingSubs), len(truth.Noisy))
	fmt.Fprintf(out, "| Mod | İşaretlenen | Doğru | Yanlış alarm | Kaçırılan | Precision | Recall | Yanlış alarm oranı | YA nedeni |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range []bool{false, true} {
		start := time.Now()
		sum, err := loss.Run(ctx, pool, loss.Options{AsOf: asOf, Filters: f})
		if err != nil {
			return err
		}
		sc := loss.Evaluate(sum.Flags, truth, subscribers)
		mode := "Filtresiz (ortalama/std, tek gün)"
		if f {
			mode = "Filtreli (medyan/MAD + kalite + bağlam + kalıcılık)"
		}
		fmt.Fprintf(out, "| %s | %d | %d | %d | %d | %.3f | %.3f | %.4f | %v |\n", mode, sc.Flagged, sc.TruePositives, sc.FalsePositives,
			sc.FalseNegatives, sc.Precision, sc.Recall, sc.FalseAlarmRate, sc.FPByCause)
		_ = start
	}
	sum, _ := loss.Run(ctx, pool, loss.Options{AsOf: asOf, Filters: true})
	if len(sum.Flags) > 0 {
		fmt.Fprintf(out, "\nÖrnek gerekçe (en yüksek skor): %v\n", sum.Flags[0].Explanation["reason"])
	}
	return nil
}
