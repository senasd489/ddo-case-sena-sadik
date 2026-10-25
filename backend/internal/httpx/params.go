package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// Istanbul is the business time zone (no DST since 2016: fixed UTC+3).
var Istanbul = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return l
	}
	return time.FixedZone("Europe/Istanbul", 3*3600)
}()

func PathInt64(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n <= 0 {
		return 0, BadRequest(name + " geçersiz")
	}
	return n, nil
}

// ParseTime accepts RFC3339 or a YYYY-MM-DD date (local midnight).
func ParseTime(v string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if d, err := time.ParseInLocation("2006-01-02", v, Istanbul); err == nil {
		return d, true
	}
	return time.Time{}, false
}

// ParseRange reads ?from=&to= (default: last `def` until now), max span maxSpan.
func ParseRange(r *http.Request, def, maxSpan time.Duration) (time.Time, time.Time, error) {
	to := time.Now()
	from := to.Add(-def)
	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		t, ok := ParseTime(v)
		if !ok {
			return from, to, BadRequest("from: RFC3339 veya YYYY-MM-DD olmalı")
		}
		from = t
		if q.Get("to") == "" {
			to = from.Add(def)
		}
	}
	if v := q.Get("to"); v != "" {
		t, ok := ParseTime(v)
		if !ok {
			return from, to, BadRequest("to: RFC3339 veya YYYY-MM-DD olmalı")
		}
		to = t
	}
	if !to.After(from) {
		return from, to, BadRequest("to, from'dan sonra olmalı")
	}
	if to.Sub(from) > maxSpan {
		return from, to, BadRequest("aralık çok uzun (en fazla " + maxSpan.String() + ")")
	}
	return from, to, nil
}
