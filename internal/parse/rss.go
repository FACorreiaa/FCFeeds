package parse

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
	"time"
)

type rssDoc struct {
	Channel struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
		TTL   string `xml:"ttl"`
		// sy:updatePeriod / sy:updateFrequency (RSS 1.0 syndication module).
		UpdatePeriod    string    `xml:"http://purl.org/rss/1.0/modules/syndication/ updatePeriod"`
		UpdateFrequency string    `xml:"http://purl.org/rss/1.0/modules/syndication/ updateFrequency"`
		Items           []rssItem `xml:"item"`
	} `xml:"channel"`
	// RSS 1.0 (RDF) puts items at the root.
	RDFItems []rssItem `xml:"item"`
}

type rssItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	GUID    string `xml:"guid"`
	PubDate string `xml:"pubDate"`
	DCDate  string `xml:"http://purl.org/dc/elements/1.1/ date"`
	Author  string `xml:"author"`
	Creator string `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Source  struct {
		URL  string `xml:"url,attr"`
		Name string `xml:",chardata"`
	} `xml:"source"`
	Enclosures []struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
	MediaContent []struct {
		URL    string `xml:"url,attr"`
		Type   string `xml:"type,attr"`
		Medium string `xml:"medium,attr"`
	} `xml:"http://search.yahoo.com/mrss/ content"`
	MediaThumbnail []struct {
		URL string `xml:"url,attr"`
	} `xml:"http://search.yahoo.com/mrss/ thumbnail"`
}

func parseRSS(body []byte, feedURL string, now time.Time) (*Feed, error) {
	var doc rssDoc
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	ch := doc.Channel
	f := &Feed{
		Kind:    KindRSS,
		Title:   CleanTitle(ch.Title),
		SiteURL: strings.TrimSpace(ch.Link),
		TTL:     rssTTL(ch.TTL, ch.UpdatePeriod, ch.UpdateFrequency),
	}
	items := ch.Items
	if len(items) == 0 {
		items = doc.RDFItems
	}
	for _, ri := range items {
		it := Item{
			Key:       ri.GUID,
			URL:       resolveRef(f.SiteURL, ri.Link),
			Title:     ri.Title,
			Source:    CleanTitle(ri.Source.Name),
			SourceURL: strings.TrimSpace(ri.Source.URL),
			Image:     rssImage(ri),
		}
		if t, ok := ParseDate(ri.PubDate); ok {
			it.Published = t
		} else if t, ok := ParseDate(ri.DCDate); ok {
			it.Published = t
		}
		if a := CleanTitle(firstNonEmpty(ri.Creator, ri.Author)); a != "" {
			it.Authors = []string{a}
		}
		if finish(&it, feedURL, now) {
			f.Items = append(f.Items, it)
		}
	}
	return f, nil
}

func rssTTL(ttl, period, freq string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(ttl)); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	var unit time.Duration
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "hourly":
		unit = time.Hour
	case "daily":
		unit = 24 * time.Hour
	case "weekly":
		unit = 7 * 24 * time.Hour
	default:
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(freq))
	if err != nil || n <= 0 {
		n = 1
	}
	return unit / time.Duration(n)
}

func rssImage(ri rssItem) string {
	for _, e := range ri.Enclosures {
		if strings.HasPrefix(e.Type, "image/") && e.URL != "" {
			return e.URL
		}
	}
	for _, m := range ri.MediaContent {
		if (strings.HasPrefix(m.Type, "image/") || m.Medium == "image") && m.URL != "" {
			return m.URL
		}
	}
	for _, t := range ri.MediaThumbnail {
		if t.URL != "" {
			return t.URL
		}
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
