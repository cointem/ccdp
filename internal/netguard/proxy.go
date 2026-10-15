package netguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TrustedProxy accepts only an explicitly configured loopback HTTP proxy.
// Its operator owns target DNS and address filtering. Environment variables
// and macOS proxy settings are deliberately not consulted.
func TrustedProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("network_proxy must be an HTTP loopback proxy without credentials, path or query")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("network_proxy requires a loopback IP and explicit port (for example http://127.0.0.1:7897)")
	}
	return u, nil
}

// ValidateTarget checks names and literal addresses without locally resolving
// names. A trusted upstream resolves them, including when host DNS is Fake-IP.
func ValidateTarget(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("blocked: only HTTP(S) destinations without URL credentials are allowed")
	}
	if _, err := url.Parse(u.String()); err != nil {
		return fmt.Errorf("blocked: invalid destination URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || strings.ContainsAny(host, "%\\\x00") {
		return fmt.Errorf("blocked: invalid destination hostname")
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := publicIP(host, u.Port(), ip); err != nil {
			return err
		}
	} else {
		if !strings.Contains(host, ".") || host == "metadata.google.internal" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
			return fmt.Errorf("blocked: local/internal destination hostname")
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return fmt.Errorf("blocked: invalid destination hostname")
			}
		}
		// Reject ambiguous numeric spellings accepted as IPs by some proxies.
		numeric := true
		for _, c := range host {
			if c != '.' && (c < '0' || c > '9') {
				numeric = false
			}
		}
		lastLabel := host[strings.LastIndexByte(host, '.')+1:]
		if len(lastLabel) > 0 && (lastLabel[0] >= '0' && lastLabel[0] <= '9') {
			numeric = true
		}
		for _, c := range host {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
				return fmt.Errorf("blocked: invalid destination hostname")
			}
		}
		if numeric {
			return fmt.Errorf("blocked: ambiguous IP address")
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("blocked: invalid destination port")
		}
	}
	return nil
}

// RoutedTransport never falls back to direct access if the configured proxy
// is unavailable. Proxy mode must be paired with ValidateTarget on every URL.
func RoutedTransport(proxy string) (*http.Transport, error) {
	if proxy == "" {
		return PublicTransport(), nil
	}
	u, err := TrustedProxy(proxy)
	if err != nil {
		return nil, err
	}
	tr, err := OriginTransport(proxy)
	if err != nil {
		return nil, err
	}
	tr.Proxy = http.ProxyURL(u)
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	return tr, nil
}

// Gateway is a per-process policy proxy. Sandboxed processes receive only its
// loopback address; the upstream endpoint stays outside their network policy.
type Gateway struct {
	URL       string
	Port      int
	server    *http.Server
	transport *http.Transport
	upstream  *url.URL
	mu        sync.Mutex
	closed    bool
	tunnels   map[net.Conn]bool
}

func StartGateway(upstream string) (*Gateway, error) {
	u, err := TrustedProxy(upstream)
	if err != nil {
		return nil, err
	}
	tr, err := RoutedTransport(upstream)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("start network policy proxy: %w", err)
	}
	g := &Gateway{URL: "http://" + ln.Addr().String(), Port: ln.Addr().(*net.TCPAddr).Port, transport: tr, upstream: u, tunnels: map[net.Conn]bool{}}
	forward := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.Header.Del("Proxy-Authorization")
			p.Out.Header.Del("Proxy-Connection")
		},
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "trusted network proxy unavailable", http.StatusBadGateway)
		},
	}
	g.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Upgrade") != "" {
				http.Error(w, "use CONNECT for upgraded connections", http.StatusBadRequest)
				return
			}
			target := r.URL
			if r.Method == http.MethodConnect {
				if _, port, err := net.SplitHostPort(r.Host); err != nil || port == "" {
					http.Error(w, "invalid CONNECT destination", http.StatusBadRequest)
					return
				}
				target = &url.URL{Scheme: "https", Host: r.Host}
			}
			if err := ValidateTarget(target); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			if r.Method == http.MethodConnect {
				g.connect(w, r)
				return
			}
			// Host and absolute-form URL must describe the same destination.
			r.Host = target.Host
			forward.ServeHTTP(w, r)
		}),
	}
	go func() { _ = g.server.Serve(ln) }()
	return g, nil
}

func (g *Gateway) track(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		c.Close()
		return false
	}
	g.tunnels[c] = true
	return true
}
func (g *Gateway) untrack(c net.Conn) {
	c.Close()
	g.mu.Lock()
	delete(g.tunnels, c)
	g.mu.Unlock()
}

func (g *Gateway) connect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	conn, err := g.transport.DialContext(ctx, "tcp", g.upstream.Host)
	if err != nil {
		http.Error(w, "trusted network proxy unavailable", http.StatusBadGateway)
		return
	}
	if !g.track(conn) {
		http.Error(w, "network policy proxy closed", http.StatusServiceUnavailable)
		return
	}
	defer g.untrack(conn)
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: r.Host}, Host: r.Host, Header: make(http.Header)}
	if err = req.Write(conn); err != nil {
		http.Error(w, "upstream CONNECT failed", http.StatusBadGateway)
		return
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		http.Error(w, "upstream CONNECT failed", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		http.Error(w, "upstream refused CONNECT", http.StatusBadGateway)
		return
	}
	client, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	if !g.track(client) {
		return
	}
	defer g.untrack(client)
	conn.SetDeadline(time.Time{})
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = rw.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(conn, rw); conn.Close(); close(done) }()
	_, _ = io.Copy(client, br)
	client.Close()
	conn.Close()
	<-done
}

func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	for c := range g.tunnels {
		c.Close()
	}
	g.mu.Unlock()
	_ = g.server.Close()
	g.transport.CloseIdleConnections()
}
