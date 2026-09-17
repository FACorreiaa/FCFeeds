// Package fetchtest builds fetch clients that can reach an httptest server
// (loopback) while still refusing every other private address, so tests of
// redirect and SSRF behaviour keep their teeth.
package fetchtest

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FACorreiaa/FCFeeds/internal/fetch"
)

// NewLoopbackClient returns a client that resolves every hostname to the
// httptest server's address unless extra maps it elsewhere, and only allows
// loopback IPs on the server's port.
func NewLoopbackClient(t *testing.T, srv *httptest.Server, extra map[string][]string) *fetch.Client {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	g := fetch.NewGuard(func(_ context.Context, h string) ([]net.IP, error) {
		if addrs, ok := extra[h]; ok {
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, net.ParseIP(a))
			}
			return ips, nil
		}
		return []net.IP{net.ParseIP(host)}, nil
	})
	g.AllowIP = func(ip net.IP) bool { return ip.IsLoopback() }
	g.AllowPort = func(p string) bool { return p == port }
	return fetch.NewClient(g, fetch.Options{Timeout: 2 * time.Second, MaxBody: 1 << 20, UserAgent: "feeds-test/1"})
}
