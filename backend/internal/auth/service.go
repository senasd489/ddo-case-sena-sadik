package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrBadCredentials = errors.New("invalid email or password")
	ErrRefreshInvalid = errors.New("refresh token invalid or expired")
	ErrRefreshReused  = errors.New("refresh token reuse detected")
)

// Tokens is returned by Login and Refresh.
type Tokens struct {
	AccessToken      string     `json:"access_token"`
	AccessExpiresAt  time.Time  `json:"access_expires_at"`
	RefreshToken     string     `json:"refresh_token"`
	RefreshExpiresAt time.Time  `json:"refresh_expires_at"`
	TokenType        string     `json:"token_type"`
	User             *Principal `json:"user"`
}

type Service struct {
	pool       *pgxpool.Pool
	signer     *Signer
	refreshTTL time.Duration
	dummyHash  string
}

func NewService(pool *pgxpool.Pool, signer *Signer, refreshTTL time.Duration) *Service {
	// Kullanıcı bulunamadığında da aynı maliyette bir doğrulama yapılır, böylece
	// yanıt süresinden e-postanın kayıtlı olup olmadığı anlaşılamaz.
	h, _ := HashPassword("timing-equaliser")
	return &Service{pool: pool, signer: signer, refreshTTL: refreshTTL, dummyHash: h}
}

func (s *Service) Signer() *Signer { return s.signer }

func (s *Service) Login(ctx context.Context, email, password string) (*Tokens, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var (
		p        Principal
		hash     string
		disabled bool
	)
	err := s.pool.QueryRow(ctx, `SELECT id, email, role, subscriber_id, crew_id, password_hash, disabled
		FROM users WHERE email = $1`, email).
		Scan(&p.UserID, &p.Email, &p.Role, &p.SubscriberID, &p.CrewID, &hash, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(s.dummyHash, password)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil {
		return nil, err
	}
	if !ok || disabled {
		return nil, ErrBadCredentials
	}
	var t *Tokens
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		t, err = s.issue(ctx, tx, &p, "")
		return err
	})
	return t, err
}

// Refresh rotates a refresh token. A token may be used exactly once; if an
// already-rotated token shows up again the whole family is revoked, because
// that means the token was stolen and used by someone else.
func (s *Service) Refresh(ctx context.Context, raw string) (*Tokens, error) {
	h := hashToken(raw)
	var t *Tokens
	reused := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id, family string
			p          Principal
			expires    time.Time
			revoked    *time.Time
			disabled   bool
		)
		err := tx.QueryRow(ctx, `SELECT rt.id, rt.family_id, rt.expires_at, rt.revoked_at,
				u.id, u.email, u.role, u.subscriber_id, u.crew_id, u.disabled
			FROM refresh_tokens rt JOIN users u ON u.id = rt.user_id
			WHERE rt.token_hash = $1 FOR UPDATE OF rt`, h).
			Scan(&id, &family, &expires, &revoked, &p.UserID, &p.Email, &p.Role, &p.SubscriberID, &p.CrewID, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRefreshInvalid
		}
		if err != nil {
			return err
		}
		if revoked != nil {
			// İptal commit edilsin diye hata döndürmeden çıkılır.
			reused = true
			_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, family)
			return err
		}
		if disabled || time.Now().After(expires) {
			return ErrRefreshInvalid
		}
		t, err = s.issue(ctx, tx, &p, family)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now(),
				replaced_by = (SELECT id FROM refresh_tokens WHERE token_hash = $2)
			WHERE id = $1`, id, hashToken(t.RefreshToken))
		return err
	})
	if err == nil && reused {
		return nil, ErrRefreshReused
	}
	return t, err
}

func (s *Service) Logout(ctx context.Context, raw string) error {
	_, err := s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()
		WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1) AND revoked_at IS NULL`, hashToken(raw))
	return err
}

func (s *Service) issue(ctx context.Context, tx pgx.Tx, p *Principal, family string) (*Tokens, error) {
	access, accessExp, err := s.signer.Issue(p)
	if err != nil {
		return nil, err
	}
	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	refreshExp := time.Now().Add(s.refreshTTL)
	if family == "" {
		_, err = tx.Exec(ctx, `INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
			VALUES ($1, gen_random_uuid(), $2, $3)`, p.UserID, hashToken(raw), refreshExp)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
			VALUES ($1, $2, $3, $4)`, p.UserID, family, hashToken(raw), refreshExp)
	}
	if err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}
	return &Tokens{AccessToken: access, AccessExpiresAt: accessExp, RefreshToken: raw,
		RefreshExpiresAt: refreshExp, TokenType: "Bearer", User: p}, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}
