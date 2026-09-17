package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsPublicIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "::1", "fe80::1", "fc00::1", "fd12::1", "::ffff:10.0.0.1", "::"}
	for _, s := range blocked {
		if IsPublicIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "151.101.1.69", "2606:4700::1111", "172.32.0.1", "100.128.0.1"}
	for _, s := range allowed {
		if !IsPublicIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func fakeResolver(m map[string][]string) Resolver {
	return func(_ context.Context, host string) ([]net.IP, error) {
		addrs, ok := m[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

func TestGuardCheckURL(t *testing.T) {
	g := NewGuard(fakeResolver(map[string][]string{
		"good.example":  {"93.184.216.34"},
		"evil.example":  {"10.0.0.5"},
		"mixed.example": {"93.184.216.34", "127.0.0.1"},
	}))
	ctx := context.Background()
	bad := []string{
		"ftp://good.example/feed",
		"https://user:pw@good.example/feed",
		"http://good.example:8080/feed",
		"http://localhost/feed",
		"http://api.horus.svc.cluster.local/feed",
		"http://metadata.internal/feed",
		"http://10.0.0.1/feed",
		"http://[::1]/feed",
		"http://evil.example/feed",
		"http://mixed.example/feed",
		"not a url",
		"",
	}
	for _, u := range bad {
		if _, err := g.CheckURL(ctx, u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
	if _, err := g.CheckURL(ctx, "https://good.example/feed.xml"); err != nil {
		t.Errorf("good url rejected: %v", err)
	}
}

// testClient allows the httptest loopback address but nothing else private,
// so redirect-to-private tests still bite.
func testClient(t *testing.T, srv *httptest.Server, extra map[string][]string) *Client {
	t.Helper()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	res := map[string][]string{host: {host}}
	for k, v := range extra {
		res[k] = v
	}
	g := NewGuard(fakeResolver(res))
	g.AllowIP = func(ip net.IP) bool { return ip.IsLoopback() }
	g.AllowPort = func(p string) bool { return p == port || p == "80" || p == "443" }
	return NewClient(g, Options{Timeout: 2 * time.Second, MaxBody: 1024, UserAgent: "feeds-test/1"})
}

func TestGetSendsValidatorsAndReturnsHeaders(t *testing.T) {
	var gotUA, gotINM, gotIMS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotINM, gotIMS = r.UserAgent(), r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", "Wed, 17 Sep 2026 10:00:00 GMT")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte("<rss/>"))
	}))
	defer srv.Close()
	c := testClient(t, srv, nil)
	res, err := c.Get(context.Background(), srv.URL+"/feed", Validators{ETag: `"v1"`, LastModified: "Tue, 16 Sep 2026 10:00:00 GMT"})
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "feeds-test/1" || gotINM != `"v1"` || gotIMS != "Tue, 16 Sep 2026 10:00:00 GMT" {
		t.Errorf("request headers: ua=%q inm=%q ims=%q", gotUA, gotINM, gotIMS)
	}
	if res.Status != 200 || string(res.Body) != "<rss/>" || res.ETag != `"v2"` || res.LastModified == "" || res.ContentType != "application/rss+xml" {
		t.Errorf("result = %+v", res)
	}
}

func TestGetNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	res, err := testClient(t, srv, nil).Get(context.Background(), srv.URL, Validators{ETag: "x"})
	if err != nil || !res.NotModified || res.Status != 304 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestGetBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 5000))
	}))
	defer srv.Close()
	_, err := testClient(t, srv, nil).Get(context.Background(), srv.URL, Validators{})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("want ErrTooLarge, got %v", err)
	}
}

func TestGetRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	res, err := testClient(t, srv, nil).Get(context.Background(), srv.URL, Validators{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 429 || res.RetryAfter != 2*time.Minute {
		t.Errorf("res=%+v", res)
	}
}

func TestGetRedirectToPrivateIsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example/", http.StatusFound)
	}))
	defer srv.Close()
	c := testClient(t, srv, map[string][]string{"evil.example": {"10.0.0.9"}})
	_, err := c.Get(context.Background(), srv.URL, Validators{})
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("want ErrBlocked, got %v", err)
	}
}

func TestGetTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()
	c := testClient(t, srv, nil)
	c.Timeout = 100 * time.Millisecond
	if _, err := c.Get(context.Background(), srv.URL, Validators{}); err == nil {
		t.Error("expected timeout error")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("30", time.Now()); d != 30*time.Second {
		t.Errorf("seconds: %v", d)
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if d := parseRetryAfter("Thu, 17 Sep 2026 12:05:00 GMT", now); d != 5*time.Minute {
		t.Errorf("http-date: %v", d)
	}
	if d := parseRetryAfter("garbage", now); d != 0 {
		t.Errorf("garbage: %v", d)
	}
}
