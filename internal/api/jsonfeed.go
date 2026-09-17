package api

import (
	"time"

	"github.com/FACorreiaa/FCFeeds/internal/store"
)

const jsonFeedVersion = "https://jsonfeed.org/version/1.1"

// JSONFeed is the JSON Feed 1.1 response envelope with a `_feeds` extension.
type JSONFeed struct {
	Version     string     `json:"version"`
	Title       string     `json:"title"`
	HomePageURL string     `json:"home_page_url,omitempty"`
	FeedURL     string     `json:"feed_url,omitempty"`
	Items       []JSONItem `json:"items"`
	Ext         *FeedsExt  `json:"_feeds,omitempty"`
}

// JSONItem is one headline. There is deliberately no content or summary.
type JSONItem struct {
	ID            string    `json:"id"`
	URL           string    `json:"url,omitempty"`
	Title         string    `json:"title"`
	DatePublished time.Time `json:"date_published"`
	Image         string    `json:"image,omitempty"`
	Ext           ItemExt   `json:"_feeds"`
}

// ItemExt carries attribution the ticker shows next to the headline.
type ItemExt struct {
	SourceName    string `json:"source_name"`
	SourceURL     string `json:"source_url,omitempty"`
	FeedURL       string `json:"feed_url"`
	DateEstimated bool   `json:"date_estimated,omitempty"`
}

// FeedsExt is response-level state: which requested feeds have not been
// fetched yet and which are serving cached items after a failed fetch.
type FeedsExt struct {
	Warming     []string  `json:"warming"`
	Stale       []string  `json:"stale"`
	GeneratedAt time.Time `json:"generated_at"`
}

// toJSONItem maps a stored item; feed supplies the fallback source name.
func toJSONItem(it store.Item, feed store.Feed) JSONItem {
	src, srcURL := it.SourceName, it.SourceURL
	if src == "" {
		src = feed.Title
	}
	if src == "" {
		src = hostOf(feed.URL)
	}
	if srcURL == "" {
		srcURL = feed.SiteURL
	}
	return JSONItem{
		ID:            it.Key,
		URL:           it.URL,
		Title:         it.Title,
		DatePublished: it.PublishedAt,
		Image:         it.ImageURL,
		Ext: ItemExt{
			SourceName:    src,
			SourceURL:     srcURL,
			FeedURL:       feed.URL,
			DateEstimated: it.DateEstimated,
		},
	}
}
