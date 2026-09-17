// Package parse turns RSS 2.0, Atom 1.0 and JSON Feed 1.1 documents into one
// normalized Feed. It never keeps article bodies: only what a ticker shows.
package parse

import "time"

// Kind is the detected wire format of a feed document.
type Kind string

const (
	KindRSS  Kind = "rss"
	KindAtom Kind = "atom"
	KindJSON Kind = "json"
)

// Feed is a normalized feed header plus its items.
type Feed struct {
	Kind    Kind
	Title   string
	SiteURL string
	// TTL is the publisher's suggested poll interval, or 0 when unspecified.
	TTL   time.Duration
	Items []Item
}

// Item is one headline. Bodies, summaries and content are dropped on purpose.
type Item struct {
	Key          string
	URL          string
	CanonicalURL string
	Title        string
	Published    time.Time
	// DateEstimated is true when the document carried no usable date and
	// Published was set to the parse time.
	DateEstimated bool
	Source        string
	SourceURL     string
	Image         string
	Authors       []string
}
