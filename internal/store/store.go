// Package store is the SQLite persistence layer: feeds, their fetch state and
// the headlines we keep. One writer process, WAL mode, foreign keys on.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Feed is a subscribed feed URL and its last-known metadata.
type Feed struct {
	ID              int64
	URL             string
	Kind            string
	Title           string
	SiteURL         string
	PollInterval    time.Duration
	CreatedAt       time.Time
	LastRequestedAt time.Time
}

// FetchState is the poller's bookkeeping for one feed.
type FetchState struct {
	FeedID       int64
	ETag         string
	LastModified string
	NextPollAt   time.Time
	LockedUntil  *time.Time
	LastFetchAt  *time.Time
	LastOKAt     *time.Time
	LastStatus   int
	FailCount    int
	LastError    string
	QuietPolls   int
}

// Due pairs a feed with its state for the poller.
type Due struct {
	Feed  Feed
	State FetchState
}

// FetchOK is what the poller records after a 200 or 304.
type FetchOK struct {
	ETag         string
	LastModified string
	Status       int
	FetchedAt    time.Time
	NextPollAt   time.Time
	Kind         string
	Title        string
	SiteURL      string
	PollInterval time.Duration
	NewItems     int
}

// Item is one stored headline. FeedURL is populated on reads.
type Item struct {
	ID            int64
	FeedID        int64
	FeedURL       string
	Key           string
	URL           string
	CanonicalURL  string
	URLHash       string
	Title         string
	SourceName    string
	SourceURL     string
	ImageURL      string
	PublishedAt   time.Time
	FirstSeenAt   time.Time
	DateEstimated bool
}

// Stats is what /healthz reports.
type Stats struct {
	Feeds        int
	Due          int
	Items        int
	OldestDueAge time.Duration
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has a single writer and the poller is the only
	// heavy user, so serializing here avoids SQLITE_BUSY without a mutex.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the handle.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// EnsureFeed upserts a feed URL, bumping last_requested_at. A brand-new feed
// is due immediately so the poller picks it up on its next tick.
func (s *Store) EnsureFeed(ctx context.Context, url string, now time.Time) (Feed, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO feeds (url, created_at, last_requested_at) VALUES (?, ?, ?) ON CONFLICT(url) DO NOTHING`,
		url, now.Unix(), now.Unix())
	if err != nil {
		return Feed{}, false, err
	}
	created, _ := res.RowsAffected()
	if created == 1 {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO fetch_state (feed_id, next_poll_at) SELECT id, ? FROM feeds WHERE url = ?`, now.Unix(), url); err != nil {
			return Feed{}, false, err
		}
	} else {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE feeds SET last_requested_at = MAX(last_requested_at, ?) WHERE url = ?`, now.Unix(), url); err != nil {
			return Feed{}, false, err
		}
	}
	f, err := s.FeedByURL(ctx, url)
	return f, created == 1, err
}

// EnsureFeeds is EnsureFeed over many URLs, preserving order.
func (s *Store) EnsureFeeds(ctx context.Context, urls []string, now time.Time) ([]Feed, error) {
	out := make([]Feed, 0, len(urls))
	for _, u := range urls {
		f, _, err := s.EnsureFeed(ctx, u, now)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

const feedCols = `id, url, kind, title, site_url, poll_interval_s, created_at, last_requested_at`

func scanFeed(row interface{ Scan(...any) error }) (Feed, error) {
	var f Feed
	var interval, created, requested int64
	if err := row.Scan(&f.ID, &f.URL, &f.Kind, &f.Title, &f.SiteURL, &interval, &created, &requested); err != nil {
		return Feed{}, err
	}
	f.PollInterval = time.Duration(interval) * time.Second
	f.CreatedAt = time.Unix(created, 0).UTC()
	f.LastRequestedAt = time.Unix(requested, 0).UTC()
	return f, nil
}

// FeedByURL loads one feed.
func (s *Store) FeedByURL(ctx context.Context, url string) (Feed, error) {
	return scanFeed(s.db.QueryRowContext(ctx, `SELECT `+feedCols+` FROM feeds WHERE url = ?`, url))
}

const stateCols = `feed_id, etag, last_modified, next_poll_at, locked_until, last_fetch_at, last_ok_at, last_status, fail_count, last_error, quiet_polls`

func scanState(row interface{ Scan(...any) error }) (FetchState, error) {
	var st FetchState
	var next int64
	var locked, fetched, ok sql.NullInt64
	if err := row.Scan(&st.FeedID, &st.ETag, &st.LastModified, &next, &locked, &fetched, &ok, &st.LastStatus, &st.FailCount, &st.LastError, &st.QuietPolls); err != nil {
		return FetchState{}, err
	}
	st.NextPollAt = time.Unix(next, 0).UTC()
	st.LockedUntil = nullTime(locked)
	st.LastFetchAt = nullTime(fetched)
	st.LastOKAt = nullTime(ok)
	return st, nil
}

func nullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}

// FetchState loads the poller state for a feed.
func (s *Store) FetchState(ctx context.Context, feedID int64) (FetchState, error) {
	return scanState(s.db.QueryRowContext(ctx, `SELECT `+stateCols+` FROM fetch_state WHERE feed_id = ?`, feedID))
}

// ClaimDue returns up to limit feeds whose next poll is due and that are not
// locked, and locks them for lockFor so a slow fetch is not double-claimed.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, lockFor time.Duration, limit int) ([]Due, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT `+feedCols+` FROM feeds WHERE id IN (
			SELECT feed_id FROM fetch_state
			WHERE next_poll_at <= ? AND (locked_until IS NULL OR locked_until < ?)
			ORDER BY next_poll_at LIMIT ?)`,
		now.Unix(), now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	var feeds []Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		feeds = append(feeds, f)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Due, 0, len(feeds))
	until := now.Add(lockFor).Unix()
	for _, f := range feeds {
		if _, err := tx.ExecContext(ctx, `UPDATE fetch_state SET locked_until = ? WHERE feed_id = ?`, until, f.ID); err != nil {
			return nil, err
		}
		st, err := scanState(tx.QueryRowContext(ctx, `SELECT `+stateCols+` FROM fetch_state WHERE feed_id = ?`, f.ID))
		if err != nil {
			return nil, err
		}
		out = append(out, Due{Feed: f, State: st})
	}
	return out, tx.Commit()
}

// ClaimFeed locks one specific feed if it is not already locked, regardless
// of whether it is due. Used for on-demand first fetches.
func (s *Store) ClaimFeed(ctx context.Context, feedID int64, now time.Time, lockFor time.Duration) (Due, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE fetch_state SET locked_until = ? WHERE feed_id = ? AND (locked_until IS NULL OR locked_until < ?)`,
		now.Add(lockFor).Unix(), feedID, now.Unix())
	if err != nil {
		return Due{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Due{}, false, nil
	}
	f, err := scanFeed(s.db.QueryRowContext(ctx, `SELECT `+feedCols+` FROM feeds WHERE id = ?`, feedID))
	if err != nil {
		return Due{}, false, err
	}
	st, err := s.FetchState(ctx, feedID)
	if err != nil {
		return Due{}, false, err
	}
	return Due{Feed: f, State: st}, true, nil
}

// RecordFetchOK stores validators, schedules the next poll, refreshes feed
// metadata and tracks quiet polls for adaptive intervals.
func (s *Store) RecordFetchOK(ctx context.Context, feedID int64, r FetchOK) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	quiet := `quiet_polls + 1`
	if r.NewItems > 0 {
		quiet = `0`
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE fetch_state SET
			etag = CASE WHEN ? != '' THEN ? ELSE etag END,
			last_modified = CASE WHEN ? != '' THEN ? ELSE last_modified END,
			next_poll_at = ?, locked_until = NULL, last_fetch_at = ?, last_ok_at = ?,
			last_status = ?, fail_count = 0, last_error = '', quiet_polls = `+quiet+`
		 WHERE feed_id = ?`,
		r.ETag, r.ETag, r.LastModified, r.LastModified,
		r.NextPollAt.Unix(), r.FetchedAt.Unix(), r.FetchedAt.Unix(), r.Status, feedID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE feeds SET
			kind = CASE WHEN ? != '' THEN ? ELSE kind END,
			title = CASE WHEN ? != '' THEN ? ELSE title END,
			site_url = CASE WHEN ? != '' THEN ? ELSE site_url END,
			poll_interval_s = CASE WHEN ? > 0 THEN ? ELSE poll_interval_s END
		 WHERE id = ?`,
		r.Kind, r.Kind, r.Title, r.Title, r.SiteURL, r.SiteURL,
		int64(r.PollInterval/time.Second), int64(r.PollInterval/time.Second), feedID); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordFetchError bumps fail_count, keeps last_ok_at intact and schedules the
// backed-off retry. Cached items stay servable.
func (s *Store) RecordFetchError(ctx context.Context, feedID int64, status int, msg string, fetchedAt, nextPollAt time.Time) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE fetch_state SET
			next_poll_at = ?, locked_until = NULL, last_fetch_at = ?, last_status = ?,
			fail_count = fail_count + 1, last_error = ?
		 WHERE feed_id = ?`,
		nextPollAt.Unix(), fetchedAt.Unix(), status, msg, feedID)
	return err
}

// UpsertItems inserts headlines, ignoring ones already known by key, and
// returns how many were new.
func (s *Store) UpsertItems(ctx context.Context, feedID int64, items []Item, now time.Time) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO items (feed_id, item_key, url, canonical_url, url_hash, title, source_name, source_url, image_url, published_at, first_seen_at, date_estimated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(feed_id, item_key) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	inserted := 0
	for _, it := range items {
		hash := it.URLHash
		if hash == "" && it.CanonicalURL != "" {
			hash = hashString(it.CanonicalURL)
		}
		est := 0
		if it.DateEstimated {
			est = 1
		}
		res, err := stmt.ExecContext(ctx, feedID, it.Key, it.URL, it.CanonicalURL, hash, it.Title,
			it.SourceName, it.SourceURL, it.ImageURL, it.PublishedAt.Unix(), now.Unix(), est)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

const itemCols = `i.id, i.feed_id, f.url, i.item_key, i.url, i.canonical_url, i.url_hash, i.title, i.source_name, i.source_url, i.image_url, i.published_at, i.first_seen_at, i.date_estimated`

// ListItems returns headlines for the given feeds newest first. A zero since
// means no lower bound.
func (s *Store) ListItems(ctx context.Context, feedIDs []int64, since time.Time, limit int) ([]Item, error) {
	if len(feedIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(feedIDs)+2)
	for _, id := range feedIDs {
		args = append(args, id)
	}
	q := `SELECT ` + itemCols + ` FROM items i JOIN feeds f ON f.id = i.feed_id
		WHERE i.feed_id IN (` + placeholders(len(feedIDs)) + `)`
	if !since.IsZero() {
		q += ` AND i.published_at >= ?`
		args = append(args, since.Unix())
	}
	q += ` ORDER BY i.published_at DESC, i.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var pub, seen int64
		var est int
		if err := rows.Scan(&it.ID, &it.FeedID, &it.FeedURL, &it.Key, &it.URL, &it.CanonicalURL, &it.URLHash, &it.Title,
			&it.SourceName, &it.SourceURL, &it.ImageURL, &pub, &seen, &est); err != nil {
			return nil, err
		}
		it.PublishedAt = time.Unix(pub, 0).UTC()
		it.FirstSeenAt = time.Unix(seen, 0).UTC()
		it.DateEstimated = est == 1
		out = append(out, it)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// PruneItems keeps at most keep newest items for a feed and drops anything
// published before olderThan.
func (s *Store) PruneItems(ctx context.Context, feedID int64, keep int, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM items WHERE feed_id = ? AND (published_at < ? OR id NOT IN (
			SELECT id FROM items WHERE feed_id = ? ORDER BY published_at DESC, id DESC LIMIT ?))`,
		feedID, olderThan.Unix(), feedID, keep)
	return err
}

// EvictFeeds deletes feeds nobody has requested since before the cutoff.
// Items and state cascade.
func (s *Store) EvictFeeds(ctx context.Context, unrequestedBefore time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM feeds WHERE last_requested_at < ?`, unrequestedBefore.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats summarises the poll backlog.
func (s *Store) Stats(ctx context.Context, now time.Time) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feeds`).Scan(&st.Feeds); err != nil {
		return st, err
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(next_poll_at) FROM fetch_state WHERE next_poll_at <= ?`, now.Unix()).Scan(&st.Due, &oldest); err != nil {
		return st, err
	}
	if oldest.Valid {
		st.OldestDueAge = now.Sub(time.Unix(oldest.Int64, 0))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&st.Items); err != nil {
		return st, err
	}
	return st, nil
}

// FetchStates loads poller state for many feeds at once, keyed by feed id.
// Unknown ids are simply absent.
func (s *Store) FetchStates(ctx context.Context, feedIDs []int64) (map[int64]FetchState, error) {
	out := map[int64]FetchState{}
	if len(feedIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(feedIDs))
	for i, id := range feedIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+stateCols+` FROM fetch_state WHERE feed_id IN (`+placeholders(len(feedIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		out[st.FeedID] = st
	}
	return out, rows.Err()
}

// ErrNotFound is returned by lookups that find nothing.
var ErrNotFound = errors.New("store: not found")

// IsNotFound reports whether err is a missing-row error.
func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound)
}
