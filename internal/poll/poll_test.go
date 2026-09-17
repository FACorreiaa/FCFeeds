package poll

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FACorreiaa/FCFeeds/internal/fetch/fetchtest"
	"github.com/FACorreiaa/FCFeeds/internal/store"
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func cfg() Config {
	c := DefaultConfig()
	c.Tick = 10 * time.Millisecond
	c.HostGap = 0
	return c
}

func TestNextIntervalRules(t *testing.T) {
	c := DefaultConfig()
	cases := []struct {
		name         string
		publisherTTL time.Duration
		quiet        int
		want         time.Duration
	}{
		{"default when publisher silent", 0, 0, 15 * time.Minute},
		{"publisher ttl respected", 30 * time.Minute, 0, 30 * time.Minute},
		{"floor at 5m", time.Minute, 0, 5 * time.Minute},
		{"below threshold no growth", 0, 5, 15 * time.Minute},
		{"doubles after 6 quiet", 0, 6, 30 * time.Minute},
		{"doubles again after 12", 0, 12, 60 * time.Minute},
		{"capped at 2h", 0, 60, 2 * time.Hour},
	}
	for _, tc := range cases {
		if got := NextInterval(tc.publisherTTL, tc.quiet, c); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackoffRules(t *testing.T) {
	c := DefaultConfig()
	noJitter := func() float64 { return 0.5 }
	if got := Backoff(15*time.Minute, 1, 0, 500, c, noJitter); got != 30*time.Minute {
		t.Errorf("fail 1: %v", got)
	}
	if got := Backoff(15*time.Minute, 3, 0, 500, c, noJitter); got != 2*time.Hour {
		t.Errorf("fail 3: %v", got)
	}
	if got := Backoff(15*time.Minute, 10, 0, 500, c, noJitter); got != 6*time.Hour {
		t.Errorf("cap: %v", got)
	}
	if got := Backoff(15*time.Minute, 1, 3*time.Hour, 429, c, noJitter); got != 3*time.Hour {
		t.Errorf("retry-after wins when larger: %v", got)
	}
	if got := Backoff(15*time.Minute, 5, 0, 404, c, noJitter); got != 24*time.Hour {
		t.Errorf("gone x5 pins to 24h: %v", got)
	}
	if got := Backoff(15*time.Minute, 4, 0, 410, c, noJitter); got == 24*time.Hour {
		t.Errorf("gone x4 not yet pinned")
	}
	lo := Backoff(15*time.Minute, 1, 0, 500, c, func() float64 { return 0 })
	hi := Backoff(15*time.Minute, 1, 0, 500, c, func() float64 { return 1 })
	if lo >= 30*time.Minute || hi <= 30*time.Minute || hi-lo > 6*time.Minute+time.Second {
		t.Errorf("jitter ±10%%: lo=%v hi=%v", lo, hi)
	}
}

const rssBody = `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><link>https://s/</link><ttl>20</ttl>
<item><title>A</title><link>https://s/a</link><guid>a</guid><pubDate>Wed, 17 Sep 2026 11:00:00 GMT</pubDate></item>
<item><title>B</title><link>https://s/b</link><guid>b</guid><pubDate>Wed, 17 Sep 2026 10:00:00 GMT</pubDate></item>
</channel></rss>`

func newPoller(t *testing.T, srv *httptest.Server) (*Poller, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p := New(st, fetchtest.NewLoopbackClient(t, srv, nil), cfg())
	p.Now = func() time.Time { return now }
	p.Rand = func() float64 { return 0.5 }
	return p, st
}

func TestFetchOneStoresItemsAndState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte(rssBody))
	}))
	defer srv.Close()
	p, st := newPoller(t, srv)
	ctx := context.Background()
	f, _, _ := st.EnsureFeed(ctx, srv.URL+"/rss", now)
	due, _ := st.ClaimDue(ctx, now, time.Minute, 10)
	out := p.FetchOne(ctx, due[0])
	if out.Err != nil || out.NewItems != 2 || out.Result != ResultOK {
		t.Fatalf("outcome = %+v", out)
	}
	items, _ := st.ListItems(ctx, []int64{f.ID}, time.Time{}, 10)
	if len(items) != 2 || items[0].Title != "A" {
		t.Errorf("items = %+v", items)
	}
	s, _ := st.FetchState(ctx, f.ID)
	if s.ETag != `"e1"` || s.FailCount != 0 || s.LockedUntil != nil {
		t.Errorf("state = %+v", s)
	}
	if !s.NextPollAt.Equal(now.Add(20 * time.Minute)) {
		t.Errorf("publisher ttl 20m should schedule next poll at +20m, got %v", s.NextPollAt)
	}
	g, _ := st.FeedByURL(ctx, srv.URL+"/rss")
	if g.Title != "T" || g.Kind != "rss" || g.PollInterval != 20*time.Minute {
		t.Errorf("feed meta = %+v", g)
	}
}

func TestFetchOneNotModifiedIsQuiet(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("ETag", `"e1"`)
			_, _ = w.Write([]byte(rssBody))
			return
		}
		if r.Header.Get("If-None-Match") != `"e1"` {
			t.Errorf("etag not sent on second fetch")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	p, st := newPoller(t, srv)
	ctx := context.Background()
	f, _, _ := st.EnsureFeed(ctx, srv.URL+"/rss", now)
	due, _ := st.ClaimDue(ctx, now, time.Minute, 10)
	_ = p.FetchOne(ctx, due[0])
	later := now.Add(time.Hour)
	p.Now = func() time.Time { return later }
	due, _ = st.ClaimDue(ctx, later, time.Minute, 10)
	out := p.FetchOne(ctx, due[0])
	if out.Result != ResultNotModified || out.NewItems != 0 {
		t.Errorf("outcome = %+v", out)
	}
	s, _ := st.FetchState(ctx, f.ID)
	if s.QuietPolls != 1 || s.LastStatus != 304 {
		t.Errorf("state = %+v", s)
	}
}

func TestFetchOneServerErrorBacksOffKeepingItems(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(rssBody))
	}))
	defer srv.Close()
	p, st := newPoller(t, srv)
	ctx := context.Background()
	f, _, _ := st.EnsureFeed(ctx, srv.URL+"/rss", now)
	due, _ := st.ClaimDue(ctx, now, time.Minute, 10)
	_ = p.FetchOne(ctx, due[0])
	fail.Store(true)
	later := now.Add(time.Hour)
	p.Now = func() time.Time { return later }
	due, _ = st.ClaimDue(ctx, later, time.Minute, 10)
	out := p.FetchOne(ctx, due[0])
	if out.Result != ResultError || out.Err == nil {
		t.Errorf("outcome = %+v", out)
	}
	s, _ := st.FetchState(ctx, f.ID)
	if s.FailCount != 1 || s.LastStatus != 502 || s.LastOKAt == nil {
		t.Errorf("state = %+v", s)
	}
	// base interval 20m from the publisher, fail 1 → 40m, jitter 0.5 → exactly 40m
	if !s.NextPollAt.Equal(later.Add(40 * time.Minute)) {
		t.Errorf("next poll = %v, want +40m", s.NextPollAt)
	}
	items, _ := st.ListItems(ctx, []int64{f.ID}, time.Time{}, 10)
	if len(items) != 2 {
		t.Errorf("cached items must survive an error: %d", len(items))
	}
}

func TestFetchOneHTMLIsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html>nope</html>"))
	}))
	defer srv.Close()
	p, st := newPoller(t, srv)
	ctx := context.Background()
	f, _, _ := st.EnsureFeed(ctx, srv.URL+"/", now)
	due, _ := st.ClaimDue(ctx, now, time.Minute, 10)
	out := p.FetchOne(ctx, due[0])
	if out.Result != ResultParseError {
		t.Errorf("outcome = %+v", out)
	}
	s, _ := st.FetchState(ctx, f.ID)
	if s.FailCount != 1 || s.LastError == "" {
		t.Errorf("state = %+v", s)
	}
}

func TestRunLoopPollsDueFeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rssBody))
	}))
	defer srv.Close()
	p, st := newPoller(t, srv)
	p.Now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	f, _, _ := st.EnsureFeed(ctx, srv.URL+"/rss", time.Now())
	go p.Run(ctx)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		items, _ := st.ListItems(ctx, []int64{f.ID}, time.Time{}, 10)
		if len(items) == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run loop never fetched the due feed")
}

func TestHostLimiterSerializesPerHost(t *testing.T) {
	l := newHostLimiter(30 * time.Millisecond)
	start := time.Now()
	rel := l.acquire("a.example")
	rel()
	rel = l.acquire("a.example")
	rel()
	if el := time.Since(start); el < 30*time.Millisecond {
		t.Errorf("second request to same host should wait for the gap, took %v", el)
	}
	start = time.Now()
	rel = l.acquire("b.example")
	rel()
	if el := time.Since(start); el > 20*time.Millisecond {
		t.Errorf("different host should not wait, took %v", el)
	}
}

func TestFetchNowFetchesImmediatelyAndDedupes(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(rssBody))
	}))
	defer srv.Close()
	p, st := newPoller(t, srv)
	ctx := context.Background()
	f, _, _ := st.EnsureFeed(ctx, srv.URL+"/rss", now)
	done := make(chan Outcome, 1)
	go func() { done <- p.FetchNow(ctx, f, time.Second) }()
	time.Sleep(50 * time.Millisecond)
	second := p.FetchNow(ctx, f, time.Second)
	if second.Result != ResultInFlight {
		t.Errorf("concurrent FetchNow should report in-flight, got %+v", second)
	}
	close(release)
	first := <-done
	if first.Result != ResultOK || first.NewItems != 2 {
		t.Errorf("first = %+v", first)
	}
	s, _ := st.FetchState(ctx, f.ID)
	if s.LockedUntil != nil {
		t.Error("lock should be released after fetch")
	}
}
