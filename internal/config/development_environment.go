package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/atomicfile"
)

const developmentBoundaryVersion = 1

type developmentBoundary struct {
	Version           int `json:"version"`
	Network, LocalTCP bool
	Writable          []string
}

func (c Config) developmentBoundary() developmentBoundary {
	policy := c.Sandbox().Snapshot()
	roots := append([]string{policy.Workspace}, policy.AdditionalDirs...)
	for i, root := range roots {
		if normalized, err := NormalizeProjectRoot(root); err == nil {
			roots[i] = normalized
		}
	}
	return developmentBoundary{Version: developmentBoundaryVersion, Network: c.NetworkAccess, LocalTCP: c.LocalDevelopmentNetwork, Writable: roots}
}
func (c Config) developmentBoundaryPath() (string, error) {
	permission, _, err := c.projectPermissionPath()
	return filepath.Join(filepath.Dir(permission), "development-environment.json"), err
}
func (c Config) DevelopmentEnvironmentAccepted() (bool, error) {
	path, err := c.developmentBoundaryPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var accepted developmentBoundary
	if err := json.Unmarshal(data, &accepted); err != nil {
		return false, err
	}
	current := c.developmentBoundary()
	if accepted.Version != current.Version || current.Network && !accepted.Network || current.LocalTCP && !accepted.LocalTCP {
		return false, nil
	}
	for _, root := range current.Writable {
		covered := false
		for _, parent := range accepted.Writable {
			rel, err := filepath.Rel(parent, root)
			if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				covered = true
				break
			}
		}
		if !covered {
			return false, nil
		}
	}
	return true, nil
}
func (c Config) AcceptDevelopmentEnvironment() error {
	path, err := c.developmentBoundaryPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.developmentBoundary(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0600)
}
func (c Config) DevelopmentEnvironmentDescription() string {
	b := c.developmentBoundary()
	return fmt.Sprintf("workspace writes: %s; host reads except protected credentials/session data; outbound network: %t; local TCP connect/listen: %t; no host Unix sockets or global installs", strings.Join(b.Writable, ", "), b.Network, b.LocalTCP)
}
