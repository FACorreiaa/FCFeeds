package poll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/FACorreiaa/feeds/internal/fetch"
	"github.com/FACorreiaa/feeds/internal/parse"
	"github.com/FACorreiaa/feeds/internal/store"
)

// Result classifies one fetch attempt for metrics.
type Result string

const (
	ResultOK          Result = "ok"
	ResultNotModified Result = "not_modified"
	ResultError       Result = "error"
	ResultParseError  Result = "parse_error"
	ResultBlocked     Result = "ssrf_reject"
	ResultTooLarge    Result = "too_large"
	ResultInFlight    Result = "in_flight"
)

// Outcome is what FetchOne reports.
type Outcome struct {
	FeedID   int64
	Result   Result
	Status   int
	NewItems int
	Duration time.Duration
	Err      error
}

// Observer receives every outcome; the metrics package plugs in here.
type Observer func(Outcome)

// Poller runs the fetch loop.
type Poller struct {
	Now     func() time.Time
	Rand    func() float64
	Observe Observer
	Log     *slog.Logger
	store   *store.Store
	client  *fetch.Client
	cfg     Config
	hosts   *hostLimiter
}

// New builds a poller.
func New(s *store.Store, c *fetch.Client, cfg Config) *Poller {
	return &Poller{
		Now:     time.Now,
		Rand:    rand.Float64,
		Observe: func(Outcome) {},
		Log:     slog.Default(),
		store:   s,
		client:  c,
		cfg:     cfg,
		hosts:   newHostLimiter(cfg.HostGap),
	}
}

// Run claims due feeds every tick and fetches them with bounded concurrency
// until ctx is done. It also runs eviction on its own schedule.
func (p *Poller) Run(ctx context.Context) {
	sem := make(chan struct{}, p.cfg.Workers)
	ticker := time.NewTicker(p.cfg.Tick)
	defer ticker.Stop()
	evict := time.NewTicker(p.cfg.EvictEvery)
	defer evict.Stop()
	var wg sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-evict.C:
			p.evict(ctx)
		case <-ticker.C:
			due, err := p.store.ClaimDue(ctx, p.Now(), p.cfg.LockFor, p.cfg.ClaimBatch)
			if err != nil {
				p.Log.Error("claim due feeds", "err", err)
				continue
			}
			for _, d := range due {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					wg.Wait()
					return
				}
				wg.Add(1)
				go func(d store.Due) {
					defer wg.Done()
					defer func() { <-sem }()
					p.FetchOne(ctx, d)
				}(d)
			}
		}
	}
}

func (p *Poller) evict(ctx context.Context) {
	n, err := p.store.EvictFeeds(ctx, p.Now().Add(-p.cfg.EvictAfter))
	if err != nil {
		p.Log.Error("evict feeds", "err", err)
		return
	}
	if n > 0 {
		p.Log.Info("evicted unrequested feeds", "count", n)
	}
}

// FetchOne fetches, parses, stores and reschedules a single claimed feed.
func (p *Poller) FetchOne(ctx context.Context, d store.Due) Outcome {
	start := p.Now()
	out := p.fetchOne(ctx, d)
	out.FeedID = d.Feed.ID
	out.Duration = p.Now().Sub(start)
	if out.Err != nil {
		p.Log.Warn("fetch failed", "feed", d.Feed.URL, "result", out.Result, "status", out.Status, "err", out.Err)
	}
	p.Observe(out)
	return out
}

func (p *Poller) fetchOne(ctx context.Context, d store.Due) Outcome {
	feedURL := d.Feed.URL
	if u, err := url.Parse(feedURL); err == nil {
		release := p.hosts.acquire(u.Hostname())
		defer release()
	}
	now := p.Now()
	res, err := p.client.Get(ctx, feedURL, fetch.Validators{ETag: d.State.ETag, LastModified: d.State.LastModified})
	if err != nil {
		result := ResultError
		switch {
		case errors.Is(err, fetch.ErrBlocked):
			result = ResultBlocked
		case errors.Is(err, fetch.ErrTooLarge):
			result = ResultTooLarge
		}
		return p.recordError(ctx, d, 0, 0, err, result, now)
	}
	if res.NotModified {
		next := now.Add(NextInterval(d.Feed.PollInterval, d.State.QuietPolls+1, p.cfg))
		err := p.store.RecordFetchOK(ctx, d.Feed.ID, store.FetchOK{
			Status: res.Status, FetchedAt: now, NextPollAt: next, PollInterval: d.Feed.PollInterval,
		})
		return Outcome{Result: ResultNotModified, Status: res.Status, Err: err}
	}
	if res.Status < 200 || res.Status > 299 {
		return p.recordError(ctx, d, res.Status, res.RetryAfter, fmt.Errorf("http %d", res.Status), ResultError, now)
	}
	feed, err := parse.Parse(res.Body, feedURL, now)
	if err != nil {
		return p.recordError(ctx, d, res.Status, 0, err, ResultParseError, now)
	}
	items := make([]store.Item, 0, len(feed.Items))
	for _, it := range feed.Items {
		items = append(items, store.Item{
			Key: it.Key, URL: it.URL, CanonicalURL: it.CanonicalURL, Title: it.Title,
			SourceName: it.Source, SourceURL: it.SourceURL, ImageURL: it.Image,
			PublishedAt: it.Published, DateEstimated: it.DateEstimated,
		})
	}
	newItems, err := p.store.UpsertItems(ctx, d.Feed.ID, items, now)
	if err != nil {
		return Outcome{Result: ResultError, Status: res.Status, Err: err}
	}
	quiet := 0
	if newItems == 0 {
		quiet = d.State.QuietPolls + 1
	}
	interval := NextInterval(feed.TTL, 0, p.cfg) // stored base interval, before adaptive growth
	next := now.Add(NextInterval(feed.TTL, quiet, p.cfg))
	if err := p.store.RecordFetchOK(ctx, d.Feed.ID, store.FetchOK{
		ETag: res.ETag, LastModified: res.LastModified, Status: res.Status,
		FetchedAt: now, NextPollAt: next, Kind: string(feed.Kind), Title: feed.Title,
		SiteURL: feed.SiteURL, PollInterval: interval, NewItems: newItems,
	}); err != nil {
		return Outcome{Result: ResultError, Status: res.Status, Err: err}
	}
	if err := p.store.PruneItems(ctx, d.Feed.ID, p.cfg.KeepItems, now.Add(-p.cfg.KeepFor)); err != nil {
		p.Log.Warn("prune items", "feed", feedURL, "err", err)
	}
	return Outcome{Result: ResultOK, Status: res.Status, NewItems: newItems}
}

func (p *Poller) recordError(ctx context.Context, d store.Due, status int, retryAfter time.Duration, cause error, result Result, now time.Time) Outcome {
	wait := Backoff(d.Feed.PollInterval, d.State.FailCount+1, retryAfter, status, p.cfg, p.Rand)
	if err := p.store.RecordFetchError(ctx, d.Feed.ID, status, cause.Error(), now, now.Add(wait)); err != nil {
		cause = errors.Join(cause, err)
	}
	return Outcome{Result: result, Status: status, Err: cause}
}

// FetchNow performs an immediate fetch of a feed that was just registered,
// with its own deadline, so a cold read can return items instead of
// "warming". A feed already being fetched reports ResultInFlight.
func (p *Poller) FetchNow(ctx context.Context, f store.Feed, budget time.Duration) Outcome {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	d, ok, err := p.store.ClaimFeed(ctx, f.ID, p.Now(), p.cfg.LockFor)
	if err != nil {
		return Outcome{FeedID: f.ID, Result: ResultError, Err: err}
	}
	if !ok {
		return Outcome{FeedID: f.ID, Result: ResultInFlight, Err: errors.New("fetch already in flight")}
	}
	return p.FetchOne(ctx, d)
}
