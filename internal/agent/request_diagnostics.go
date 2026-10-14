package agent

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	requestAuthErrorRE = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+)?|api[_-]?key\s*[:=]\s*|token\s*[:=]\s*)([^\s,;"'}]+)`)
	requestURLRE       = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
)

func safeAttemptError(err error, secret, endpoint string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	// Do not persist URL userinfo or query strings from url.Error. Endpoint
	// itself has already been stripped in the manifest; this handles provider
	// diagnostics that repeat the original unsanitized URL.
	s = requestURLRE.ReplaceAllStringFunc(s, func(rawURL string) string {
		parsed, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return "[redacted-url]"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
	if endpoint != "" {
		if parsed, parseErr := url.Parse(endpoint); parseErr == nil {
			parsed.User = nil
			parsed.RawQuery = ""
			parsed.ForceQuery = false
			parsed.Fragment = ""
			s = strings.ReplaceAll(s, endpoint, parsed.String())
		}
	}
	s = requestAuthErrorRE.ReplaceAllString(s, `${1}[redacted]`)
	if len(s) > 4096 {
		s = s[:4096] + "…"
	}
	return s
}

func sanitizeRequestEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("agent: invalid provider endpoint: %w", err)
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	result := strings.TrimSpace(u.String())
	if strings.ContainsAny(result, "\r\n") {
		return "", errors.New("agent: provider endpoint contains a newline")
	}
	return result, nil
}
