package parse

import (
	"bytes"
	"encoding/xml"
	"strings"
	"time"
)

type atomDoc struct {
	Base    string     `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	Title   string     `xml:"title"`
	Links   []atomLink `xml:"link"`
	Entries []struct {
		ID        string     `xml:"id"`
		Title     string     `xml:"title"`
		Links     []atomLink `xml:"link"`
		Published string     `xml:"published"`
		Updated   string     `xml:"updated"`
		Authors   []struct {
			Name string `xml:"name"`
		} `xml:"author"`
		// media:thumbnail is common on YouTube/Blogger Atom feeds.
		MediaThumbnail []struct {
			URL string `xml:"url,attr"`
		} `xml:"http://search.yahoo.com/mrss/ thumbnail"`
	} `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

func parseAtom(body []byte, feedURL string, now time.Time) (*Feed, error) {
	var doc atomDoc
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	base := firstNonEmpty(doc.Base, feedURL)
	f := &Feed{
		Kind:    KindAtom,
		Title:   CleanTitle(doc.Title),
		SiteURL: resolveRef(base, alternateHref(doc.Links)),
	}
	for _, e := range doc.Entries {
		it := Item{
			Key:   e.ID,
			URL:   resolveRef(base, alternateHref(e.Links)),
			Title: e.Title,
		}
		if t, ok := ParseDate(e.Published); ok {
			it.Published = t
		} else if t, ok := ParseDate(e.Updated); ok {
			it.Published = t
		}
		for _, a := range e.Authors {
			if n := CleanTitle(a.Name); n != "" {
				it.Authors = append(it.Authors, n)
			}
		}
		for _, l := range e.Links {
			if l.Rel == "enclosure" && strings.HasPrefix(l.Type, "image/") {
				it.Image = resolveRef(base, l.Href)
				break
			}
		}
		if it.Image == "" && len(e.MediaThumbnail) > 0 {
			it.Image = e.MediaThumbnail[0].URL
		}
		if finish(&it, feedURL, now) {
			f.Items = append(f.Items, it)
		}
	}
	return f, nil
}

// alternateHref picks the human-facing link: rel="alternate" first, then a
// link with no rel (which Atom defines as alternate), never self/enclosure.
func alternateHref(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "alternate" && l.Href != "" {
			return l.Href
		}
	}
	for _, l := range links {
		if l.Rel == "" && l.Href != "" {
			return l.Href
		}
	}
	return ""
}
