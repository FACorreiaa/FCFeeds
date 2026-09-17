package discover

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FACorreiaa/FCFeeds/internal/fetch"
)

const rss = `<?xml version="1.0"?><rss version="2.0"><channel><title>Site RSS</title><link>https://s/</link><item><title>x</title><link>https://s/x</link></item></channel></rss>`
const atom = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Site Atom</title><entry><id>1</id><title>y</title></entry></feed>`
const jsonFeed = `{"version":"https://jsonfeed.org/version/1.1","title":"Site JSON","items":[]}`

func newTestDiscoverer(t *testing.T, srv *httptest.Server) *Discoverer {
	t.Helper()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	g := fetch.NewGuard(func(_ context.Context, h string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(host)}, nil
	})
	g.AllowIP = func(ip net.IP) bool { return ip.IsLoopback() }
	g.AllowPort = func(p string) bool { return p == port }
	c := fetch.NewClient(g, fetch.Options{Timeout: 2 * time.Second, MaxBody: 1 << 20, UserAgent: "t"})
	return New(c)
}

func serve(routes map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(body, "<!doctype") || strings.HasPrefix(body, "<html") {
			w.Header().Set("Content-Type", "text/html")
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestDirectFeedURLIsSingleCandidate(t *testing.T) {
	srv := serve(map[string]string{"/rss.xml": rss})
	defer srv.Close()
	got, err := newTestDiscoverer(t, srv).Discover(context.Background(), srv.URL+"/rss.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "rss" || got[0].Title != "Site RSS" || got[0].URL != srv.URL+"/rss.xml" {
		t.Errorf("got %+v", got)
	}
}

func TestHTMLLinkRelAlternateDeclaredFirstAndRelativeResolved(t *testing.T) {
	html := `<!doctype html><html><head>
	<link rel="alternate" type="application/atom+xml" href="/atom.xml" title="Atom">
	<link rel="alternate" type="application/rss+xml" href="rss.xml">
	<link rel="alternate" type="application/feed+json" href="` + "%s" + `/feed.json">
	<link rel="stylesheet" href="/x.css">
	</head><body>hi</body></html>`
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(strings.Replace(html, "%s", srv.URL, 1)))
		case "/atom.xml":
			_, _ = w.Write([]byte(atom))
		case "/blog/rss.xml":
			_, _ = w.Write([]byte(rss))
		case "/feed.json":
			_, _ = w.Write([]byte(jsonFeed))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := newTestDiscoverer(t, srv).Discover(context.Background(), srv.URL+"/blog/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates: %+v", len(got), got)
	}
	if got[0].URL != srv.URL+"/atom.xml" || got[0].Type != "atom" || got[0].Title != "Site Atom" {
		t.Errorf("[0] = %+v", got[0])
	}
	if got[1].URL != srv.URL+"/blog/rss.xml" || got[1].Type != "rss" {
		t.Errorf("[1] relative href not resolved against page: %+v", got[1])
	}
	if got[2].Type != "json" {
		t.Errorf("[2] = %+v", got[2])
	}
}

func TestProbesCommonPathsWhenPageDeclaresNothing(t *testing.T) {
	srv := serve(map[string]string{
		"/":     `<!doctype html><html><head><title>no links</title></head><body></body></html>`,
		"/feed": rss,
	})
	defer srv.Close()
	got, err := newTestDiscoverer(t, srv).Discover(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != srv.URL+"/feed" {
		t.Errorf("got %+v", got)
	}
}

func TestBrokenDeclaredLinkIsDropped(t *testing.T) {
	srv := serve(map[string]string{
		"/":        `<!doctype html><html><head><link rel="alternate" type="application/rss+xml" href="/gone"></head></html>`,
		"/gone":    `<!doctype html><html>not a feed</html>`,
		"/rss.xml": rss,
	})
	defer srv.Close()
	got, err := newTestDiscoverer(t, srv).Discover(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != srv.URL+"/rss.xml" {
		t.Errorf("declared-but-broken should fall through to probes: %+v", got)
	}
}

func TestNothingFoundIsEmptyNotError(t *testing.T) {
	srv := serve(map[string]string{"/": `<!doctype html><html><body>plain</body></html>`})
	defer srv.Close()
	got, err := newTestDiscoverer(t, srv).Discover(context.Background(), srv.URL+"/")
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v err %v", got, err)
	}
}

func TestUnreachablePageIsError(t *testing.T) {
	srv := serve(map[string]string{})
	defer srv.Close()
	if _, err := newTestDiscoverer(t, srv).Discover(context.Background(), srv.URL+"/nope"); err == nil {
		t.Error("404 on the page itself should be an error")
	}
}
