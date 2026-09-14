package agent

import (
	"net/url"
	"os"
	"strings"
)

func maskKey(k string) string {
	if k == "" {
		return "(not set)"
	}
	if len(k) <= 8 {
		return "••••"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

func probeWritableDir(path, pattern string) (bool, string) {
	if strings.TrimSpace(path) == "" {
		return false, "directory is not configured"
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, path + " (does not exist)"
	}
	if !st.IsDir() {
		return false, path + " (not a directory)"
	}
	f, err := os.CreateTemp(path, pattern)
	if err != nil {
		return false, path
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if closeErr != nil || removeErr != nil {
		return false, path
	}
	return true, path
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return raw
	}
	if u.User != nil {
		u.User = url.User("••••")
	}
	if q := u.Query(); len(q) > 0 {
		for key := range q {
			upper := strings.ToUpper(key)
			if strings.Contains(upper, "KEY") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") {
				q.Set(key, "••••")
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
