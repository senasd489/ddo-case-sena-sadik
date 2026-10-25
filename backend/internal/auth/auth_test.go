package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestHashAndVerifyPassword(t *testing.T) {
	h, err := HashPassword("Demo1234!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", h)
	}
	ok, err := VerifyPassword(h, "Demo1234!")
	if err != nil || !ok {
		t.Fatalf("correct password rejected: %v", err)
	}
	ok, _ = VerifyPassword(h, "demo1234!")
	if ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("Demo1234!")
	if h == h2 {
		t.Fatal("salt not random: identical hashes")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, h := range []string{"", "plain", "$2a$10$bcrypt", "$argon2id$v=19$m=1,t=1,p=1$!!$!!"} {
		if ok, err := VerifyPassword(h, "x"); ok || err == nil {
			t.Errorf("hash %q: want error, got ok=%v err=%v", h, ok, err)
		}
	}
}

func TestJWTRoundTrip(t *testing.T) {
	s := NewSigner("0123456789abcdef0123456789abcdef", time.Minute)
	sid := int64(7)
	tok, exp, err := s.Issue(&Principal{UserID: 3, Email: "a@b", Role: RoleSubscriber, SubscriberID: &sid})
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) > time.Minute || time.Until(exp) < 50*time.Second {
		t.Fatalf("bad expiry %v", exp)
	}
	p, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != 3 || p.Role != RoleSubscriber || p.SubscriberID == nil || *p.SubscriberID != 7 {
		t.Fatalf("claims lost: %+v", p)
	}
}

func TestJWTRejectsExpiredTamperedAndForeign(t *testing.T) {
	s := NewSigner("0123456789abcdef0123456789abcdef", time.Minute)
	tok, _, _ := s.Issue(&Principal{UserID: 1, Role: RoleAdmin})

	// expired
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := s.Verify(tok); err == nil {
		t.Error("expired token accepted")
	}
	s.now = time.Now

	// different secret
	other := NewSigner("ffffffffffffffffffffffffffffffff", time.Minute)
	if _, err := other.Verify(tok); err == nil {
		t.Error("token signed with another secret accepted")
	}

	// tampered payload
	parts := strings.Split(tok, ".")
	parts[1] = parts[1][:len(parts[1])-2] + "AA"
	if _, err := s.Verify(strings.Join(parts, ".")); err == nil {
		t.Error("tampered token accepted")
	}

	// alg=none
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "1", "role": "admin", "iss": issuer,
		"exp": time.Now().Add(time.Hour).Unix()})
	ns, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := s.Verify(ns); err == nil {
		t.Error("alg=none accepted")
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range AllRoles {
		if !r.Valid() {
			t.Errorf("%s should be valid", r)
		}
	}
	if Role("root").Valid() {
		t.Error("unknown role accepted")
	}
	p := &Principal{Role: RoleBilling}
	if !p.Is(RoleAdmin, RoleBilling) || p.Is(RoleAdmin) {
		t.Error("Principal.Is wrong")
	}
}
