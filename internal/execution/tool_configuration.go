package execution

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/fsops"
	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
)

// Configuration copies contain a deliberately small set of development
// settings. Credential helpers, tokens, includes and arbitrary command hooks
// are never copied. Project configuration remains inside the project sandbox.
func prepareToolConfiguration(env []string, policy *sandbox.Sandbox) ([]string, error) {
	c := controllerFor(policy.ExecutorID)
	if c == nil {
		return nil, ErrEnvironmentUnavailable
	}
	home := environmentValue(env, "HOME")
	read := func(path string) string {
		f, err := fsops.OpenAuthorizedRegular("", path, policy.ResolveConfigurationRead)
		if err != nil {
			return ""
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 128<<10))
		if err != nil || len(b) >= 128<<10 {
			return ""
		}
		return string(b)
	}
	configs := []struct{ key, kind, source string }{
		{"NPM_CONFIG_USERCONFIG", "npm", filepath.Join(home, ".npmrc")},
		{"PIP_CONFIG_FILE", "pip", filepath.Join(home, ".config/pip/pip.conf")},
		{"GIT_CONFIG_GLOBAL", "git", filepath.Join(home, ".gitconfig")},
	}
	goenv := environmentValue(env, "GOENV")
	if goenv != "off" {
		if goenv == "" {
			goenv = filepath.Join(home, "Library/Application Support/go/env")
		}
		configs = append(configs, struct{ key, kind, source string }{"GOENV", "go", goenv})
	}
	for _, cfg := range configs {
		source := cfg.source
		if explicit := environmentValue(env, cfg.key); explicit != "" {
			source = explicit
		}
		data := filterToolConfiguration(cfg.kind, read(source))
		path, err := c.configurationFile(cfg.kind, data)
		if err != nil {
			return nil, err
		}
		policy.AddRuntimeReadFile(path)
		env = setEnvironment(env, cfg.key, path)
		if cfg.kind == "npm" {
			env = setEnvironment(env, "npm_config_userconfig", path)
		}
	}
	env = setEnvironment(env, "GIT_CONFIG_NOSYSTEM", "1")
	return env, nil
}

func (c *Controller) configurationFile(kind, data string) (string, error) {
	if err := c.ensure(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	hash := sha256.Sum256([]byte(data))
	dir, err := runtimepath.EnsurePrivate(filepath.Join(c.root, "config"))
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%x.conf", kind, hash[:16]))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if os.IsExist(err) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	_, err = io.WriteString(f, data)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	return path, closeErr
}

func filterToolConfiguration(kind, data string) string {
	var out strings.Builder
	section := ""
	writtenSection := ""
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.TrimSpace(strings.Trim(line, "[]")))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		lower := strings.ToLower(key)
		allowed := false
		switch kind {
		case "npm":
			allowed = lower == "registry" || strings.HasSuffix(lower, ":registry") || lower == "strict-ssl" || lower == "fund" || lower == "audit"
		case "pip":
			allowed = (section == "global" || section == "install") && (lower == "index-url" || lower == "extra-index-url" || lower == "timeout" || lower == "retries")
		case "git":
			allowed = section == "user" && (lower == "name" || lower == "email") || section == "init" && lower == "defaultbranch" || section == "core" && (lower == "autocrlf" || lower == "eol" || lower == "ignorecase") || section == "pull" && lower == "rebase" || section == "fetch" && lower == "prune"
		case "go":
			allowed = key == "GOPROXY" || key == "GOSUMDB" || key == "GOPRIVATE" || key == "GONOSUMDB" || key == "GONOPROXY" || key == "GOTOOLCHAIN" || key == "GO111MODULE"
		}
		if !allowed || strings.ContainsAny(value, "\x00\r\n$") {
			continue
		}
		sensitive := false
		for _, word := range strings.FieldsFunc(strings.Trim(value, "\"'"), func(r rune) bool { return r == ',' || r == '|' || r == ' ' }) {
			if strings.Contains(word, "://") {
				u, err := url.Parse(word)
				if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
					sensitive = true
					break
				}
			}
		}
		if sensitive {
			continue
		}
		if (kind == "git" || kind == "pip") && section != writtenSection {
			fmt.Fprintf(&out, "[%s]\n", section)
			writtenSection = section
		}
		if kind == "go" {
			fmt.Fprintf(&out, "%s=%s\n", key, value)
		} else {
			fmt.Fprintf(&out, "%s = %s\n", key, value)
		}
	}
	return out.String()
}
