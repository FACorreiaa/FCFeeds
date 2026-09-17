// Package api is the HTTP surface: JSON Feed responses for one feed or a
// merged timeline, discovery, health and metrics.
package api

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FACorreiaa/feeds/internal/discover"
	"github.com/FACorreiaa/feeds/internal/fetch"
	"github.com/FACorreiaa/feeds/internal/metrics"
	"github.com/FACorreiaa/feeds/internal/poll"
	"github.com/FACorreiaa/feeds/internal/store"
)

// Options tunes request handling.
type Options struct {
	ColdFetchBudget time.Duration
	MaxFeedsPerCall int
	DefaultLimit    int
	MaxLimit        int
}

// Server holds the dependencies.
type Server struct {
	store   *store.Store
	poller  *poll.Poller
	disc    *discover.Discoverer
	metrics *metrics.Registry
	opts    Options
	Now     func() time.Time
	Log     *slog.Logger
}

// New builds a server.
func New(s *store.Store, p *poll.Poller, d *discover.Discoverer, m *metrics.Registry, o Options) *Server {
	if o.ColdFetchBudget == 0 {
		o.ColdFetchBudget = 8 * time.Second
	}
	if o.MaxFeedsPerCall == 0 {
		o.MaxFeedsPerCall = 25
	}
	if o.DefaultLimit == 0 {
		o.DefaultLimit = 50
	}
	if o.MaxLimit == 0 {
		o.MaxLimit = 200
	}
	srv := &Server{store: s, poller: p, disc: d, metrics: m, opts: o, Now: time.Now, Log: slog.Default()}
	p.Observe = func(out poll.Outcome) {
		m.IncFetch(string(out.Result))
		m.ObserveFetchDuration(out.Duration)
	}
	m.GaugeSource(func() metrics.Gauges {
		st, err := s.Stats(context.Background(), srv.Now())
		if err != nil {
			return metrics.Gauges{}
		}
		return metrics.Gauges{Feeds: st.Feeds, Due: st.Due, Items: st.Items}
	})
	return srv
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/feeds", s.instrument("/v1/feeds", s.getFeed))
	mux.HandleFunc("GET /v1/items", s.instrument("/v1/items", s.getItems))
	mux.HandleFunc("POST /v1/discover", s.instrument("/v1/discover", s.postDiscover))
	mux.HandleFunc("GET /healthz", s.instrument("/healthz", s.healthz))
	mux.Handle("GET /metrics", s.metrics)
	return mux
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h(sw, r)
		s.metrics.IncHTTP(route, sw.status)
	}
}

// --- handlers ---

func (s *Server) getFeed(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		s.fail(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := s.checkFeedURL(r.Context(), raw); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	now := s.Now()
	feed, _, err := s.store.EnsureFeed(ctx, raw, now)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	state, err := s.store.FetchState(ctx, feed.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if state.LastOKAt == nil {
		// Cold: fetch synchronously within budget so "add feed" shows items.
		out := s.poller.FetchNow(ctx, feed, s.opts.ColdFetchBudget)
		if out.Result != poll.ResultOK && out.Result != poll.ResultNotModified && out.Result != poll.ResultInFlight {
			s.fail(w, http.StatusBadGateway, fmt.Sprintf("could not fetch feed: %v", out.Err))
			return
		}
		if feed, err = s.store.FeedByURL(ctx, raw); err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if state, err = s.store.FetchState(ctx, feed.ID); err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	items, err := s.store.ListItems(ctx, []int64{feed.ID}, time.Time{}, s.opts.MaxLimit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := JSONFeed{
		Version:     jsonFeedVersion,
		Title:       feed.Title,
		HomePageURL: feed.SiteURL,
		FeedURL:     feed.URL,
		Items:       make([]JSONItem, 0, len(items)),
		Ext:         &FeedsExt{Warming: []string{}, Stale: []string{}, GeneratedAt: now},
	}
	for _, it := range items {
		resp.Items = append(resp.Items, toJSONItem(it, feed))
	}
	stale := isStale(state)
	if stale {
		resp.Ext.Stale = append(resp.Ext.Stale, feed.URL)
		w.Header().Set("X-Feeds-Stale", "true")
		s.metrics.IncStaleServe()
	}
	if state.LastOKAt == nil {
		resp.Ext.Warming = append(resp.Ext.Warming, feed.URL)
	} else {
		w.Header().Set("X-Feeds-Fetched-At", state.LastOKAt.UTC().Format(time.RFC3339))
	}
	s.writeFeed(w, r, resp)
}

func (s *Server) getItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	urls := splitFeeds(q.Get("feeds"))
	if len(urls) == 0 {
		s.fail(w, http.StatusBadRequest, "feeds is required (comma-separated feed urls)")
		return
	}
	if len(urls) > s.opts.MaxFeedsPerCall {
		s.fail(w, http.StatusBadRequest, fmt.Sprintf("at most %d feeds per call", s.opts.MaxFeedsPerCall))
		return
	}
	ctx := r.Context()
	for _, u := range urls {
		if err := s.checkFeedURL(ctx, u); err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "since must be RFC 3339")
			return
		}
		since = t
	}
	limit := s.opts.DefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		limit = clamp(n, 1, s.opts.MaxLimit)
	}
	now := s.Now()
	feeds, err := s.store.EnsureFeeds(ctx, urls, now)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ids := make([]int64, len(feeds))
	byID := make(map[int64]store.Feed, len(feeds))
	for i, f := range feeds {
		ids[i] = f.ID
		byID[f.ID] = f
	}
	states, err := s.store.FetchStates(ctx, ids)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Over-fetch so dedupe across feeds still fills the limit.
	raw, err := s.store.ListItems(ctx, ids, since, limit*3)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	merged := dedupe(raw)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	resp := JSONFeed{
		Version: jsonFeedVersion,
		Title:   "Merged timeline",
		Items:   make([]JSONItem, 0, len(merged)),
		Ext:     &FeedsExt{Warming: []string{}, Stale: []string{}, GeneratedAt: now},
	}
	for _, it := range merged {
		resp.Items = append(resp.Items, toJSONItem(it, byID[it.FeedID]))
	}
	staleAny := false
	for _, f := range feeds {
		st := states[f.ID]
		if st.LastOKAt == nil {
			resp.Ext.Warming = append(resp.Ext.Warming, f.URL)
		} else if isStale(st) {
			resp.Ext.Stale = append(resp.Ext.Stale, f.URL)
			staleAny = true
		}
	}
	if staleAny {
		w.Header().Set("X-Feeds-Stale", "true")
		s.metrics.IncStaleServe()
	}
	s.writeFeed(w, r, resp)
}

func (s *Server) postDiscover(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.URL == "" {
		s.fail(w, http.StatusBadRequest, `body must be {"url": "https://..."}`)
		return
	}
	cands, err := s.disc.Discover(r.Context(), in.URL)
	if err != nil {
		if errors.Is(err, fetch.ErrBlocked) {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		s.fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if cands == nil {
		cands = []discover.Candidate{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"candidates": cands})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.store.Ping(ctx); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "db": err.Error()})
		return
	}
	st, err := s.store.Stats(ctx, s.Now())
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "db": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "db": "ok", "feeds": st.Feeds, "due": st.Due, "items": st.Items,
		"oldest_due_age_s": int(st.OldestDueAge.Seconds()),
	})
}

// --- helpers ---

// checkFeedURL runs the same SSRF guard the fetcher uses, so a bad URL is a
// 400 now rather than a poller error later.
func (s *Server) checkFeedURL(ctx context.Context, raw string) error {
	if _, err := s.poller.CheckURL(ctx, raw); err != nil {
		return err
	}
	return nil
}

func isStale(st store.FetchState) bool {
	return st.FailCount > 0 && st.LastOKAt != nil
}

func splitFeeds(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// dedupe collapses the same article across feeds: identical url_hash keeps
// the earliest-published copy; otherwise an identical normalized title
// within 48h keeps the first copy. Input and output are newest-first.
func dedupe(items []store.Item) []store.Item {
	asc := make([]store.Item, len(items))
	copy(asc, items)
	sort.SliceStable(asc, func(i, j int) bool { return asc[i].PublishedAt.Before(asc[j].PublishedAt) })
	seenHash := map[string]bool{}
	type titleSeen struct {
		title string
		at    time.Time
	}
	var titles []titleSeen
	var kept []store.Item
	for _, it := range asc {
		if it.URLHash != "" {
			if seenHash[it.URLHash] {
				continue
			}
			seenHash[it.URLHash] = true
		}
		nt := normalizeTitle(it.Title)
		dup := false
		for _, t := range titles {
			if t.title == nt && it.PublishedAt.Sub(t.at) <= 48*time.Hour {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		titles = append(titles, titleSeen{nt, it.PublishedAt})
		kept = append(kept, it)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].PublishedAt.After(kept[j].PublishedAt) })
	return kept
}

func normalizeTitle(t string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(t) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}

// weakETag hashes the response minus generated_at, so an unchanged timeline
// keeps validating across calls.
func weakETag(f JSONFeed) string {
	if f.Ext != nil {
		ext := *f.Ext
		ext.GeneratedAt = time.Time{}
		f.Ext = &ext
	}
	body, _ := json.Marshal(f)
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeFeed encodes a JSON Feed with an ETag, honours If-None-Match and
// gzips when the client accepts it.
func (s *Server) writeFeed(w http.ResponseWriter, r *http.Request, f JSONFeed) {
	body, err := json.Marshal(f)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	etag := weakETag(f)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=60")
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/feed+json; charset=utf-8")
	if len(body) > 256 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(body)
		_ = gz.Close()
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
