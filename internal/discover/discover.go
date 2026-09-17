// Package discover finds feed URLs for a site: the URL itself if it already
// is a feed, otherwise <link rel="alternate"> declarations on the page, and
// as a last resort a handful of conventional paths.
package discover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/FACorreiaa/feeds/internal/fetch"
	"github.com/FACorreiaa/feeds/internal/parse"
)

// Candidate is a validated feed URL.
type Candidate struct {
	URL   string `json:"url"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

// Discoverer runs discovery through a guarded client.
type Discoverer struct {
	client *fetch.Client
	// ProbePaths are tried relative to the site root when the page declares
	// no feeds.
	ProbePaths []string
	// MaxCandidates caps how many declared links are validated.
	MaxCandidates int
}

// New returns a discoverer with the conventional probe list.
func New(c *fetch.Client) *Discoverer {
	return &Discoverer{
		client:        c,
		ProbePaths:    []string{"/feed", "/rss", "/rss.xml", "/atom.xml", "/feed.xml", "/feed.json", "/index.xml"},
		MaxCandidates: 8,
	}
}

var feedTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
	"application/json":      true,
	"application/xml":       true,
	"text/xml":              true,
}

// Discover returns candidates ordered declared-first. An unreachable page is
// an error; a reachable page with no feeds is an empty, nil-error result.
func (d *Discoverer) Discover(ctx context.Context, rawURL string) ([]Candidate, error) {
	res, err := d.client.Get(ctx, rawURL, fetch.Validators{})
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status > 299 {
		return nil, fmt.Errorf("discover: %s returned %d", rawURL, res.Status)
	}
	if c, ok := d.validate(res.FinalURL, res.Body); ok {
		return []Candidate{c}, nil
	}
	base, err := url.Parse(res.FinalURL)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	seen := map[string]bool{}
	for _, href := range declaredFeeds(res.Body, base) {
		if seen[href] || len(out) >= d.MaxCandidates {
			continue
		}
		seen[href] = true
		if c, ok := d.fetchAndValidate(ctx, href); ok {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	root := &url.URL{Scheme: base.Scheme, Host: base.Host}
	for _, p := range d.ProbePaths {
		probe := root.ResolveReference(&url.URL{Path: p}).String()
		if seen[probe] {
			continue
		}
		seen[probe] = true
		if c, ok := d.fetchAndValidate(ctx, probe); ok {
			out = append(out, c)
			break // one working conventional path is enough
		}
	}
	return out, nil
}

func (d *Discoverer) fetchAndValidate(ctx context.Context, u string) (Candidate, bool) {
	res, err := d.client.Get(ctx, u, fetch.Validators{})
	if err != nil || res.Status != http.StatusOK {
		return Candidate{}, false
	}
	return d.validate(u, res.Body)
}

func (d *Discoverer) validate(u string, body []byte) (Candidate, bool) {
	f, err := parse.Parse(body, u, time.Now())
	if err != nil {
		return Candidate{}, false
	}
	return Candidate{URL: u, Type: string(f.Kind), Title: f.Title}, true
}

// declaredFeeds extracts <link rel="alternate" type="...feed..."> hrefs from
// an HTML document, resolved against base, in document order.
func declaredFeeds(body []byte, base *url.URL) []string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}
	var out []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "link" {
			var rel, typ, href string
			for _, a := range n.Attr {
				switch strings.ToLower(a.Key) {
				case "rel":
					rel = strings.ToLower(a.Val)
				case "type":
					typ = strings.ToLower(strings.TrimSpace(strings.Split(a.Val, ";")[0]))
				case "href":
					href = strings.TrimSpace(a.Val)
				}
			}
			if strings.Contains(rel, "alternate") && feedTypes[typ] && href != "" {
				if ref, err := url.Parse(href); err == nil {
					out = append(out, base.ResolveReference(ref).String())
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			// <body> can be skipped once <head> is done, but feeds declared in
			// body happen in the wild, so walk everything.
			walk(c)
		}
	}
	walk(doc)
	return out
}

// ErrNoFeeds is a sentinel callers may use when an empty result should be
// reported as an error to end users.
var ErrNoFeeds = errors.New("discover: no feeds found")
