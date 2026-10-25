package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	H(func(http.ResponseWriter, *http.Request) error { return ErrForbidden })(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "forbidden" || body.Error.Message == "" {
		t.Fatalf("bad envelope: %s", rec.Body)
	}
}

func TestInternalErrorsAreHidden(t *testing.T) {
	rec := httptest.NewRecorder()
	H(func(http.ResponseWriter, *http.Request) error { return errors.New("pq: secret detail") })(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("internal error leaked: %d %s", rec.Code, rec.Body)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"a":1,"evil":2}`))
	var dst struct {
		A int `json:"a"`
	}
	if err := Decode(req, &dst); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestPageParsingAndList(t *testing.T) {
	r := httptest.NewRequest("GET", "/?limit=2&cursor="+EncodeCursor("10", "x"), nil)
	p, err := ParsePage(r, 50, 100)
	if err != nil || p.Limit != 2 {
		t.Fatalf("parse: %v %+v", err, p)
	}
	if n, _ := p.CursorInt64(0); n != 10 {
		t.Fatalf("cursor part = %d", n)
	}
	if _, err := ParsePage(httptest.NewRequest("GET", "/?limit=1000", nil), 50, 100); err == nil {
		t.Fatal("limit over max accepted")
	}
	l := NewList([]int{1, 2, 3}, 2, func(i int) string { return EncodeCursor("c") })
	if len(l.Items) != 2 || l.NextCursor == nil {
		t.Fatalf("list trimming wrong: %+v", l)
	}
	l = NewList([]int{1}, 2, func(i int) string { return "" })
	if l.NextCursor != nil {
		t.Fatal("unexpected next cursor")
	}
}

func TestRecoverAndRequestID(t *testing.T) {
	h := Logging(nil2())(Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 500 || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("recover/request id: %d %v", rec.Code, rec.Header())
	}
}
