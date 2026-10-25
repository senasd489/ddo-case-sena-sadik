// Package seed loads deterministic demo data: tariffs, the grid topology,
// subscribers, meters, crews and one demo account per role. The same seed
// value always produces exactly the same rows.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
)

const DemoPassword = "Demo1234!"

type Options struct {
	Seed        uint64
	Subscribers int       // toplam abone (sayaç) sayısı
	Epoch       time.Time // sayaçların kurulum tarihi
}

// DemoUsers lists the accounts documented in README/TESTING.md.
var DemoUsers = []struct {
	Email, Name string
	Role        auth.Role
}{
	{"abone@enerjihat.local", "Ayşe Abone", auth.RoleSubscriber},
	{"saha@enerjihat.local", "Selim Saha", auth.RoleFieldCrew},
	{"fatura@enerjihat.local", "Fatma Faturalama", auth.RoleBilling},
	{"operator@enerjihat.local", "Oğuz Operatör", auth.RoleOperator},
	{"analist@enerjihat.local", "Aslı Analist", auth.RoleAnalyst},
	{"admin@enerjihat.local", "Yusuf Yönetici", auth.RoleAdmin},
}

// Center of the synthetic service area (Bandırma).
const baseLat, baseLon = 40.3520, 27.9770

// Grid shape: each transformer feeds feedersPerTr feeders, each feeder a
// chain/tree of polesPerFeeder poles; subscribers are spread over poles.
const (
	subsPerTransformer = 200
	feedersPerTr       = 3
	polesPerFeeder     = 8
)

// AlreadySeeded reports whether demo data exists.
func AlreadySeeded(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_info WHERE key='seeded')`).Scan(&ok)
	return ok, err
}

func Run(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, o Options) error {
	if o.Subscribers <= 0 {
		o.Subscribers = 5000
	}
	if o.Epoch.IsZero() {
		o.Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rng := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15))
	start := time.Now()

	pwHash, err := auth.HashPassword(DemoPassword)
	if err != nil {
		return err
	}

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := seedTariffs(ctx, tx); err != nil {
			return fmt.Errorf("tariffs: %w", err)
		}
		if err := seedGrid(ctx, tx, rng, o); err != nil {
			return fmt.Errorf("grid: %w", err)
		}
		if err := seedCrews(ctx, tx, rng); err != nil {
			return fmt.Errorf("crews: %w", err)
		}
		for _, u := range DemoUsers {
			var sub, crew any
			switch u.Role {
			case auth.RoleSubscriber:
				sub = int64(1)
			case auth.RoleFieldCrew:
				crew = int64(1)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO users (email, password_hash, full_name, role, subscriber_id, crew_id)
				VALUES ($1,$2,$3,$4,$5,$6)`, u.Email, pwHash, u.Name, string(u.Role), sub, crew); err != nil {
				return fmt.Errorf("user %s: %w", u.Email, err)
			}
		}
		// Abone #2 için de giriş: yetki testleri (başka abonenin verisine erişim) için.
		if _, err := tx.Exec(ctx, `INSERT INTO users (email, password_hash, full_name, role, subscriber_id)
			VALUES ('abone2@enerjihat.local', $1, 'Barış İkinci Abone', 'subscriber', 2)`, pwHash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app_info(key, value) VALUES ('seeded', $1), ('seed_value', $2), ('seed_subscribers', $3)`,
			time.Now().UTC().Format(time.RFC3339), fmt.Sprint(o.Seed), fmt.Sprint(o.Subscribers))
		return err
	})
	if err != nil {
		return err
	}
	log.Info("seed complete", "subscribers", o.Subscribers, "seed", o.Seed, "ms", time.Since(start).Milliseconds())
	return nil
}

func seedTariffs(ctx context.Context, tx pgx.Tx) error {
	// Fiyatlar kuruş/kWh. 15 Ekim'de mesken tarifesi değişir: dönem ortası
	// tarife değişikliği (gün esaslı bölme) bu sayede demo verisinde vardır.
	_, err := tx.Exec(ctx, `
	INSERT INTO tariffs (id, code, name, category) VALUES
	  (1, 'MESKEN', 'Mesken (kademeli)', 'residential'),
	  (2, 'TICARETHANE', 'Ticarethane (üç zamanlı)', 'commercial'),
	  (3, 'SANAYI', 'Sanayi (üç zamanlı, reaktif)', 'industrial'),
	  (4, 'TARIM', 'Tarımsal sulama (kademeli)', 'agricultural');
	SELECT setval('tariffs_id_seq', 4);

	INSERT INTO tariff_versions (id, tariff_id, valid_from, valid_to, mode, day_price, peak_price, night_price,
	                             distribution_price, reactive_price, reactive_free_ratio, energy_tax_rate, fund_rate, vat_rate) VALUES
	  (1, 1, '2026-01-01', '2026-10-15', 'tiered', NULL, NULL, NULL, 95.0000, 0, 0.20, 0.05, 0.02, 0.20),
	  (2, 1, '2026-10-15', NULL,         'tiered', NULL, NULL, NULL, 104.5000, 0, 0.20, 0.05, 0.02, 0.20),
	  (3, 2, '2026-01-01', NULL, 'tou', 255.4000, 389.1000, 158.2000, 110.0000, 45.0000, 0.20, 0.05, 0.02, 0.20),
	  (4, 3, '2026-01-01', NULL, 'tou', 230.9000, 351.7000, 142.6000, 88.0000, 52.0000, 0.20, 0.01, 0.02, 0.20),
	  (5, 4, '2026-01-01', NULL, 'tiered', NULL, NULL, NULL, 70.0000, 0, 0.20, 0.01, 0.02, 0.20);
	SELECT setval('tariff_versions_id_seq', 5);

	INSERT INTO tariff_tiers (version_id, tier_no, from_kwh, to_kwh, unit_price) VALUES
	  (1, 1, 0, 150, 182.3000), (1, 2, 150, 300, 245.6000), (1, 3, 300, NULL, 321.9000),
	  (2, 1, 0, 150, 199.8000), (2, 2, 150, 300, 268.4000), (2, 3, 300, NULL, 352.0000),
	  (5, 1, 0, 1000, 160.0000), (5, 2, 1000, NULL, 210.0000);`)
	return err
}

type node struct {
	id       int64
	lat, lon float64
}

func seedGrid(ctx context.Context, tx pgx.Tx, rng *rand.Rand, o Options) error {
	nTr := int(math.Ceil(float64(o.Subscribers) / subsPerTransformer))
	var nodeRows [][]any
	var nextID int64 = 1
	transformers := make([]node, 0, nTr)
	var poles []node

	for t := 0; t < nTr; t++ {
		// Trafolar merkez etrafında bir ızgaraya yerleşir.
		side := int(math.Ceil(math.Sqrt(float64(nTr))))
		lat := baseLat + (float64(t/side)-float64(side)/2)*0.012 + (rng.Float64()-0.5)*0.003
		lon := baseLon + (float64(t%side)-float64(side)/2)*0.016 + (rng.Float64()-0.5)*0.003
		tr := node{nextID, lat, lon}
		nextID++
		transformers = append(transformers, tr)
		techLoss := 3.0 + float64(rng.IntN(30))/10 // %3.0 – %5.9
		nodeRows = append(nodeRows, []any{tr.id, fmt.Sprintf("TR-%04d", t+1), "transformer", fmt.Sprintf("Trafo %d", t+1),
			nil, nil, nil, lat, lon, 400 + 100*rng.IntN(6), techLoss})
		for f := 0; f < feedersPerTr; f++ {
			fid := nextID
			nextID++
			flat, flon := lat+(rng.Float64()-0.5)*0.004, lon+(rng.Float64()-0.5)*0.004
			nodeRows = append(nodeRows, []any{fid, fmt.Sprintf("TR-%04d-F%d", t+1, f+1), "feeder",
				fmt.Sprintf("Besleyici %d/%d", t+1, f+1), tr.id, nil, tr.id, flat, flon, nil, 0.0})
			// Direkler: ilk direk besleyiciye, sonrakiler ağaç şeklinde öncekilere bağlanır.
			first := nextID
			for p := 0; p < polesPerFeeder; p++ {
				pid := nextID
				nextID++
				parent := fid
				if p > 0 {
					parent = first + int64(rng.IntN(p)) // 0..p-1 arasından rastgele bir önceki direk
				}
				plat, plon := flat+(rng.Float64()-0.5)*0.006, flon+(rng.Float64()-0.5)*0.006
				poles = append(poles, node{pid, plat, plon})
				nodeRows = append(nodeRows, []any{pid, fmt.Sprintf("TR-%04d-F%d-P%02d", t+1, f+1, p+1), "pole",
					fmt.Sprintf("Direk %d/%d/%d", t+1, f+1, p+1), parent, nil, parent, plat, plon, nil, 0.0})
			}
		}
	}
	// Yedek besleme: her besleyicinin alternatif kaynağı komşu trafo.
	for i := range nodeRows {
		if nodeRows[i][2] == "feeder" && nTr > 1 {
			tr := nodeRows[i][4].(int64)
			idx := 0
			for k, t := range transformers {
				if t.id == tr {
					idx = k
				}
			}
			nodeRows[i][5] = transformers[(idx+1)%nTr].id
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"network_nodes"},
		[]string{"id", "code", "kind", "name", "parent_id", "alt_parent_id", "normal_parent_id", "lat", "lon", "capacity_kva", "tech_loss_pct"},
		pgx.CopyFromRows(nodeRows)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT setval('network_nodes_id_seq', $1)`, nextID); err != nil {
		return err
	}

	firstNames := []string{"Ahmet", "Mehmet", "Ayşe", "Fatma", "Mustafa", "Zeynep", "Emine", "Ali", "Hüseyin", "Elif", "Hasan", "Merve", "İbrahim", "Esra", "Murat", "Deniz", "Can", "Ece", "Burak", "Selin"}
	lastNames := []string{"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir", "Arslan", "Doğan", "Kılıç", "Aslan", "Çetin", "Kara", "Koç", "Kurt", "Özkan", "Şimşek"}
	streets := []string{"Atatürk Cd.", "Cumhuriyet Mh.", "Paşabayır Mh.", "Hacı Yusuf Mh.", "Sunullah Mh.", "Edincik Yolu", "Erdek Cd.", "Liman Cd.", "Ömer Seyfettin Cd.", "17 Eylül Mh."}

	subRows := make([][]any, 0, o.Subscribers)
	meterRows := make([][]any, 0, o.Subscribers+nTr)
	for i := 0; i < o.Subscribers; i++ {
		id := int64(i + 1)
		tr := i / subsPerTransformer
		// Aynı trafonun direklerine dağıt.
		poleIdx := tr*feedersPerTr*polesPerFeeder + rng.IntN(feedersPerTr*polesPerFeeder)
		pole := poles[poleIdx]
		cat, tariff, kw := "residential", int64(1), 7.0+float64(rng.IntN(8))
		switch r := rng.IntN(100); {
		case r >= 99:
			cat, tariff, kw = "agricultural", 4, 15+float64(rng.IntN(20))
		case r >= 95:
			cat, tariff, kw = "industrial", 3, 50+float64(rng.IntN(200))
		case r >= 85:
			cat, tariff, kw = "commercial", 2, 15+float64(rng.IntN(30))
		}
		// İlk iki abone demo hesaplarına bağlıdır; mesken olsunlar.
		if i < 2 {
			cat, tariff, kw = "residential", 1, 10
		}
		name := firstNames[rng.IntN(len(firstNames))] + " " + lastNames[rng.IntN(len(lastNames))]
		addr := fmt.Sprintf("%s No:%d, Bandırma", streets[rng.IntN(len(streets))], 1+rng.IntN(120))
		subRows = append(subRows, []any{id, fmt.Sprintf("AB%08d", id), name, addr, cat, pole.id, tariff, kw,
			pole.lat + (rng.Float64()-0.5)*0.001, pole.lon + (rng.Float64()-0.5)*0.001})
		meterRows = append(meterRows, []any{id, fmt.Sprintf("SM%09d", id), "subscriber", id, nil, o.Epoch})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"subscribers"},
		[]string{"id", "subscriber_no", "full_name", "address", "category", "pole_id", "tariff_id", "contract_kw", "lat", "lon"},
		pgx.CopyFromRows(subRows)); err != nil {
		return err
	}
	for t, tr := range transformers {
		meterRows = append(meterRows, []any{int64(o.Subscribers + t + 1), fmt.Sprintf("HM%06d", t+1), "head", nil, tr.id, o.Epoch})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"meters"},
		[]string{"id", "serial_no", "kind", "subscriber_id", "node_id", "installed_at"},
		pgx.CopyFromRows(meterRows)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT setval('subscribers_id_seq', $1), setval('meters_id_seq', $2)`,
		int64(o.Subscribers), int64(len(meterRows)))
	return err
}

func seedCrews(ctx context.Context, tx pgx.Tx, rng *rand.Rand) error {
	skills := [][]string{
		{"lv", "mv"}, {"lv"}, {"lv", "mv", "transformer"}, {"lv"},
		{"mv", "transformer"}, {"lv", "mv"}, {"lv"}, {"lv", "mv", "transformer"},
	}
	for i, sk := range skills {
		lat := baseLat + (rng.Float64()-0.5)*0.08
		lon := baseLon + (rng.Float64()-0.5)*0.10
		if _, err := tx.Exec(ctx, `INSERT INTO crews (code, name, skills, lat, lon, speed_factor) VALUES ($1,$2,$3,$4,$5,$6)`,
			fmt.Sprintf("EKIP-%02d", i+1), fmt.Sprintf("Saha Ekibi %d", i+1), sk, lat, lon, 0.8+float64(rng.IntN(5))/10); err != nil {
			return err
		}
	}
	return nil
}
