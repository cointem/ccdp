package tools

import (
	"context"
	"net"
	"testing"
)

func TestValidateExternalURLBlocksInternalAddresses(t *testing.T) {
	blocked := []string{
		"http://localhost/x",
		"http://LOCALHOST:8080/x",
		"http://metadata/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://instance-data/x",
		"http://127.0.0.1/x",
		"http://127.8.8.8/x",
		"http://10.0.0.1/x",
		"http://172.16.0.1/x",
		"http://172.31.255.255/x",
		"http://192.168.1.1/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/x",
		"http://[::1]/x",
		"http://[fe80::1]/x",
		"http://[fc00::1]/x",
		"http://[fd12::1]/x",
		"file:///etc/passwd",
	}
	for _, raw := range blocked {
		if err := validateExternalURL(context.Background(), raw); err == nil {
			t.Errorf("expected block for %q", raw)
		}
	}
	allowed := []string{
		"https://example.com/x",
		"https://8.8.8.8/dns-query",
		"https://100.64.0.1/x", // shared address space, not RFC1918
	}
	for _, raw := range allowed {
		if err := validateExternalURLWithLookup(context.Background(), raw, func(ctx context.Context, host string) ([]net.IP, error) {
			if host == "example.com" {
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			}
			return resolveHostIPs(ctx, host)
		}); err != nil {
			t.Errorf("unexpected block for %q: %v", raw, err)
		}
	}
}

func TestHTTPGetBlocksPrivateTarget(t *testing.T) {
	// No network needed: the address check fires before any dial.
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:9000/admin",
	} {
		if _, _, _, _, err := httpGetDetails(context.Background(), raw); err == nil {
			t.Errorf("expected httpGet to block %q", raw)
		}
	}
}

func TestExternalURLRejectsMixedPublicPrivateDNSAnswers(t *testing.T) {
	err := validateExternalURLWithLookup(context.Background(), "https://public-name.example/x", func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")}, nil
	})
	if err == nil {
		t.Fatal("public host name bypassed private DNS address check")
	}
}
