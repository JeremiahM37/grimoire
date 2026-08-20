// Package metrics reports what the server already knows.
//
// Until now the only evidence a running instance produced was a log line at
// startup and the secret vault's audit trail. "Why was that query slow", "what
// did the connector do at 03:00", "is the cache being rebuilt on every write
// again" were all unanswerable without attaching to the process — which is a
// bad position to be in during the first incident, and the reason this is the
// first thing to add after correctness.
//
// Deliberately dependency-free, and deliberately about things this server can
// answer from what it is already doing: no sampling, no collectors, no agent.
// The numbers here are counters the code increments as it works, exposed in the
// Prometheus text format because that is what scrapes it.
package metrics

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Registry holds every metric. One per process; the zero value is unusable, so
// use New.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*counter
	gauges     map[string]*gauge
	histograms map[string]*histogram
	start      time.Time
}

func New() *Registry {
	return &Registry{
		counters:   map[string]*counter{},
		gauges:     map[string]*gauge{},
		histograms: map[string]*histogram{},
		start:      time.Now(),
	}
}

// Default is the process-wide registry. A package-level default is the wrong
// choice for a library and the right one here: instrumentation that has to be
// threaded through every constructor does not get added.
var Default = New()

type series struct {
	name   string
	help   string
	labels map[string]string
}

func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+labels[k])
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

type counter struct {
	series
	v atomic.Int64
}

type gauge struct {
	series
	fn func() float64
}

// histogram is a fixed-bucket latency histogram. Fixed rather than configurable
// because every latency in this server is a request or a query, and buckets
// from a millisecond to ten seconds cover both.
type histogram struct {
	series
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	total   uint64
}

var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Count increments a counter.
func (r *Registry) Count(name, help string, labels map[string]string, delta int64) {
	k := key(name, labels)
	r.mu.RLock()
	c := r.counters[k]
	r.mu.RUnlock()
	if c == nil {
		r.mu.Lock()
		if c = r.counters[k]; c == nil {
			c = &counter{series: series{name: name, help: help, labels: labels}}
			r.counters[k] = c
		}
		r.mu.Unlock()
	}
	c.v.Add(delta)
}

// Observe records a duration in seconds.
func (r *Registry) Observe(name, help string, labels map[string]string, seconds float64) {
	k := key(name, labels)
	r.mu.RLock()
	h := r.histograms[k]
	r.mu.RUnlock()
	if h == nil {
		r.mu.Lock()
		if h = r.histograms[k]; h == nil {
			h = &histogram{
				series:  series{name: name, help: help, labels: labels},
				buckets: latencyBuckets,
				counts:  make([]uint64, len(latencyBuckets)),
			}
			r.histograms[k] = h
		}
		r.mu.Unlock()
	}
	h.mu.Lock()
	h.sum += seconds
	h.total++
	for i, b := range h.buckets {
		if seconds <= b {
			h.counts[i]++
		}
	}
	h.mu.Unlock()
}

// Gauge registers a value read at scrape time — corpus size, cache residency,
// whether the vault is unlocked. Read at scrape rather than pushed, so a gauge
// cannot go stale by someone forgetting to update it.
func (r *Registry) Gauge(name, help string, labels map[string]string, fn func() float64) {
	k := key(name, labels)
	r.mu.Lock()
	r.gauges[k] = &gauge{series: series{name: name, help: help, labels: labels}, fn: fn}
	r.mu.Unlock()
}

// Timer starts a duration measurement.
func Timer() func(name, help string, labels map[string]string) {
	start := time.Now()
	return func(name, help string, labels map[string]string) {
		Default.Observe(name, help, labels, time.Since(start).Seconds())
	}
}

// Write renders the registry in the Prometheus text exposition format.
func (r *Registry) Write(w *strings.Builder) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	helps := map[string]string{}
	lines := map[string][]string{}

	add := func(s series, rendered string) {
		if s.help != "" {
			helps[s.name] = s.help
		}
		lines[s.name] = append(lines[s.name], rendered)
	}

	for _, c := range r.counters {
		add(c.series, fmt.Sprintf("%s %d", labelled(c.name, c.labels), c.v.Load()))
	}
	for _, g := range r.gauges {
		v := g.fn()
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		add(g.series, fmt.Sprintf("%s %g", labelled(g.name, g.labels), v))
	}
	for _, h := range r.histograms {
		h.mu.Lock()
		for i, b := range h.buckets {
			l := maps.Clone(h.labels)
			if l == nil {
				l = map[string]string{}
			}
			l["le"] = fmt.Sprint(b)
			add(h.series, fmt.Sprintf("%s %d", labelled(h.name+"_bucket", l), h.counts[i]))
		}
		inf := maps.Clone(h.labels)
		if inf == nil {
			inf = map[string]string{}
		}
		inf["le"] = "+Inf"
		add(h.series, fmt.Sprintf("%s %d", labelled(h.name+"_bucket", inf), h.total))
		add(h.series, fmt.Sprintf("%s %g", labelled(h.name+"_sum", h.labels), h.sum))
		add(h.series, fmt.Sprintf("%s %d", labelled(h.name+"_count", h.labels), h.total))
		h.mu.Unlock()
	}

	names := make([]string, 0, len(lines))
	for n := range lines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if help := helps[n]; help != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", n, help)
		}
		kind := "counter"
		switch {
		case strings.HasSuffix(n, "_seconds"):
			kind = "histogram"
		case strings.Contains(n, "_current") || strings.HasSuffix(n, "_bytes") ||
			strings.HasSuffix(n, "_total_notes") || strings.HasSuffix(n, "_info"):
			kind = "gauge"
		}
		fmt.Fprintf(w, "# TYPE %s %s\n", n, kind)
		sorted := lines[n]
		sort.Strings(sorted)
		for _, l := range sorted {
			fmt.Fprintln(w, l)
		}
	}
	fmt.Fprintf(w, "# HELP grimoire_uptime_seconds How long this process has been serving.\n")
	fmt.Fprintf(w, "# TYPE grimoire_uptime_seconds gauge\ngrimoire_uptime_seconds %g\n",
		time.Since(r.start).Seconds())
}

func labelled(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Convenience wrappers over the default registry, so instrumenting a call site
// is one line and does not need the registry threaded to it.

func Count(name, help string, labels map[string]string) {
	Default.Count(name, help, labels, 1)
}

func Add(name, help string, labels map[string]string, delta int64) {
	Default.Count(name, help, labels, delta)
}

func Observe(name, help string, labels map[string]string, seconds float64) {
	Default.Observe(name, help, labels, seconds)
}

func Gauge(name, help string, labels map[string]string, fn func() float64) {
	Default.Gauge(name, help, labels, fn)
}
