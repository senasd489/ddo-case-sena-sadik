package httpx

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

// Page is the keyset-pagination request: ?limit=50&cursor=<opaque>.
// The cursor encodes the sort key(s) of the last row of the previous page,
// so the next query is "WHERE (k1,k2) > (cursor) ORDER BY k1,k2 LIMIT n" —
// an index range scan whose cost does not grow with the page number.
type Page struct {
	Limit  int
	Cursor []string // decoded key parts, empty on the first page
}

func ParsePage(r *http.Request, def, max int) (Page, error) {
	p := Page{Limit: def}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > max {
			return p, BadRequest("limit 1-" + strconv.Itoa(max) + " arasında olmalı")
		}
		p.Limit = n
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil {
			return p, BadRequest("geçersiz cursor")
		}
		p.Cursor = strings.Split(string(b), "|")
	}
	return p, nil
}

// CursorInt64 returns the i-th cursor part as int64 (0 when absent).
func (p Page) CursorInt64(i int) (int64, error) {
	if i >= len(p.Cursor) {
		return 0, nil
	}
	n, err := strconv.ParseInt(p.Cursor[i], 10, 64)
	if err != nil {
		return 0, BadRequest("geçersiz cursor")
	}
	return n, nil
}

func EncodeCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "|")))
}

// List is the standard list envelope.
type List[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// NewList trims the extra (limit+1-th) row used to detect a next page.
func NewList[T any](items []T, limit int, cursor func(T) string) List[T] {
	if items == nil {
		items = []T{}
	}
	l := List[T]{Items: items}
	if len(items) > limit {
		l.Items = items[:limit]
		c := cursor(items[limit-1])
		l.NextCursor = &c
	}
	return l
}
