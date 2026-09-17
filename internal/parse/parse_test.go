package parse

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDetect(t *testing.T) {
	cases := map[string]Kind{
		"rss_basic.xml":       KindRSS,
		"rss_googlenews.xml":  KindRSS,
		"atom_basic.xml":      KindAtom,
		"jsonfeed_basic.json": KindJSON,
	}
	for name, want := range cases {
		got, err := Detect(load(t, name))
		if err != nil || got != want {
			t.Errorf("%s: got %q err %v, want %q", name, got, err, want)
		}
	}
	if _, err := Detect([]byte("<html><body>nope</body></html>")); err == nil {
		t.Error("html should not detect as a feed")
	}
}

func TestParseRSSBasic(t *testing.T) {
	f, err := Parse(load(t, "rss_basic.xml"), "https://example.com/rss", now)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindRSS || f.Title != "Example Finance" || f.SiteURL != "https://example.com/" {
		t.Errorf("feed header: %+v", f)
	}
	if f.TTL != 30*time.Minute {
		t.Errorf("ttl = %v, want 30m", f.TTL)
	}
	if len(f.Items) != 3 {
		t.Fatalf("items = %d, want 3 (empty title skipped)", len(f.Items))
	}
	a := f.Items[0]
	if a.Key != "guid-1" {
		t.Errorf("key = %q", a.Key)
	}
	if a.Title != "Fed holds & signals patience" {
		t.Errorf("title = %q", a.Title)
	}
	if a.URL != "https://example.com/a?utm_source=rss&id=1#top" {
		t.Errorf("url = %q", a.URL)
	}
	if a.CanonicalURL != "https://example.com/a?id=1" {
		t.Errorf("canonical = %q", a.CanonicalURL)
	}
	if !a.Published.Equal(time.Date(2026, 9, 16, 14, 5, 0, 0, time.UTC)) || a.DateEstimated {
		t.Errorf("published = %v est=%v", a.Published, a.DateEstimated)
	}
	b := f.Items[1]
	if b.Key != KeyForURL("https://example.com/b") {
		t.Errorf("no-guid key = %q", b.Key)
	}
	if !b.Published.Equal(now) || !b.DateEstimated {
		t.Errorf("no-date should estimate to now: %v %v", b.Published, b.DateEstimated)
	}
	c := f.Items[2]
	if !c.Published.Equal(time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("dc:date = %v", c.Published)
	}
	if c.Image != "https://example.com/c.jpg" {
		t.Errorf("enclosure image = %q", c.Image)
	}
}

func TestParseRSSGoogleNewsSource(t *testing.T) {
	f, err := Parse(load(t, "rss_googlenews.xml"), "https://news.google.com/rss", now)
	if err != nil {
		t.Fatal(err)
	}
	it := f.Items[0]
	if it.Source != "CNBC" || it.SourceURL != "https://www.cnbc.com" {
		t.Errorf("source = %q %q", it.Source, it.SourceURL)
	}
	if it.Image != "https://img.example/t.png" {
		t.Errorf("media:thumbnail = %q", it.Image)
	}
}

func TestParseAtom(t *testing.T) {
	f, err := Parse(load(t, "atom_basic.xml"), "https://blog.example.org/atom.xml", now)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindAtom || f.Title != "Example Blog" || f.SiteURL != "https://blog.example.org/" {
		t.Errorf("header: %+v", f)
	}
	if len(f.Items) != 2 {
		t.Fatalf("items = %d", len(f.Items))
	}
	a := f.Items[0]
	if a.Key != "urn:uuid:1" || a.Title != "Hello world" {
		t.Errorf("a = %+v", a)
	}
	if a.URL != "https://blog.example.org/posts/hello" {
		t.Errorf("relative alternate link not resolved: %q", a.URL)
	}
	if !a.Published.Equal(time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("updated fallback = %v", a.Published)
	}
	if len(a.Authors) != 1 || a.Authors[0] != "Ana" {
		t.Errorf("authors = %v", a.Authors)
	}
	b := f.Items[1]
	if !b.Published.Equal(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("published should win: %v", b.Published)
	}
}

func TestParseJSONFeed(t *testing.T) {
	f, err := Parse(load(t, "jsonfeed_basic.json"), "https://json.example/feed.json", now)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindJSON || f.Title != "JSON Example" || f.SiteURL != "https://json.example/" {
		t.Errorf("header: %+v", f)
	}
	if len(f.Items) != 3 {
		t.Fatalf("items = %d", len(f.Items))
	}
	a := f.Items[0]
	if a.Key != "1" || a.Image != "https://json.example/1.png" || len(a.Authors) != 1 {
		t.Errorf("a = %+v", a)
	}
	if !a.Published.Equal(time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("tz offset: %v", a.Published)
	}
	if !f.Items[1].DateEstimated {
		t.Error("missing date should be estimated")
	}
	if f.Items[2].Key != KeyForURL("https://json.example/3") {
		t.Errorf("no id should fall back to url key: %q", f.Items[2].Key)
	}
}

func TestParseRejectsHTML(t *testing.T) {
	if _, err := Parse([]byte("<!doctype html><html><body>x</body></html>"), "https://x", now); err == nil {
		t.Error("expected error for html")
	}
}

func TestCanonicalURL(t *testing.T) {
	cases := map[string]string{
		"HTTPS://Example.COM:443/a?utm_campaign=x&b=2&a=1#frag":    "https://example.com/a?a=1&b=2",
		"http://example.com:80/p/?fbclid=1&gclid=2&mc_cid=3&ref=x": "http://example.com/p/",
		"https://example.com/plain":                                "https://example.com/plain",
	}
	for in, want := range cases {
		if got := CanonicalURL(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}

func TestParseDate(t *testing.T) {
	ok := []string{
		"Tue, 16 Sep 2026 14:05:00 GMT",
		"Tue, 16 Sep 2026 14:05:00 +0000",
		"16 Sep 26 14:05 +0000",
		"Tue, 9 Sep 2026 14:05:00 GMT",
		"2026-09-16T14:05:00Z",
		"2026-09-16T14:05:00.123+02:00",
		"2026-09-16 14:05:00",
	}
	for _, s := range ok {
		if _, ok := ParseDate(s); !ok {
			t.Errorf("failed to parse %q", s)
		}
	}
	if _, ok := ParseDate("yesterday"); ok {
		t.Error("garbage should fail")
	}
}

func TestClampFutureDate(t *testing.T) {
	future := now.Add(48 * time.Hour)
	if got := clampDate(future, now); !got.Equal(now) {
		t.Errorf("future >24h should clamp to now, got %v", got)
	}
	near := now.Add(time.Hour)
	if got := clampDate(near, now); !got.Equal(near) {
		t.Errorf("near-future should be kept, got %v", got)
	}
}

func TestCleanTitle(t *testing.T) {
	if got := CleanTitle("  A &amp; <b>B</b>\n\n C  "); got != "A & B C" {
		t.Errorf("got %q", got)
	}
	long := make([]rune, 400)
	for i := range long {
		long[i] = 'x'
	}
	if got := CleanTitle(string(long)); len([]rune(got)) != 300 {
		t.Errorf("cap 300 runes, got %d", len([]rune(got)))
	}
}
