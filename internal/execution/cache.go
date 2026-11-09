package execution

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
)

// BindDevelopmentCache gives equally authorized owners of one project the
// same cache directory. Scratch, credentials, logs and executor state remain
// owner-private. Package managers retain their own content/toolchain keys.
func BindDevelopmentCache(policy *sandbox.Sandbox) {
	if policy == nil {
		return
	}
	c := controllerFor(policy.ExecutorID)
	if c == nil {
		return
	}
	c.mu.Lock()
	root := c.privateRoot
	c.mu.Unlock()
	if root == "" {
		return
	}
	p := policy.Snapshot()
	canonicalPaths := make(map[string]string)
	canonical := func(path string) string {
		if resolved, ok := canonicalPaths[path]; ok {
			return resolved
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			canonicalPaths[path] = resolved
			return resolved
		}
		clean := filepath.Clean(path)
		canonicalPaths[path] = clean
		return clean
	}
	roots := func(paths []string, omitPrivateChildren bool) []string {
		out := make([]string, 0, len(paths))
		for _, path := range paths {
			path = canonical(path)
			private := false
			if omitPrivateChildren {
				for _, parent := range p.PrivateRoots {
					rel, err := filepath.Rel(canonical(parent), path)
					if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						private = true
						break
					}
				}
			}
			if !private {
				out = append(out, path)
			}
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	services := append([]sandbox.LocalService(nil), p.LocalServices...)
	slices.SortFunc(services, func(a, b sandbox.LocalService) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return strings.Compare(string(left), string(right))
	})
	scope := struct {
		Workspace, Platform, Proxy                                        string
		Write, ReadOnly, Denied, Credentials, Private, Protected, Entries []string
		Network, LocalDevelopment                                         bool
		Services                                                          []sandbox.LocalService
	}{Workspace: canonical(p.Workspace), Platform: runtime.GOOS + "/" + runtime.GOARCH, Proxy: p.NetworkProxy,
		Write: roots(p.AdditionalDirs, false), ReadOnly: roots(p.ReadOnlyDirs, true), Denied: roots(p.DisallowedDirs, false),
		Credentials: roots(p.CredentialDirs, false), Private: roots(p.PrivateRoots, false), Protected: roots(p.ProtectedDirs, true), Entries: roots(p.ProtectedEntries, true),
		Network: p.AllowNetwork, LocalDevelopment: p.LocalDevelopmentNetwork, Services: services}
	data, _ := json.Marshal(scope)
	hash := sha256.Sum256(data)
	policy.SetCacheDir(filepath.Join(root, "cache", "v1", fmt.Sprintf("%x", hash)))
}

func prepareDevelopmentCache(policy *sandbox.Sandbox) (string, error) {
	BindPrivateRoots(policy)
	BindDevelopmentCache(policy)
	dir := policy.Snapshot().CacheDir
	if dir == "" {
		return "", ErrEnvironmentUnavailable
	}
	return runtimepath.EnsurePrivate(dir)
}
