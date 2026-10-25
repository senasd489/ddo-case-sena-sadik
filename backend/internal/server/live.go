package server

import (
	"net/http"
	"time"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

// live is the "is the system alive" summary shown in the UI header.
func (s *Server) live(w http.ResponseWriter, r *http.Request) error {
	var out struct {
		Now              time.Time  `json:"now"`
		LastBatchAt      *time.Time `json:"last_batch_at"`
		LastReadAt       *time.Time `json:"last_read_at"`
		ReadingsLastHour int64      `json:"readings_last_hour"`
		BatchesLastHour  int64      `json:"batches_last_hour"`
		SignalsLastHour  int64      `json:"signals_last_hour"`
		OpenOutages      int64      `json:"open_outages"`
		InterruptedSubs  int64      `json:"interrupted_subscribers"`
		BusyCrews        int64      `json:"busy_crews"`
		OpenSuspects     int64      `json:"open_suspects"`
		DirtyQueue       int64      `json:"consumption_queue"`
	}
	err := s.pool.QueryRow(r.Context(), `SELECT now(),
		(SELECT max(created_at) FROM ingest_batches),
		(SELECT max(max_read_at) FROM ingest_batches),
		(SELECT coalesce(sum(inserted + corrections), 0) FROM ingest_batches WHERE created_at > now() - interval '1 hour'),
		(SELECT count(*) FROM ingest_batches WHERE created_at > now() - interval '1 hour'),
		(SELECT count(*) FROM outage_signals WHERE created_at > now() - interval '1 hour'),
		(SELECT count(*) FROM outages WHERE status IN ('detected','confirmed','assigned','in_progress')),
		(SELECT count(*) FROM outage_affected a JOIN outages o ON o.id = a.outage_id
		  WHERE a.restored_at IS NULL AND o.status IN ('detected','confirmed','assigned','in_progress')),
		(SELECT count(*) FROM crews WHERE status = 'busy'),
		(SELECT count(*) FROM suspicion_flags WHERE status = 'open'),
		(SELECT count(*) FROM consumption_dirty)`).
		Scan(&out.Now, &out.LastBatchAt, &out.LastReadAt, &out.ReadingsLastHour, &out.BatchesLastHour, &out.SignalsLastHour,
			&out.OpenOutages, &out.InterruptedSubs, &out.BusyCrews, &out.OpenSuspects, &out.DirtyQueue)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}
