// Package metrics is a tiny Prometheus text-exposition registry. The service
// has about a dozen series; pulling in client_golang for that would be the
// heaviest dependency in the binary.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Gauges are point-in-time values refreshed by the caller.
type Gauges struct {
	Feeds int
	Due   int
	Items int
}

var buckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30}

// Registry holds counters, one histogram and gauges.
type Registry struct {
	mu          sync.Mutex
	fetch       map[string]uint64
	http        map[string]uint64 // key: route\x00code
	stale       uint64
	histCounts  []uint64
	histSum     float64
	histCount   uint64
	gauges      Gauges
	gaugeUpdate func() Gauges
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{fetch: map[string]uint64{}, http: map[string]uint64{}, histCounts: make([]uint64, len(buckets))}
}

// IncFetch counts one fetch outcome.
func (r *Registry) IncFetch(result string) {
	r.mu.Lock()
	r.fetch[result]++
	r.mu.Unlock()
}

// ObserveFetchDuration records one fetch duration.
func (r *Registry) ObserveFetchDuration(d time.Duration) {
	s := d.Seconds()
	r.mu.Lock()
	for i, b := range buckets {
		if s <= b {
			r.histCounts[i]++
		}
	}
	r.histSum += s
	r.histCount++
	r.mu.Unlock()
}

// IncHTTP counts one served request.
func (r *Registry) IncHTTP(route string, code int) {
	r.mu.Lock()
	r.http[route+"\x00"+strconv.Itoa(code)]++
	r.mu.Unlock()
}

// IncStaleServe counts a response that served stale items.
func (r *Registry) IncStaleServe() {
	r.mu.Lock()
	r.stale++
	r.mu.Unlock()
}

// SetGauges stores the latest gauge values.
func (r *Registry) SetGauges(g Gauges) {
	r.mu.Lock()
	r.gauges = g
	r.mu.Unlock()
}

// GaugeSource registers a function called on every scrape to refresh gauges.
func (r *Registry) GaugeSource(f func() Gauges) {
	r.mu.Lock()
	r.gaugeUpdate = f
	r.mu.Unlock()
}

// ServeHTTP writes the exposition.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	if r.gaugeUpdate != nil {
		f := r.gaugeUpdate
		r.mu.Unlock()
		g := f()
		r.mu.Lock()
		r.gauges = g
	}
	defer r.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP feeds_fetch_total Feed fetch attempts by outcome.")
	fmt.Fprintln(w, "# TYPE feeds_fetch_total counter")
	for _, k := range sortedKeys(r.fetch) {
		fmt.Fprintf(w, "feeds_fetch_total{result=%q} %d\n", k, r.fetch[k])
	}
	fmt.Fprintln(w, "# HELP feeds_fetch_duration_seconds Feed fetch latency.")
	fmt.Fprintln(w, "# TYPE feeds_fetch_duration_seconds histogram")
	for i, b := range buckets {
		fmt.Fprintf(w, "feeds_fetch_duration_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(b, 'g', -1, 64), r.histCounts[i])
	}
	fmt.Fprintf(w, "feeds_fetch_duration_seconds_bucket{le=\"+Inf\"} %d\n", r.histCount)
	fmt.Fprintf(w, "feeds_fetch_duration_seconds_sum %s\n", strconv.FormatFloat(r.histSum, 'g', -1, 64))
	fmt.Fprintf(w, "feeds_fetch_duration_seconds_count %d\n", r.histCount)
	fmt.Fprintln(w, "# HELP feeds_http_requests_total Served requests by route and status.")
	fmt.Fprintln(w, "# TYPE feeds_http_requests_total counter")
	for _, k := range sortedKeys(r.http) {
		route, code := k[:indexNul(k)], k[indexNul(k)+1:]
		fmt.Fprintf(w, "feeds_http_requests_total{route=%q,code=%q} %d\n", route, code, r.http[k])
	}
	fmt.Fprintln(w, "# HELP feeds_stale_serves_total Responses that served cached items after a failed fetch.")
	fmt.Fprintln(w, "# TYPE feeds_stale_serves_total counter")
	fmt.Fprintf(w, "feeds_stale_serves_total %d\n", r.stale)
	fmt.Fprintln(w, "# TYPE feeds_feeds_total gauge")
	fmt.Fprintf(w, "feeds_feeds_total %d\n", r.gauges.Feeds)
	fmt.Fprintln(w, "# TYPE feeds_due_backlog gauge")
	fmt.Fprintf(w, "feeds_due_backlog %d\n", r.gauges.Due)
	fmt.Fprintln(w, "# TYPE feeds_items_total gauge")
	fmt.Fprintf(w, "feeds_items_total %d\n", r.gauges.Items)
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func indexNul(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return i
		}
	}
	return len(s)
}
