// Package metrics es un registro mínimo de métricas en formato de exposición de Prometheus
// (contadores, gauges e histogramas con etiquetas). Evita depender de client_golang.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Registry agrupa métricas.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	write(w io.Writer)
	name() string
}

// Default es el registro global.
var Default = &Registry{}

func (r *Registry) add(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, m)
}

// Handler sirve /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.mu.Lock()
		ms := append([]metric(nil), r.metrics...)
		r.mu.Unlock()
		sort.Slice(ms, func(i, j int) bool { return ms[i].name() < ms[j].name() })
		for _, m := range ms {
			m.write(w)
		}
	})
}

func labelStr(names, values []string, extra ...string) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, n, escape(values[i])))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, extra[i], extra[i+1]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func fmtFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return fmt.Sprintf("%g", v)
}

// vec es la base de las métricas con etiquetas.
type vec struct {
	mu     sync.Mutex
	n      string
	help   string
	typ    string
	labels []string
	vals   map[string]*float64
	keys   map[string][]string
}

func newVec(n, help, typ string, labels []string) *vec {
	return &vec{n: n, help: help, typ: typ, labels: labels, vals: map[string]*float64{}, keys: map[string][]string{}}
}

func (v *vec) name() string { return v.n }

func (v *vec) ptr(values []string) *float64 {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("métrica %s: se esperaban %d etiquetas", v.n, len(v.labels)))
	}
	k := strings.Join(values, "\x00")
	p := v.vals[k]
	if p == nil {
		p = new(float64)
		v.vals[k] = p
		v.keys[k] = append([]string(nil), values...)
	}
	return p
}

func (v *vec) write(w io.Writer) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.n, v.help, v.n, v.typ)
	keys := make([]string, 0, len(v.vals))
	for k := range v.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", v.n, labelStr(v.labels, v.keys[k]), fmtFloat(*v.vals[k]))
	}
}

// CounterVec es un contador con etiquetas.
type CounterVec struct{ *vec }

// NewCounter registra un contador.
func (r *Registry) NewCounter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{newVec(name, help, "counter", labels)}
	r.add(c)
	return c
}

// Add suma delta (>= 0).
func (c *CounterVec) Add(delta float64, labels ...string) {
	c.mu.Lock()
	*c.ptr(labels) += delta
	c.mu.Unlock()
}

// Inc suma 1.
func (c *CounterVec) Inc(labels ...string) { c.Add(1, labels...) }

// GaugeVec es un gauge con etiquetas.
type GaugeVec struct{ *vec }

// NewGauge registra un gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *GaugeVec {
	g := &GaugeVec{newVec(name, help, "gauge", labels)}
	r.add(g)
	return g
}

// Set fija el valor.
func (g *GaugeVec) Set(v float64, labels ...string) {
	g.mu.Lock()
	*g.ptr(labels) = v
	g.mu.Unlock()
}

// Reset borra todas las series del gauge (para recuentos que cambian de etiquetas).
func (g *GaugeVec) Reset() {
	g.mu.Lock()
	g.vals = map[string]*float64{}
	g.keys = map[string][]string{}
	g.mu.Unlock()
}

// HistogramVec es un histograma con etiquetas.
type HistogramVec struct {
	mu      sync.Mutex
	n, help string
	labels  []string
	buckets []float64
	data    map[string]*hdata
}

type hdata struct {
	values []string
	counts []uint64
	sum    float64
	count  uint64
}

// NewHistogram registra un histograma.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	h := &HistogramVec{n: name, help: help, labels: labels, buckets: buckets, data: map[string]*hdata{}}
	r.add(h)
	return h
}

func (h *HistogramVec) name() string { return h.n }

// Observe registra una observación.
func (h *HistogramVec) Observe(v float64, labels ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := strings.Join(labels, "\x00")
	d := h.data[k]
	if d == nil {
		d = &hdata{values: append([]string(nil), labels...), counts: make([]uint64, len(h.buckets))}
		h.data[k] = d
	}
	for i, b := range h.buckets {
		if v <= b {
			d.counts[i]++
		}
	}
	d.sum += v
	d.count++
}

func (h *HistogramVec) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.n, h.help, h.n)
	keys := make([]string, 0, len(h.data))
	for k := range h.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := h.data[k]
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.n, labelStr(h.labels, d.values, "le", fmtFloat(b)), d.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.n, labelStr(h.labels, d.values, "le", "+Inf"), d.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.n, labelStr(h.labels, d.values), fmtFloat(d.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.n, labelStr(h.labels, d.values), d.count)
	}
}

// DefBuckets son los límites por defecto (segundos).
var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}
