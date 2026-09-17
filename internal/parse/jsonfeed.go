package parse

import (
	"encoding/json"
	"strings"
	"time"
)

type jsonDoc struct {
	Title       string `json:"title"`
	HomePageURL string `json:"home_page_url"`
	Items       []struct {
		ID            string `json:"id"`
		URL           string `json:"url"`
		ExternalURL   string `json:"external_url"`
		Title         string `json:"title"`
		DatePublished string `json:"date_published"`
		DateModified  string `json:"date_modified"`
		Image         string `json:"image"`
		BannerImage   string `json:"banner_image"`
		Authors       []struct {
			Name string `json:"name"`
		} `json:"authors"`
		Author *struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"items"`
}

func parseJSONFeed(body []byte, feedURL string, now time.Time) (*Feed, error) {
	var doc jsonDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	f := &Feed{
		Kind:    KindJSON,
		Title:   CleanTitle(doc.Title),
		SiteURL: strings.TrimSpace(doc.HomePageURL),
	}
	for _, ji := range doc.Items {
		it := Item{
			Key:   ji.ID,
			URL:   firstNonEmpty(ji.URL, ji.ExternalURL),
			Title: ji.Title,
			Image: firstNonEmpty(ji.Image, ji.BannerImage),
		}
		if t, ok := ParseDate(ji.DatePublished); ok {
			it.Published = t
		} else if t, ok := ParseDate(ji.DateModified); ok {
			it.Published = t
		}
		for _, a := range ji.Authors {
			if n := CleanTitle(a.Name); n != "" {
				it.Authors = append(it.Authors, n)
			}
		}
		if len(it.Authors) == 0 && ji.Author != nil {
			if n := CleanTitle(ji.Author.Name); n != "" {
				it.Authors = []string{n}
			}
		}
		if finish(&it, feedURL, now) {
			f.Items = append(f.Items, it)
		}
	}
	return f, nil
}
