package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FACorreiaa/feeds/internal/discover"
	"github.com/FACorreiaa/feeds/internal/fetch/fetchtest"
	"github.com/FACorreiaa/feeds/internal/metrics"
	"github.com/FACorreiaa/feeds/internal/poll"
	"github.com/FACorreiaa/feeds/internal/store"
)

const rssA = `<?xml version="1.0"?><rss version="2.0"><channel><title>Feed A</title><link>https://a.example/</link>
<item><title>Shared story</title><link>https://pub.example/story?utm_source=a</link><guid>a1</guid><pubDate>Wed, 17 Sep 2026 11:00:00 GMT</pubDate><source url="https://pub.example">Pub</source></item>
<item><title>Only in A</title><link>https://a.example/2</link><guid>a2</guid><pubDate>Wed, 17 Sep 2026 09:00:00 GMT</pubDate></item>
</channel></rss>`

const rssB = `<?xml version="1.0"?><rss version="2.0"><channel><title>Feed B</title><link>https://b.example/</link>
<item><title>Shared story</title><link>https://pub.example/story?utm_source=b</link><guid>b1</guid><pubDate>Wed, 17 Sep 2026 10:30:00 GMT</pubDate></item>
<item><title>Only in B</title><link>https://b.example/2</link><guid>b2</guid><pubDate>Wed, 17 Sep 2026 10:00:00 GMT</pubDate></item>
</channel></rss>`

type upstream struct {
	srv   *httptest.Server
	failA atomic.Bool
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.xml":
			if u.failA.Load() {
				w.WriteHeader(500)
				return
			}
			w.Header().Set("ETag", `"a"`)
			_, _ = w.Write([]byte(rssA))
		case "/b.xml":
			_, _ = w.Write([]byte(rssB))
		case "/site/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><link rel="alternate" type="application/rss+xml" href="/a.xml"></head></html>`))
		case "/broken":
			w.WriteHeader(500)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

type harness struct {
	up     *upstream
	store  *store.Store
	poller *poll.Poller
	api    http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	up := newUpstream(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	client := fetchtest.NewLoopbackClient(t, up.srv, map[string][]string{"evil.example": {"10.0.0.1"}})
	pc := poll.DefaultConfig()
	pc.HostGap = 0
	p := poll.New(st, client, pc)
	p.Rand = func() float64 { return 0.5 }
	srv := New(st, p, discover.New(client), metrics.New(), Options{ColdFetchBudget: 2 * time.Second})
	return &harness{up: up, store: st, poller: p, api: srv.Handler()}
}

func (h *harness) do(t *testing.T, method, path string, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (h *harness) pollAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	due, err := h.store.ClaimDue(ctx, time.Now().Add(365*24*time.Hour), time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		h.poller.FetchOne(ctx, d)
	}
}

func TestGetFeedColdFetchReturnsJSONFeed(t *testing.T) {
	h := newHarness(t)
	res, body := h.do(t, "GET", "/v1/feeds?url="+h.up.srv.URL+"/a.xml", "", nil)
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/feed+json") {
		t.Errorf("content-type = %q", ct)
	}
	if res.Header.Get("ETag") == "" {
		t.Error("missing ETag")
	}
	var f JSONFeed
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	if f.Version != "https://jsonfeed.org/version/1.1" || f.Title != "Feed A" || f.HomePageURL != "https://a.example/" || f.FeedURL != h.up.srv.URL+"/a.xml" {
		t.Errorf("header: %+v", f)
	}
	if len(f.Items) != 2 || f.Items[0].Title != "Shared story" || f.Items[0].URL != "https://pub.example/story?utm_source=a" {
		t.Errorf("items: %+v", f.Items)
	}
	if f.Items[0].Ext.SourceName != "Pub" || f.Items[0].Ext.SourceURL != "https://pub.example" || f.Items[0].Ext.FeedURL != h.up.srv.URL+"/a.xml" {
		t.Errorf("item ext: %+v", f.Items[0].Ext)
	}
	if f.Items[1].Ext.SourceName != "Feed A" {
		t.Errorf("item without <source> should inherit feed title, got %q", f.Items[1].Ext.SourceName)
	}
	if strings.Contains(string(body), "content_") || strings.Contains(string(body), "summary") {
		t.Error("bodies must never be emitted")
	}
	if f.Ext == nil || len(f.Ext.Warming) != 0 || len(f.Ext.Stale) != 0 {
		t.Errorf("ext: %+v", f.Ext)
	}
}

func TestGetFeedConditionalRequest(t *testing.T) {
	h := newHarness(t)
	res, _ := h.do(t, "GET", "/v1/feeds?url="+h.up.srv.URL+"/a.xml", "", nil)
	etag := res.Header.Get("ETag")
	res2, _ := h.do(t, "GET", "/v1/feeds?url="+h.up.srv.URL+"/a.xml", "", map[string]string{"If-None-Match": etag})
	if res2.StatusCode != 304 {
		t.Errorf("want 304, got %d", res2.StatusCode)
	}
}

func TestGetFeedRejectsBlockedAndMissingURL(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"", "?url=", "?url=http://evil.example/x", "?url=ftp://x/y", "?url=notaurl"} {
		res, _ := h.do(t, "GET", "/v1/feeds"+q, "", nil)
		if res.StatusCode != 400 {
			t.Errorf("%q: want 400, got %d", q, res.StatusCode)
		}
	}
}

func TestGetFeedColdFailureIs502(t *testing.T) {
	h := newHarness(t)
	res, body := h.do(t, "GET", "/v1/feeds?url="+h.up.srv.URL+"/broken", "", nil)
	if res.StatusCode != 502 {
		t.Errorf("want 502, got %d: %s", res.StatusCode, body)
	}
}

func TestGetFeedStaleAfterUpstreamFailure(t *testing.T) {
	h := newHarness(t)
	url := h.up.srv.URL + "/a.xml"
	h.do(t, "GET", "/v1/feeds?url="+url, "", nil)
	h.up.failA.Store(true)
	h.pollAll(t)
	res, body := h.do(t, "GET", "/v1/feeds?url="+url, "", nil)
	if res.StatusCode != 200 || res.Header.Get("X-Feeds-Stale") != "true" {
		t.Errorf("status %d stale=%q", res.StatusCode, res.Header.Get("X-Feeds-Stale"))
	}
	var f JSONFeed
	_ = json.Unmarshal(body, &f)
	if len(f.Items) != 2 || len(f.Ext.Stale) != 1 || f.Ext.Stale[0] != url {
		t.Errorf("stale feed should still serve cached items: items=%d ext=%+v", len(f.Items), f.Ext)
	}
}

func TestItemsMergesDedupesAndWarms(t *testing.T) {
	h := newHarness(t)
	a, b := h.up.srv.URL+"/a.xml", h.up.srv.URL+"/b.xml"
	res, body := h.do(t, "GET", "/v1/items?feeds="+a+","+b, "", nil)
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var f JSONFeed
	_ = json.Unmarshal(body, &f)
	if len(f.Items) != 0 || len(f.Ext.Warming) != 2 {
		t.Errorf("first call should be empty and warming both: items=%d ext=%+v", len(f.Items), f.Ext)
	}
	h.pollAll(t)
	_, body = h.do(t, "GET", "/v1/items?feeds="+a+","+b, "", nil)
	_ = json.Unmarshal(body, &f)
	if len(f.Ext.Warming) != 0 {
		t.Errorf("warming after poll: %+v", f.Ext)
	}
	// 4 stored, "Shared story" appears in both with the same canonical URL → 3.
	if len(f.Items) != 3 {
		t.Fatalf("items = %d: %+v", len(f.Items), f.Items)
	}
	titles := []string{f.Items[0].Title, f.Items[1].Title, f.Items[2].Title}
	if titles[0] != "Shared story" || titles[1] != "Only in B" || titles[2] != "Only in A" {
		t.Errorf("order: %v", titles)
	}
	if f.Items[0].Ext.FeedURL != b {
		t.Errorf("dedupe keeps the earliest-published copy (B at 10:30): %+v", f.Items[0].Ext)
	}
	_, body = h.do(t, "GET", "/v1/items?feeds="+a+","+b+"&limit=1", "", nil)
	_ = json.Unmarshal(body, &f)
	if len(f.Items) != 1 {
		t.Errorf("limit: %d", len(f.Items))
	}
	_, body = h.do(t, "GET", "/v1/items?feeds="+a+","+b+"&since=2026-09-17T10:15:00Z", "", nil)
	_ = json.Unmarshal(body, &f)
	if len(f.Items) != 1 || f.Items[0].Title != "Shared story" {
		t.Errorf("since: %+v", f.Items)
	}
}

func TestItemsValidation(t *testing.T) {
	h := newHarness(t)
	if res, _ := h.do(t, "GET", "/v1/items", "", nil); res.StatusCode != 400 {
		t.Errorf("no feeds: %d", res.StatusCode)
	}
	many := make([]string, 26)
	for i := range many {
		many[i] = h.up.srv.URL + "/f" + string(rune('a'+i))
	}
	if res, _ := h.do(t, "GET", "/v1/items?feeds="+strings.Join(many, ","), "", nil); res.StatusCode != 400 {
		t.Errorf(">25 feeds: %d", res.StatusCode)
	}
	if res, _ := h.do(t, "GET", "/v1/items?feeds=http://evil.example/x", "", nil); res.StatusCode != 400 {
		t.Errorf("blocked feed url: %d", res.StatusCode)
	}
	if res, _ := h.do(t, "GET", "/v1/items?feeds="+h.up.srv.URL+"/a.xml&since=yesterday", "", nil); res.StatusCode != 400 {
		t.Errorf("bad since: %d", res.StatusCode)
	}
	res, body := h.do(t, "GET", "/v1/items?feeds="+h.up.srv.URL+"/a.xml&limit=9999", "", nil)
	if res.StatusCode != 200 {
		t.Errorf("limit should clamp, not reject: %d %s", res.StatusCode, body)
	}
}

func TestDiscover(t *testing.T) {
	h := newHarness(t)
	res, body := h.do(t, "POST", "/v1/discover", `{"url":"`+h.up.srv.URL+`/site/"}`, map[string]string{"Content-Type": "application/json"})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var out struct {
		Candidates []discover.Candidate `json:"candidates"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Candidates) != 1 || out.Candidates[0].URL != h.up.srv.URL+"/a.xml" || out.Candidates[0].Type != "rss" {
		t.Errorf("candidates: %+v", out.Candidates)
	}
	if res, _ := h.do(t, "POST", "/v1/discover", `{bad`, nil); res.StatusCode != 400 {
		t.Errorf("bad json: %d", res.StatusCode)
	}
	if res, _ := h.do(t, "POST", "/v1/discover", `{"url":"http://evil.example/"}`, nil); res.StatusCode != 400 {
		t.Errorf("blocked: %d", res.StatusCode)
	}
	if res, _ := h.do(t, "POST", "/v1/discover", `{"url":"`+h.up.srv.URL+`/nope"}`, nil); res.StatusCode != 502 {
		t.Errorf("unreachable page: %d", res.StatusCode)
	}
	if res, _ := h.do(t, "GET", "/v1/discover", "", nil); res.StatusCode != 405 {
		t.Errorf("GET discover: %d", res.StatusCode)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	h := newHarness(t)
	res, body := h.do(t, "GET", "/healthz", "", nil)
	if res.StatusCode != 200 {
		t.Fatalf("healthz %d", res.StatusCode)
	}
	var hz map[string]any
	_ = json.Unmarshal(body, &hz)
	if hz["ok"] != true || hz["db"] != "ok" {
		t.Errorf("healthz body: %v", hz)
	}
	h.do(t, "GET", "/v1/feeds?url="+h.up.srv.URL+"/a.xml", "", nil)
	res, body = h.do(t, "GET", "/metrics", "", nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), `feeds_http_requests_total{route="/v1/feeds",code="200"} 1`) ||
		!strings.Contains(string(body), `feeds_fetch_total{result="ok"} 1`) || !strings.Contains(string(body), "feeds_feeds_total 1") {
		t.Errorf("metrics: %d\n%s", res.StatusCode, body)
	}
}

func TestGzipWhenAccepted(t *testing.T) {
	h := newHarness(t)
	res, _ := h.do(t, "GET", "/v1/feeds?url="+h.up.srv.URL+"/a.xml", "", map[string]string{"Accept-Encoding": "gzip"})
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("want gzip, got %q", res.Header.Get("Content-Encoding"))
	}
}
