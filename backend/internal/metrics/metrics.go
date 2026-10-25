// Package metrics is a small, dependency-free Prometheus text-format exporter.
// It supports labelled counters, histograms and callback gauges, which is all
// this service needs (see DECISIONS.md, ADR on dependencies).
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type metric interface {
	write(w io.Writer)
}

var (
	regMu    sync.Mutex
	registry []metric
	started  = time.Now()
)

func register(m metric) {
	regMu.Lock()
	registry = append(registry, m)
	regMu.Unlock()
}

func labelString(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts = append(parts, fmt.Sprintf(`%s=%q`, n, v))
	}
	parts = append(parts, extra...)
	return "{" + strings.Join(parts, ",") + "}"
}

// CounterVec is a monotonically increasing counter keyed by label values.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	vals       map[string]float64
	keys       map[string][]string
}

func NewCounter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, vals: map[string]float64{}, keys: map[string][]string{}}
	register(c)
	return c
}

func (c *CounterVec) Add(v float64, lv ...string) {
	k := strings.Join(lv, "\xff")
	c.mu.Lock()
	c.vals[k] += v
	if _, ok := c.keys[k]; !ok {
		c.keys[k] = append([]string(nil), lv...)
	}
	c.mu.Unlock()
}

func (c *CounterVec) Inc(lv ...string) { c.Add(1, lv...) }

// Value returns the current value (used in tests).
func (c *CounterVec) Value(lv ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vals[strings.Join(lv, "\xff")]
}

func (c *CounterVec) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	ks := make([]string, 0, len(c.vals))
	for k := range c.vals {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Fprintf(w, "%s%s %g\n", c.name, labelString(c.labels, c.keys[k]), c.vals[k])
	}
}

// HistogramVec records observations into cumulative buckets.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histSeries
}

type histSeries struct {
	lv     []string
	counts []uint64
	sum    float64
	count  uint64
}

func NewHistogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histSeries{}}
	register(h)
	return h
}

func (h *HistogramVec) Observe(v float64, lv ...string) {
	k := strings.Join(lv, "\xff")
	h.mu.Lock()
	s, ok := h.series[k]
	if !ok {
		s = &histSeries{lv: append([]string(nil), lv...), counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
	h.mu.Unlock()
}

func (h *HistogramVec) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	ks := make([]string, 0, len(h.series))
	for k := range h.series {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		s := h.series[k]
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labels, s.lv, fmt.Sprintf(`le="%g"`, b)), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labels, s.lv, `le="+Inf"`), s.count)
		fmt.Fprintf(w, "%s_sum%s %g\n", h.name, labelString(h.labels, s.lv), s.sum)
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelString(h.labels, s.lv), s.count)
	}
}

// GaugeFunc reports a value computed at scrape time.
type GaugeFunc struct {
	name, help string
	fn         func() float64
}

func NewGaugeFunc(name, help string, fn func() float64) *GaugeFunc {
	g := &GaugeFunc{name: name, help: help, fn: fn}
	register(g)
	return g
}

func (g *GaugeFunc) write(w io.Writer) {
	v := g.fn()
	if math.IsNaN(v) {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, v)
}

var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Shared application metrics.
var (
	HTTPRequests = NewCounter("enerjihat_http_requests_total", "HTTP istek sayısı", "method", "route", "status")
	HTTPDuration = NewHistogram("enerjihat_http_request_duration_seconds", "HTTP istek süresi", DefBuckets, "method", "route")
	DomainEvents = NewCounter("enerjihat_domain_events_total", "Alan olayları (okuma, fatura, kesinti…)", "kind")
)

func init() {
	NewGaugeFunc("enerjihat_uptime_seconds", "Süreç çalışma süresi", func() float64 { return time.Since(started).Seconds() })
	NewGaugeFunc("go_goroutines", "Goroutine sayısı", func() float64 { return float64(runtime.NumGoroutine()) })
	NewGaugeFunc("go_memstats_heap_alloc_bytes", "Heap kullanımı", func() float64 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc)
	})
}

// Handler serves every registered metric in Prometheus text format 0.0.4.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		regMu.Lock()
		ms := append([]metric(nil), registry...)
		regMu.Unlock()
		for _, m := range ms {
			m.write(w)
		}
	})
}
