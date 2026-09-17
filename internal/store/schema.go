package store

// Migrations run in order inside one transaction each; user_version records
// how many have been applied. Append, never edit.
var migrations = []string{
	`CREATE TABLE feeds (
		id                INTEGER PRIMARY KEY,
		url               TEXT    NOT NULL UNIQUE,
		kind              TEXT    NOT NULL DEFAULT '',
		title             TEXT    NOT NULL DEFAULT '',
		site_url          TEXT    NOT NULL DEFAULT '',
		poll_interval_s   INTEGER NOT NULL DEFAULT 900,
		created_at        INTEGER NOT NULL,
		last_requested_at INTEGER NOT NULL
	);
	CREATE TABLE fetch_state (
		feed_id       INTEGER PRIMARY KEY REFERENCES feeds(id) ON DELETE CASCADE,
		etag          TEXT    NOT NULL DEFAULT '',
		last_modified TEXT    NOT NULL DEFAULT '',
		next_poll_at  INTEGER NOT NULL,
		locked_until  INTEGER,
		last_fetch_at INTEGER,
		last_ok_at    INTEGER,
		last_status   INTEGER NOT NULL DEFAULT 0,
		fail_count    INTEGER NOT NULL DEFAULT 0,
		last_error    TEXT    NOT NULL DEFAULT '',
		quiet_polls   INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX fetch_due ON fetch_state(next_poll_at);
	CREATE TABLE items (
		id             INTEGER PRIMARY KEY,
		feed_id        INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
		item_key       TEXT    NOT NULL,
		url            TEXT    NOT NULL DEFAULT '',
		canonical_url  TEXT    NOT NULL DEFAULT '',
		url_hash       TEXT    NOT NULL DEFAULT '',
		title          TEXT    NOT NULL,
		source_name    TEXT    NOT NULL DEFAULT '',
		source_url     TEXT    NOT NULL DEFAULT '',
		image_url      TEXT    NOT NULL DEFAULT '',
		published_at   INTEGER NOT NULL,
		first_seen_at  INTEGER NOT NULL,
		date_estimated INTEGER NOT NULL DEFAULT 0,
		UNIQUE(feed_id, item_key)
	);
	CREATE INDEX items_pub ON items(published_at DESC);
	CREATE INDEX items_urlhash ON items(url_hash);
	CREATE INDEX items_feed_pub ON items(feed_id, published_at DESC);`,
}
