package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEnsureFeedIsIdempotentAndDueImmediately(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	f1, created, err := s.EnsureFeed(ctx, "https://a.example/rss", now)
	if err != nil || !created {
		t.Fatalf("first ensure: created=%v err=%v", created, err)
	}
	f2, created, err := s.EnsureFeed(ctx, "https://a.example/rss", now.Add(time.Minute))
	if err != nil || created || f2.ID != f1.ID {
		t.Fatalf("second ensure: created=%v id=%d/%d err=%v", created, f1.ID, f2.ID, err)
	}
	if !f2.LastRequestedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("last_requested_at not bumped: %v", f2.LastRequestedAt)
	}
	st, err := s.FetchState(ctx, f1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.NextPollAt.Equal(now) {
		t.Errorf("new feed should be due at creation: %v", st.NextPollAt)
	}
}

func TestClaimDueLocksRows(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	f, _, _ := s.EnsureFeed(ctx, "https://a.example/rss", now)
	_, _, _ = s.EnsureFeed(ctx, "https://b.example/rss", now.Add(time.Minute)) // not due yet at `now`
	due, err := s.ClaimDue(ctx, now, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Feed.ID != f.ID {
		t.Fatalf("due = %+v", due)
	}
	again, _ := s.ClaimDue(ctx, now, time.Minute, 10)
	if len(again) != 0 {
		t.Errorf("locked row claimed twice")
	}
	after, _ := s.ClaimDue(ctx, now.Add(2*time.Minute), time.Minute, 10)
	if len(after) != 2 {
		t.Errorf("lock expiry + b due: got %d", len(after))
	}
}

func TestRecordFetchOKAndError(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	f, _, _ := s.EnsureFeed(ctx, "https://a.example/rss", now)
	next := now.Add(15 * time.Minute)
	err := s.RecordFetchOK(ctx, f.ID, FetchOK{
		ETag: `"e1"`, LastModified: "Tue, 16 Sep 2026 14:05:00 GMT", Status: 200,
		FetchedAt: now, NextPollAt: next, Kind: "rss", Title: "A", SiteURL: "https://a.example/",
		PollInterval: 15 * time.Minute, NewItems: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.FetchState(ctx, f.ID)
	if st.ETag != `"e1"` || st.FailCount != 0 || st.LastStatus != 200 || !st.NextPollAt.Equal(next) || st.LockedUntil != nil {
		t.Errorf("state after ok: %+v", st)
	}
	if st.LastOKAt == nil || !st.LastOKAt.Equal(now) {
		t.Errorf("last_ok_at = %v", st.LastOKAt)
	}
	if st.QuietPolls != 0 {
		t.Errorf("quiet_polls should reset when items arrive: %d", st.QuietPolls)
	}
	g, _ := s.FeedByURL(ctx, "https://a.example/rss")
	if g.Title != "A" || g.Kind != "rss" || g.PollInterval != 15*time.Minute {
		t.Errorf("feed meta not updated: %+v", g)
	}

	// Quiet poll (304 or zero new) increments quiet_polls.
	_ = s.RecordFetchOK(ctx, f.ID, FetchOK{Status: 304, FetchedAt: now, NextPollAt: next, NewItems: 0, Kind: "rss", Title: "A", PollInterval: 15 * time.Minute})
	st, _ = s.FetchState(ctx, f.ID)
	if st.QuietPolls != 1 {
		t.Errorf("quiet_polls = %d, want 1", st.QuietPolls)
	}

	errNext := now.Add(time.Hour)
	if err := s.RecordFetchError(ctx, f.ID, 503, "upstream down", now, errNext); err != nil {
		t.Fatal(err)
	}
	st, _ = s.FetchState(ctx, f.ID)
	if st.FailCount != 1 || st.LastStatus != 503 || st.LastError != "upstream down" || !st.NextPollAt.Equal(errNext) {
		t.Errorf("state after error: %+v", st)
	}
	if st.LastOKAt == nil || !st.LastOKAt.Equal(now) {
		t.Errorf("error must not clear last_ok_at")
	}
}

func TestUpsertItemsDedupesAndCountsNew(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	f, _, _ := s.EnsureFeed(ctx, "https://a.example/rss", now)
	items := []Item{
		{Key: "k1", URL: "https://a.example/1", CanonicalURL: "https://a.example/1", Title: "One", PublishedAt: now.Add(-time.Hour)},
		{Key: "k2", URL: "https://a.example/2", CanonicalURL: "https://a.example/2", Title: "Two", PublishedAt: now.Add(-2 * time.Hour)},
	}
	n, err := s.UpsertItems(ctx, f.ID, items, now)
	if err != nil || n != 2 {
		t.Fatalf("first upsert n=%d err=%v", n, err)
	}
	items = append(items, Item{Key: "k3", URL: "https://a.example/3", CanonicalURL: "https://a.example/3", Title: "Three", PublishedAt: now})
	n, err = s.UpsertItems(ctx, f.ID, items, now.Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("second upsert n=%d err=%v (want 1 new)", n, err)
	}
	got, err := s.ListItems(ctx, []int64{f.ID}, time.Time{}, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("list: %d %v", len(got), err)
	}
	if got[0].Key != "k3" || got[2].Key != "k2" {
		t.Errorf("not sorted by published desc: %v %v %v", got[0].Key, got[1].Key, got[2].Key)
	}
	if got[0].URLHash == "" || got[0].FeedURL != "https://a.example/rss" {
		t.Errorf("url_hash/feed_url not populated: %+v", got[0])
	}
	since, _ := s.ListItems(ctx, []int64{f.ID}, now.Add(-90*time.Minute), 10)
	if len(since) != 2 {
		t.Errorf("since filter: %d", len(since))
	}
	lim, _ := s.ListItems(ctx, []int64{f.ID}, time.Time{}, 1)
	if len(lim) != 1 {
		t.Errorf("limit: %d", len(lim))
	}
}

func TestPruneKeepsNewestAndDropsOld(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	f, _, _ := s.EnsureFeed(ctx, "https://a.example/rss", now)
	var items []Item
	for i := 0; i < 10; i++ {
		items = append(items, Item{Key: string(rune('a' + i)), Title: "t", PublishedAt: now.Add(-time.Duration(i) * 24 * time.Hour)})
	}
	_, _ = s.UpsertItems(ctx, f.ID, items, now)
	// keep 5 newest, and nothing older than 60h → 3 survive (0,1,2 days old)
	if err := s.PruneItems(ctx, f.ID, 5, now.Add(-60*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListItems(ctx, []int64{f.ID}, time.Time{}, 100)
	if len(got) != 3 {
		t.Errorf("after prune = %d, want 3", len(got))
	}
}

func TestEvictUnrequestedFeedsCascades(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old, _, _ := s.EnsureFeed(ctx, "https://old.example/rss", now.Add(-30*24*time.Hour))
	_, _, _ = s.EnsureFeed(ctx, "https://new.example/rss", now)
	_, _ = s.UpsertItems(ctx, old.ID, []Item{{Key: "x", Title: "t", PublishedAt: now}}, now)
	n, err := s.EvictFeeds(ctx, now.Add(-14*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("evict n=%d err=%v", n, err)
	}
	if _, err := s.FetchState(ctx, old.ID); err == nil {
		t.Error("fetch_state should cascade")
	}
	if got, _ := s.ListItems(ctx, []int64{old.ID}, time.Time{}, 10); len(got) != 0 {
		t.Error("items should cascade")
	}
}

func TestStats(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	_, _, _ = s.EnsureFeed(ctx, "https://a.example/rss", now.Add(-10*time.Minute))
	_, _, _ = s.EnsureFeed(ctx, "https://b.example/rss", now.Add(time.Hour))
	st, err := s.Stats(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Feeds != 2 || st.Due != 1 || st.OldestDueAge != 10*time.Minute {
		t.Errorf("stats = %+v", st)
	}
}
