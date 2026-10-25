package server

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/seed"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

func TestHealthReportsDatabase(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/health", "", nil)
	var h struct {
		Status string
		Checks struct{ Database struct{ Status string } }
	}
	r.json(t, &h)
	if r.Status != 200 || h.Status != "ok" || h.Checks.Database.Status != "up" {
		t.Fatalf("health: %d %s", r.Status, r.Body)
	}
}

func TestLoginOutcomes(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, email, pw string
		status          int
		code            string
	}{
		{"ok", "admin@enerjihat.local", seed.DemoPassword, 200, ""},
		{"case-insensitive email", "ADMIN@enerjihat.local", seed.DemoPassword, 200, ""},
		{"wrong password", "admin@enerjihat.local", "yanlis", 401, "invalid_credentials"},
		{"unknown user", "kimse@enerjihat.local", seed.DemoPassword, 401, "invalid_credentials"},
		{"invalid email", "not-an-email", "x", 422, "validation_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.do("POST", "/api/v1/auth/login", "", map[string]string{"email": c.email, "password": c.pw})
			if r.Status != c.status {
				t.Fatalf("status %d, want %d: %s", r.Status, c.status, r.Body)
			}
			if c.code != "" && r.errCode(t) != c.code {
				t.Fatalf("code %s, want %s", r.errCode(t), c.code)
			}
		})
	}
}

func TestEveryDemoRoleCanLogIn(t *testing.T) {
	e := newEnv(t)
	for _, u := range seed.DemoUsers {
		tk := e.login(u.Email)
		var me struct{ Role string }
		e.do("GET", "/api/v1/auth/me", tk.AccessToken, nil).json(t, &me)
		if me.Role != string(u.Role) {
			t.Errorf("%s: role %s, want %s", u.Email, me.Role, u.Role)
		}
	}
}

func TestUnauthenticatedAndForbidden(t *testing.T) {
	e := newEnv(t)
	if r := e.do("GET", "/api/v1/auth/me", "", nil); r.Status != 401 || r.errCode(t) != "unauthorized" {
		t.Fatalf("no token: %d %s", r.Status, r.Body)
	}
	if r := e.do("GET", "/api/v1/auth/me", "garbage.token.here", nil); r.Status != 401 || r.errCode(t) != "invalid_token" {
		t.Fatalf("bad token: %d %s", r.Status, r.Body)
	}
	for _, email := range []string{"abone@enerjihat.local", "saha@enerjihat.local", "fatura@enerjihat.local",
		"operator@enerjihat.local", "analist@enerjihat.local"} {
		tk := e.login(email)
		if r := e.do("GET", "/api/v1/admin/users", tk.AccessToken, nil); r.Status != 403 || r.errCode(t) != "forbidden" {
			t.Errorf("%s on admin route: %d %s", email, r.Status, r.Body)
		}
	}
}

func TestAdminUserListKeysetPagination(t *testing.T) {
	e := newEnv(t)
	tk := e.login("admin@enerjihat.local")
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/api/v1/admin/users?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var page struct {
			Items      []struct{ ID int64 }
			NextCursor *string `json:"next_cursor"`
		}
		e.do("GET", path, tk.AccessToken, nil).json(t, &page)
		pages++
		for _, u := range page.Items {
			if seen[u.ID] {
				t.Fatalf("duplicate id %d across pages", u.ID)
			}
			seen[u.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != len(seed.DemoUsers)+1 || pages != 3 {
		t.Fatalf("saw %d users in %d pages", len(seen), pages)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := newEnv(t)
	tk := e.login("operator@enerjihat.local")

	r := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": tk.RefreshToken})
	if r.Status != 200 {
		t.Fatalf("refresh: %d %s", r.Status, r.Body)
	}
	var next tokens
	r.json(t, &next)
	if next.RefreshToken == tk.RefreshToken {
		t.Fatal("refresh token not rotated")
	}
	// Eski token tekrar kullanılırsa: çalınmış sayılır, tüm aile iptal edilir.
	if r := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": tk.RefreshToken}); r.Status != 401 || r.errCode(t) != "refresh_reused" {
		t.Fatalf("reuse: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": next.RefreshToken}); r.Status != 401 {
		t.Fatalf("family not revoked after reuse: %d %s", r.Status, r.Body)
	}
}

func TestLogoutRevokesRefresh(t *testing.T) {
	e := newEnv(t)
	tk := e.login("analist@enerjihat.local")
	if r := e.do("POST", "/api/v1/auth/logout", "", map[string]string{"refresh_token": tk.RefreshToken}); r.Status != 204 {
		t.Fatalf("logout %d", r.Status)
	}
	if r := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": tk.RefreshToken}); r.Status != 401 {
		t.Fatalf("refresh after logout: %d", r.Status)
	}
}

func newUserBody(email string) map[string]any {
	return map[string]any{"email": email, "full_name": "Test", "password": "Parola123!", "role": "analyst"}
}

func TestIdempotencyReplayAndMismatch(t *testing.T) {
	e := newEnv(t)
	tk := e.login("admin@enerjihat.local")
	key := "key-user-create-0001"

	r1 := e.do("POST", "/api/v1/admin/users", tk.AccessToken, newUserBody("yeni@enerjihat.local"), "Idempotency-Key", key)
	r2 := e.do("POST", "/api/v1/admin/users", tk.AccessToken, newUserBody("yeni@enerjihat.local"), "Idempotency-Key", key)
	if r1.Status != 201 || r2.Status != 201 || string(r1.Body) != string(r2.Body) {
		t.Fatalf("replay differs: %d %s / %d %s", r1.Status, r1.Body, r2.Status, r2.Body)
	}
	if r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay header missing")
	}
	// Aynı anahtar, farklı gövde → 422
	r3 := e.do("POST", "/api/v1/admin/users", tk.AccessToken, newUserBody("baska@enerjihat.local"), "Idempotency-Key", key)
	if r3.Status != 422 || r3.errCode(t) != "idempotency_key_mismatch" {
		t.Fatalf("mismatch: %d %s", r3.Status, r3.Body)
	}
	// Anahtarsız tekrar: gerçek ikinci istek, e-posta çakışması.
	if r := e.do("POST", "/api/v1/admin/users", tk.AccessToken, newUserBody("yeni@enerjihat.local")); r.Status != 409 {
		t.Fatalf("duplicate without key: %d %s", r.Status, r.Body)
	}
}

// Aynı anahtarla eşzamanlı 10 istek: tam olarak bir kayıt oluşur.
func TestIdempotencyConcurrentSameKey(t *testing.T) {
	e := newEnv(t)
	tk := e.login("admin@enerjihat.local")
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = e.do("POST", "/api/v1/admin/users", tk.AccessToken, newUserBody("es@enerjihat.local"),
				"Idempotency-Key", "concurrent-key-01").Status
		}(i)
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		if s != 201 && s != 409 {
			t.Fatalf("unexpected status %d (%v)", s, statuses)
		}
		if s == 201 {
			created++
		}
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email='es@enerjihat.local'`).Scan(&n)
	if n != 1 || created < 1 {
		t.Fatalf("users=%d created-responses=%d statuses=%v", n, created, statuses)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	e := newEnv(t)
	tk := e.login("admin@enerjihat.local")
	if r := e.do("POST", "/api/v1/admin/users", tk.AccessToken, newUserBody("denetim@enerjihat.local")); r.Status != 201 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	var page struct {
		Items []struct {
			Action    string
			ActorRole *string        `json:"actor_role"`
			New       map[string]any `json:"new_value"`
		}
	}
	e.do("GET", "/api/v1/admin/audit?entity=user", tk.AccessToken, nil).json(t, &page)
	if len(page.Items) == 0 || page.Items[0].Action != "user.create" || *page.Items[0].ActorRole != "admin" ||
		page.Items[0].New["email"] != "denetim@enerjihat.local" {
		t.Fatalf("audit row missing: %+v", page)
	}
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE audit_log SET action='x'`); err == nil {
		t.Fatal("UPDATE on audit_log succeeded")
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE on audit_log succeeded")
	}
	if _, err := e.pool.Exec(ctx, `TRUNCATE audit_log`); err == nil {
		t.Fatal("TRUNCATE on audit_log succeeded")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	pool := testutil.EmptyDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx, pool, testutil.Logger()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// Aynı tohum → bit düzeyinde aynı seed verisi; farklı tohum → farklı veri.
func TestSeedIsDeterministic(t *testing.T) {
	ctx := context.Background()
	digest := func(s uint64) string {
		pool := testutil.EmptyDB(t)
		if err := db.Migrate(ctx, pool, testutil.Logger()); err != nil {
			t.Fatal(err)
		}
		if err := seed.Run(ctx, pool, testutil.Logger(), seed.Options{Seed: s, Subscribers: 450}); err != nil {
			t.Fatal(err)
		}
		var d string
		err := pool.QueryRow(ctx, `SELECT md5(
			(SELECT string_agg(concat_ws(',', id, code, parent_id, alt_parent_id, round(lat::numeric,6), round(lon::numeric,6), tech_loss_pct), ';' ORDER BY id) FROM network_nodes) ||
			(SELECT string_agg(concat_ws(',', id, full_name, address, category, pole_id, tariff_id, contract_kw), ';' ORDER BY id) FROM subscribers) ||
			(SELECT string_agg(concat_ws(',', id, skills, round(lat::numeric,6)), ';' ORDER BY id) FROM crews))`).Scan(&d)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	a, b, c := digest(7), digest(7), digest(8)
	if a != b {
		t.Fatalf("same seed differs: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("different seeds produced identical data")
	}
	fmt.Println("seed digest", a)
}
