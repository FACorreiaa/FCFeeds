package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExposition(t *testing.T) {
	r := New()
	r.IncFetch("ok")
	r.IncFetch("ok")
	r.IncFetch("error")
	r.ObserveFetchDuration(300 * time.Millisecond)
	r.ObserveFetchDuration(3 * time.Second)
	r.IncHTTP("/v1/items", 200)
	r.IncHTTP("/v1/items", 400)
	r.IncStaleServe()
	r.SetGauges(Gauges{Feeds: 3, Due: 1, Items: 42})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	want := []string{
		`feeds_fetch_total{result="ok"} 2`,
		`feeds_fetch_total{result="error"} 1`,
		`feeds_fetch_duration_seconds_bucket{le="0.5"} 1`,
		`feeds_fetch_duration_seconds_bucket{le="5"} 2`,
		`feeds_fetch_duration_seconds_bucket{le="+Inf"} 2`,
		`feeds_fetch_duration_seconds_count 2`,
		`feeds_http_requests_total{route="/v1/items",code="200"} 1`,
		`feeds_http_requests_total{route="/v1/items",code="400"} 1`,
		`feeds_stale_serves_total 1`,
		`feeds_feeds_total 3`,
		`feeds_due_backlog 1`,
		`feeds_items_total 42`,
		`# TYPE feeds_fetch_total counter`,
		`# TYPE feeds_fetch_duration_seconds histogram`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in:\n%s", w, body)
		}
	}
	if !strings.Contains(body, "feeds_fetch_duration_seconds_sum 3.3") {
		t.Errorf("sum: %s", body)
	}
}
