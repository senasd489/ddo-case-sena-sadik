package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusExposition(t *testing.T) {
	c := NewCounter("test_counter_total", "t", "a")
	c.Inc("x")
	c.Add(2, "x")
	h := NewHistogram("test_hist_seconds", "t", []float64{0.1, 1}, "r")
	h.Observe(0.05, "/a")
	h.Observe(0.5, "/a")
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`test_counter_total{a="x"} 3`,
		`test_hist_seconds_bucket{r="/a",le="0.1"} 1`,
		`test_hist_seconds_bucket{r="/a",le="1"} 2`,
		`test_hist_seconds_bucket{r="/a",le="+Inf"} 2`,
		`test_hist_seconds_count{r="/a"} 2`,
		"# TYPE enerjihat_uptime_seconds gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if c.Value("x") != 3 {
		t.Error("Value")
	}
}
