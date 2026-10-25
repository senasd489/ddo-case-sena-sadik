// Package audit writes append-only audit records (who, when, what, old/new).
package audit

import (
	"context"
	"encoding/json"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

type Entry struct {
	Action   string // ör. "invoice.issue", "topology.switch"
	Entity   string // ör. "invoice"
	EntityID string
	Old      any
	New      any
}

// Record must be called with the same transaction that performs the change,
// so the audit row commits (or rolls back) together with it.
func Record(ctx context.Context, q db.Querier, e Entry) error {
	var actorID *int64
	var role *string
	if p := auth.FromContext(ctx); p != nil {
		if p.UserID > 0 {
			actorID = &p.UserID
		}
		r := string(p.Role)
		role = &r
	}
	oldJ, err := toJSON(e.Old)
	if err != nil {
		return err
	}
	newJ, err := toJSON(e.New)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO audit_log (actor_id, actor_role, action, entity, entity_id, old_value, new_value, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`,
		actorID, role, e.Action, e.Entity, e.EntityID, oldJ, newJ, httpx.RequestID(ctx))
	return err
}

func toJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return json.Marshal(v)
}
