// Package netguard builds direct HTTP transports whose actual TCP dial is
// constrained independently of an earlier URL/DNS preflight. Transports do
// not consult proxy environment variables, so a host proxy or Unix socket can
// never become an unreviewed network path.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PublicTransport dials resolved public Internet addresses directly. It
// rejects the whole DNS answer if any returned address is private, loopback,
// unspecified, or link-local, and pins each connection to a checked IP.
func PublicTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: checkedDial(publicIP)}
}

// OriginTransport dials only the configured HTTP(S) origin, directly and
// without a proxy. It permits host-local endpoints because callers must have
// separately authorized the configured remote MCP connection.
func OriginTransport(rawOrigin string) (*http.Transport, error) {
	u, err := url.Parse(strings.TrimSpace(rawOrigin))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid HTTP origin %q", rawOrigin)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return nil, fmt.Errorf("invalid HTTP origin port %q", port)
	}
	want := net.JoinHostPort(host, port)
	return &http.Transport{Proxy: nil, DialContext: checkedDial(func(string, string, net.IP) error { return nil }, want)}, nil
}

type addressPolicy func(host, port string, ip net.IP) error

func checkedDial(policy addressPolicy, onlyOrigin ...string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("network %q is not allowed", network)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("invalid TCP address %q", address)
		}
		if len(onlyOrigin) != 0 {
			wantHost, wantPort, splitErr := net.SplitHostPort(onlyOrigin[0])
			if splitErr != nil || !strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(wantHost, ".")) || port != wantPort {
				return nil, fmt.Errorf("network destination %q is outside the configured origin", address)
			}
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if err := policy(host, port, ip); err != nil {
				return nil, err
			}
		}
		dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		var lastErr error
		for _, ip := range ips {
			if network == "tcp4" && ip.To4() == nil || network == "tcp6" && ip.To4() != nil {
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no address for %s is compatible with %s", host, network)
		}
		return nil, lastErr
	}
}

func lookup(ctx context.Context, host string) ([]net.IP, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []net.IP{net.IP(addr.AsSlice())}, nil
	}
	answers, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	ips := make([]net.IP, 0, len(answers))
	seen := map[string]bool{}
	for _, answer := range answers {
		if answer.IP == nil || seen[answer.IP.String()] {
			continue
		}
		seen[answer.IP.String()] = true
		ips = append(ips, answer.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %q: no addresses", host)
	}
	return ips, nil
}

func publicIP(_ string, _ string, ip net.IP) error {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("invalid resolved IP %q", ip)
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("blocked: request to internal/private address")
	}
	return nil
}
