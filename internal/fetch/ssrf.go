// Package fetch is the outbound HTTP client. Every request goes through an
// SSRF guard that validates the URL, resolves the host and dials only vetted
// public addresses, re-checking on each redirect hop.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrBlocked is returned when a URL or one of its redirect hops points at
// something we refuse to fetch.
var ErrBlocked = errors.New("fetch: blocked by ssrf guard")

// Resolver resolves a hostname. Tests inject a fake.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// Guard decides which URLs and IPs are reachable.
type Guard struct {
	Resolve Resolver
	// AllowIP defaults to IsPublicIP. Tests loosen it to reach loopback.
	AllowIP func(net.IP) bool
	// AllowPort defaults to 80/443 only.
	AllowPort func(string) bool
}

// NewGuard returns a guard with production defaults and the given resolver
// (nil means the system resolver).
func NewGuard(r Resolver) *Guard {
	if r == nil {
		r = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, nil
		}
	}
	return &Guard{
		Resolve:   r,
		AllowIP:   IsPublicIP,
		AllowPort: func(p string) bool { return p == "80" || p == "443" },
	}
}

var blockedSuffixes = []string{".internal", ".svc", ".cluster.local", ".local", ".localhost"}

// CheckURL validates scheme, userinfo, port and hostname, then resolves the
// host and requires every returned address to be allowed. It returns the
// parsed URL and the vetted IPs.
func (g *Guard) CheckURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q", ErrBlocked, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo in url", ErrBlocked)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrBlocked)
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	if !g.AllowPort(port) {
		return nil, fmt.Errorf("%w: port %s", ErrBlocked, port)
	}
	if host == "localhost" {
		return nil, fmt.Errorf("%w: host %s", ErrBlocked, host)
	}
	for _, suf := range blockedSuffixes {
		if strings.HasSuffix(host, suf) {
			return nil, fmt.Errorf("%w: host %s", ErrBlocked, host)
		}
	}
	if _, err := g.vetHost(ctx, host); err != nil {
		return nil, err
	}
	return u, nil
}

// vetHost resolves and checks every address. One bad address fails the host:
// a resolver returning a public and a private answer is a rebinding attempt.
func (g *Guard) vetHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !g.AllowIP(ip) {
			return nil, fmt.Errorf("%w: ip %s", ErrBlocked, ip)
		}
		return []net.IP{ip}, nil
	}
	ips, err := g.Resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: %s resolved to nothing", ErrBlocked, host)
	}
	for _, ip := range ips {
		if !g.AllowIP(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlocked, host, ip)
		}
	}
	return ips, nil
}

// DialContext resolves at dial time and connects to a vetted IP, so a DNS
// answer that changed between CheckURL and the connection cannot slip through.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !g.AllowPort(port) {
		return nil, fmt.Errorf("%w: port %s", ErrBlocked, port)
	}
	ips, err := g.vetHost(ctx, strings.ToLower(host))
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

var privateV4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
}

var privateNets []*net.IPNet

func init() {
	for _, c := range privateV4 {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		privateNets = append(privateNets, n)
	}
}

// IsPublicIP reports whether an address is safe to connect to from inside
// the cluster: not loopback, private, link-local, ULA, multicast or unspecified.
func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsPrivate() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range privateNets {
			if n.Contains(v4) {
				return false
			}
		}
		return true
	}
	// IPv6: ULA is covered by IsPrivate; also refuse documentation and
	// IPv4-mapped forms, which To4 already unwrapped above.
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return false // 2001:db8::/32 documentation
	}
	return true
}
