package netguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrustedProxyAndTargets(t *testing.T) {
	for _, raw := range []string{"http://example.com:80", "socks5://127.0.0.1:7897", "http://127.0.0.1", "http://user:pass@127.0.0.1:80", "http://127.0.0.1:0", "http://127.0.0.1:80/path"} {
		if _, err := TrustedProxy(raw); err == nil {
			t.Errorf("accepted proxy %q", raw)
		}
	}
	for _, raw := range []string{"http://localhost/", "http://localhost./", "http://localhost../", "http://a..localhost../", "http://foo.local/", "http://metadata.google.internal/", "http://127.0.0.1/", "http://[::1]/", "http://10.0.0.1/", "http://198.18.0.63/", "http://2130706433/", "http://127.1/", "http://0x7f.0.0.1/", "file:///tmp/a"} {
		u, _ := url.Parse(raw)
		if err := ValidateTarget(u); err == nil {
			t.Errorf("accepted destination %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:7897", "http://[::1]:7897"} {
		if _, err := TrustedProxy(raw); err != nil {
			t.Error(err)
		}
	}
}

func TestGatewayForwardsNamesWithoutLocalDNSAndBlocksPrivate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "public.invalid" {
			t.Errorf("upstream target=%q", r.URL.String())
		}
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("client proxy credentials leaked")
		}
		fmt.Fprint(w, "via trusted upstream")
	}))
	defer upstream.Close()
	g, err := StartGateway(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	proxy, _ := url.Parse(g.URL)
	tr := &http.Transport{Proxy: http.ProxyURL(proxy)}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", "http://public.invalid/path", nil)
	req.Header.Set("Proxy-Authorization", "secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "via trusted upstream" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	for _, target := range []string{"http://127.0.0.1/", "http://metadata.google.internal/"} {
		resp, err = client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("private target got %d", resp.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("blocked requests reached upstream: %d", calls.Load())
	}
}

func TestGatewayConnectAndClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "public.invalid:443" {
			t.Errorf("unexpected CONNECT %s %s", r.Method, r.Host)
			http.Error(w, "bad", 400)
			return
		}
		conn, rw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString(line)
		rw.Flush()
		io.Copy(io.Discard, rw)
	}))
	defer upstream.Close()
	g, err := StartGateway(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	u, _ := url.Parse(g.URL)
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "CONNECT public.invalid:443 HTTP/1.1\r\nHost: public.invalid:443\r\n\r\n")
	br := bufio.NewReader(conn)
	req := &http.Request{Method: "CONNECT"}
	resp, err := http.ReadResponse(br, req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	fmt.Fprint(conn, "hello\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("tunnel: %q %v", line, err)
	}
	g.Close()
	_, err = br.ReadByte()
	if err == nil {
		t.Fatal("gateway close left tunnel open")
	}
}

func TestProxyDialCannotEscapeOrigin(t *testing.T) {
	tr, err := RoutedTransport("http://127.0.0.1:7897")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "outside the configured origin") {
		t.Fatalf("direct bypass: %v", err)
	}
}
