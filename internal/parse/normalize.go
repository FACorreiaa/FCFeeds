package parse

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxTitleRunes = 300

var (
	reTags       = regexp.MustCompile(`<[^>]*>`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// trackingParams are query keys dropped when canonicalizing item URLs.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true, "ref": true,
}

// CleanTitle unescapes entities, strips tags, collapses whitespace and caps
// the length so a headline is safe to render inline.
func CleanTitle(s string) string {
	s = html.UnescapeString(s)
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s) // entities that were hiding inside tags
	s = strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) > maxTitleRunes {
		r := []rune(s)
		s = string(r[:maxTitleRunes])
	}
	return s
}

// CanonicalURL lowercases scheme and host, drops default ports, fragments and
// tracking parameters, and sorts the remaining query so equal articles hash
// equal. Unparseable input is returned unchanged.
func CanonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if (u.Scheme == "http" && strings.HasSuffix(host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(host, ":443")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	u.Host = host
	u.Fragment = ""
	u.RawFragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if trackingParams[k] || strings.HasPrefix(k, "utm_") {
				q.Del(k)
			}
		}
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			for _, v := range q[k] {
				if b.Len() > 0 {
					b.WriteByte('&')
				}
				b.WriteString(url.QueryEscape(k))
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(v))
			}
		}
		u.RawQuery = b.String()
	}
	return u.String()
}

// KeyForURL is the item key used when a document carries no guid/id.
func KeyForURL(raw string) string {
	return hash(CanonicalURL(raw))
}

func keyForTitle(feedURL, title string, published time.Time) string {
	return hash(feedURL + "|" + title + "|" + published.UTC().Format(time.RFC3339))
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// clampDate pulls dates more than a day in the future back to now; publishers
// with broken clocks otherwise pin their item to the top of every timeline.
func clampDate(t, now time.Time) time.Time {
	if t.After(now.Add(24 * time.Hour)) {
		return now
	}
	return t
}

// resolveRef resolves a possibly relative href against a base document URL.
func resolveRef(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil || base == "" {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// finish fills derived fields on an item and reports whether it is keepable.
func finish(it *Item, feedURL string, now time.Time) bool {
	it.Title = CleanTitle(it.Title)
	if it.Title == "" {
		return false
	}
	it.URL = strings.TrimSpace(it.URL)
	if it.URL != "" {
		it.CanonicalURL = CanonicalURL(it.URL)
	}
	if it.Published.IsZero() {
		it.Published = now
		it.DateEstimated = true
	} else {
		it.Published = clampDate(it.Published.UTC(), now)
	}
	it.Key = strings.TrimSpace(it.Key)
	switch {
	case it.Key != "":
	case it.CanonicalURL != "":
		it.Key = hash(it.CanonicalURL)
	default:
		it.Key = keyForTitle(feedURL, it.Title, it.Published)
	}
	return true
}
