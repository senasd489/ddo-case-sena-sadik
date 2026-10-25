package auth

import (
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type claims struct {
	Role         Role   `json:"role"`
	Email        string `json:"email"`
	SubscriberID *int64 `json:"sid,omitempty"`
	CrewID       *int64 `json:"cid,omitempty"`
	jwt.RegisteredClaims
}

const issuer = "enerjihat"

// Signer issues and verifies short-lived HS256 access tokens.
type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewSigner(secret string, ttl time.Duration) *Signer {
	return &Signer{secret: []byte(secret), ttl: ttl, now: time.Now}
}

func (s *Signer) Issue(p *Principal) (string, time.Time, error) {
	now := s.now()
	exp := now.Add(s.ttl)
	c := claims{
		Role: p.Role, Email: p.Email, SubscriberID: p.SubscriberID, CrewID: p.CrewID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(p.UserID, 10),
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.secret)
	return tok, exp, err
}

var ErrInvalidToken = errors.New("invalid token")

func (s *Signer) Verify(token string) (*Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		return s.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return nil, ErrInvalidToken
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || !c.Role.Valid() {
		return nil, ErrInvalidToken
	}
	return &Principal{UserID: id, Email: c.Email, Role: c.Role, SubscriberID: c.SubscriberID, CrewID: c.CrewID}, nil
}
