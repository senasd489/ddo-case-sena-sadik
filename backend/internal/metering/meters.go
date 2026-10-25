package metering

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
)

var (
	ErrNoActiveMeter = errors.New("subscriber has no active meter")
	ErrBadChange     = errors.New("invalid meter change")
)

type MeterChange struct {
	SubscriberID   int64     `json:"subscriber_id"`
	At             time.Time `json:"at"`             // 15 dk sınırı
	FinalIndexWh   int64     `json:"final_index_wh"` // eski sayacın kapanış endeksi
	FinalReactive  *int64    `json:"final_reactive_varh"`
	NewSerial      string    `json:"new_serial_no"`
	InitialIndexWh int64     `json:"initial_index_wh"` // yeni sayacın açılış endeksi
}

type Meter struct {
	ID             int64      `json:"id"`
	SerialNo       string     `json:"serial_no"`
	Kind           string     `json:"kind"`
	SubscriberID   *int64     `json:"subscriber_id,omitempty"`
	NodeID         *int64     `json:"node_id,omitempty"`
	InstalledAt    time.Time  `json:"installed_at"`
	RemovedAt      *time.Time `json:"removed_at,omitempty"`
	InitialIndexWh int64      `json:"initial_index_wh"`
	FinalIndexWh   *int64     `json:"final_index_wh,omitempty"`
}

// ReplaceMeter closes the subscriber's active meter at c.At with its final
// index and installs a new one starting at c.InitialIndexWh. Consumption on
// both sides of the change is computed per meter, so the old meter's high
// register and the new meter's low one never form a negative (or huge) delta.
// The affected meter-days are queued for recomputation.
func ReplaceMeter(ctx context.Context, tx pgx.Tx, c MeterChange) (*Meter, error) {
	if c.At.Unix()%900 != 0 || c.FinalIndexWh < 0 || c.InitialIndexWh < 0 || c.NewSerial == "" {
		return nil, ErrBadChange
	}
	var old Meter
	err := tx.QueryRow(ctx, `SELECT id, serial_no, installed_at FROM meters
		WHERE subscriber_id = $1 AND removed_at IS NULL AND kind = 'subscriber' FOR UPDATE`, c.SubscriberID).
		Scan(&old.ID, &old.SerialNo, &old.InstalledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoActiveMeter
	}
	if err != nil {
		return nil, err
	}
	if !c.At.After(old.InstalledAt) {
		return nil, ErrBadChange
	}
	// Kapanış endeksi, değişimden önceki son okumadan küçük olamaz.
	var last *int64
	if err := tx.QueryRow(ctx, `SELECT max(active_index_wh) FROM readings WHERE meter_id = $1 AND read_at <= $2 AND read_at > $2 - interval '31 days'`,
		old.ID, c.At).Scan(&last); err != nil {
		return nil, err
	}
	if last != nil && c.FinalIndexWh < *last {
		return nil, ErrBadChange
	}
	// Değişim anından sonra eski sayaca yazılmış okumalar yeni sayaca ait olamaz.
	if _, err := tx.Exec(ctx, `DELETE FROM readings WHERE meter_id = $1 AND read_at > $2`, old.ID, c.At); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE meters SET removed_at = $2, final_index_wh = $3, final_reactive_varh = $4 WHERE id = $1`,
		old.ID, c.At, c.FinalIndexWh, c.FinalReactive); err != nil {
		return nil, err
	}
	var m Meter
	err = tx.QueryRow(ctx, `INSERT INTO meters (serial_no, kind, subscriber_id, installed_at, initial_index_wh)
		VALUES ($1, 'subscriber', $2, $3, $4)
		RETURNING id, serial_no, kind, subscriber_id, node_id, installed_at, removed_at, initial_index_wh, final_index_wh`,
		c.NewSerial, c.SubscriberID, c.At, c.InitialIndexWh).
		Scan(&m.ID, &m.SerialNo, &m.Kind, &m.SubscriberID, &m.NodeID, &m.InstalledAt, &m.RemovedAt, &m.InitialIndexWh, &m.FinalIndexWh)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO consumption_dirty (meter_id, day)
		SELECT id, d::date FROM unnest(ARRAY[$1::bigint, $2::bigint]) id,
		     generate_series((($3::timestamptz - interval '15 minutes') AT TIME ZONE 'Europe/Istanbul')::date,
		                     ($3::timestamptz AT TIME ZONE 'Europe/Istanbul')::date, interval '1 day') d
		ON CONFLICT (meter_id, day) DO UPDATE SET marked_at = now()`, old.ID, m.ID, c.At); err != nil {
		return nil, err
	}
	err = audit.Record(ctx, tx, audit.Entry{Action: "meter.replace", Entity: "meter", EntityID: strconv.FormatInt(m.ID, 10),
		Old: map[string]any{"meter_id": old.ID, "serial_no": old.SerialNo},
		New: map[string]any{"meter_id": m.ID, "serial_no": m.SerialNo, "at": c.At, "final_index_wh": c.FinalIndexWh, "initial_index_wh": c.InitialIndexWh}})
	return &m, err
}
