// Package auth implements password hashing, JWT access tokens, rotating
// refresh tokens and role-based access control.
package auth

import "context"

type Role string

const (
	RoleSubscriber Role = "subscriber" // Abone
	RoleFieldCrew  Role = "field_crew" // Saha ekibi
	RoleBilling    Role = "billing"    // Faturalama uzmanı
	RoleOperator   Role = "operator"   // Şebeke operatörü
	RoleAnalyst    Role = "analyst"    // Analist
	RoleAdmin      Role = "admin"      // Yönetici
)

var AllRoles = []Role{RoleSubscriber, RoleFieldCrew, RoleBilling, RoleOperator, RoleAnalyst, RoleAdmin}

func (r Role) Valid() bool {
	for _, x := range AllRoles {
		if x == r {
			return true
		}
	}
	return false
}

// Principal is the authenticated caller, available from the request context.
type Principal struct {
	UserID       int64  `json:"user_id"`
	Email        string `json:"email"`
	Role         Role   `json:"role"`
	SubscriberID *int64 `json:"subscriber_id,omitempty"`
	CrewID       *int64 `json:"crew_id,omitempty"`
}

func (p *Principal) Is(roles ...Role) bool {
	for _, r := range roles {
		if p.Role == r {
			return true
		}
	}
	return false
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// Staff roles may read any subscriber's data.
var Staff = []Role{RoleFieldCrew, RoleBilling, RoleOperator, RoleAnalyst, RoleAdmin}

// CanAccessSubscriber enforces row-level ownership: subscribers only see
// their own record; staff roles see everyone.
func (p *Principal) CanAccessSubscriber(id int64) bool {
	if p == nil {
		return false
	}
	if p.Role == RoleSubscriber {
		return p.SubscriberID != nil && *p.SubscriberID == id
	}
	return p.Is(Staff...)
}

// System is the principal used for machine-to-machine ingestion.
var System = &Principal{UserID: 0, Email: "system@enerjihat.local", Role: "system"}
