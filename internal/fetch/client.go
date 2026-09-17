package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrTooLarge is returned when a response body exceeds Options.MaxBody.
var ErrTooLarge = errors.New("fetch: response body too large")

// Options tunes the client.
type Options struct {
	Timeout      time.Duration
	MaxBody      int64
	UserAgent    string
	MaxRedirects int
}

// Validators are the conditional-GET headers from the previous fetch.
type Validators struct {
	ETag         string
	LastModified string
}

// Result is a completed fetch. Body is nil on 304.
type Result struct {
	Status       int
	Body         []byte
	ETag         string
	LastModified string
	ContentType  string
	RetryAfter   time.Duration
	NotModified  bool
	FinalURL     string
}

// Client is a guarded HTTP GET client.
type Client struct {
	Timeout   time.Duration
	MaxBody   int64
	UserAgent string
	http      *http.Client
	guard     *Guard
}

// NewClient builds a client whose transport dials only through the guard.
func NewClient(g *Guard, o Options) *Client {
	if o.MaxRedirects == 0 {
		o.MaxRedirects = 5
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	if o.MaxBody == 0 {
		o.MaxBody = 5 << 20
	}
	transport := &http.Transport{
		DialContext:           g.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		// Compressed feeds are common; the transport decodes gzip for us and
		// the body cap below applies to the decoded bytes.
		DisableCompression: false,
	}
	c := &Client{Timeout: o.Timeout, MaxBody: o.MaxBody, UserAgent: o.UserAgent, guard: g}
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= o.MaxRedirects {
				return fmt.Errorf("fetch: stopped after %d redirects", o.MaxRedirects)
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect downgrades https to %s", ErrBlocked, req.URL.Scheme)
			}
			if _, err := g.CheckURL(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	return c
}

// Get performs a guarded conditional GET.
func (c *Client) Get(ctx context.Context, rawURL string, v Validators) (*Result, error) {
	u, err := c.guard.CheckURL(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/json, application/xml, text/xml, text/html;q=0.5, */*;q=0.1")
	if v.ETag != "" {
		req.Header.Set("If-None-Match", v.ETag)
	}
	if v.LastModified != "" {
		req.Header.Set("If-Modified-Since", v.LastModified)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error wraps our guard errors; surface ErrBlocked through errors.Is.
		return nil, err
	}
	defer resp.Body.Close()
	res := &Result{
		Status:       resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		FinalURL:     resp.Request.URL.String(),
	}
	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		return res, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.MaxBody {
		return nil, ErrTooLarge
	}
	res.Body = body
	return res, nil
}

// parseRetryAfter handles both delta-seconds and HTTP-date forms.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
