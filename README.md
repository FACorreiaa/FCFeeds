# feeds

Shared in-cluster feed aggregator for every app on `maat`. Polls RSS 2.0,
Atom 1.0 and JSON Feed 1.1 sources once, politely, on behalf of all
consumers, and serves normalized **JSON Feed 1.1** timelines. No API keys,
no model tokens, no vendor.

Runs as `feeds` in namespace `horus`:

    http://feeds.horus.svc.cluster.local:8080

A consumer must be listed in `allow-feeds` in
`platform/infra/cluster/network-policies/horus.yaml` or every request is
refused — `horus` is default-deny ingress and the failure looks exactly like
the service being down.

## What it serves

Headline, source, timestamp, link. Never article bodies or summaries. That
keeps every consumer on the right side of App Store / Play review and of
publishers' terms: we link out, we do not republish.

| Endpoint | Purpose |
|---|---|
| `GET /v1/feeds?url=<feed>` | One feed as JSON Feed. First request does a synchronous fetch (8 s budget); later ones serve cache. `X-Feeds-Stale: true` when the last fetch failed but cached items exist. |
| `GET /v1/items?feeds=a,b&since=<RFC3339>&limit=50` | Merged newest-first timeline across up to 25 feeds, deduped. Never blocks on network: feeds not yet fetched are listed in `_feeds.warming`. |
| `POST /v1/discover` `{"url": "https://site"}` | Feed candidates for a site: the URL itself, `<link rel="alternate">` declarations, then `/feed`, `/rss.xml`, `/atom.xml`, `/feed.json`, … Each validated by parsing. No side effects. |
| `GET /healthz` | DB ping plus backlog counts. Readiness and liveness. |
| `GET /metrics` | Prometheus text. |

Every JSON Feed item carries `_feeds.source_name`, `_feeds.source_url` and
`_feeds.feed_url`. Responses carry `_feeds.warming[]`, `_feeds.stale[]` and
`_feeds.generated_at`. Responses have weak ETags; send `If-None-Match`.

## How feeds get registered

Implicitly. Any `GET /v1/feeds` or `GET /v1/items` upserts the URL and marks
it due. Feeds nobody has requested for `FEEDS_EVICT_AFTER` (14 days) are
deleted nightly. Consumers keep no state about this service.

## Polling

- Interval: publisher `<ttl>` / `sy:updatePeriod`, floored at `FEEDS_MIN_POLL`
  (5 m), default 15 m. Six consecutive polls with no new items double it, up
  to 2 h; new items reset it.
- Conditional GET with `ETag` / `Last-Modified`; a 304 costs nothing.
- Errors: exponential backoff with jitter, capped at 6 h; `Retry-After`
  honoured; five 404/410s in a row pin the feed to daily. Cached items keep
  serving throughout.
- One request at a time per host, 2 s apart, one `User-Agent`.
- Retention: 300 items or 14 days per feed.

## Safety

Outbound requests pass an SSRF guard: `http`/`https` on 80/443 only, no
userinfo, hostname denylist (`localhost`, `*.internal`, `*.svc`,
`*.cluster.local`, `*.local`), every resolved address must be public
(re-checked at dial time and on each redirect hop), no https→http
redirects, 5 MiB body cap, 15 s timeout. Image URLs are stored, never
fetched.

## Configuration

| Variable | Default |
|---|---|
| `FEEDS_ADDR` | `:8080` |
| `FEEDS_DB_PATH` | `/data/feeds.db` |
| `FEEDS_USER_AGENT` | `feeds/1.0 (+…; shared aggregator)` |
| `FEEDS_MIN_POLL` | `5m` (must be ≥ 1m) |
| `FEEDS_WORKERS` | `4` |
| `FEEDS_MAX_BODY_BYTES` | `5242880` |
| `FEEDS_FETCH_TIMEOUT` | `15s` |
| `FEEDS_EVICT_AFTER` | `336h` |
| `FEEDS_COLD_FETCH_BUDGET` | `8s` |

## Development

    make test      # go test -race ./...
    make run       # local server on :8080 with data/feeds.db
    curl 'localhost:8080/v1/feeds?url=https://feeds.bbci.co.uk/news/rss.xml'

Dependencies: standard library, `golang.org/x/net/html` (discovery),
`modernc.org/sqlite` (pure Go, no cgo). Nothing else.

## Deploy

CI pushes `ghcr.io/facorreiaa/fcfeeds:<sha>` on `main`. Set that tag in
`platform/infra/apps/feeds/values-production.yaml` and merge; ArgoCD rolls
it. Single replica, `strategy: Recreate` — SQLite has one writer.
