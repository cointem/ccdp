package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ccdp/internal/atomicfile"
	"ccdp/internal/permissions"
)

type projectPermission struct {
	Root string `json:"root"`
	Mode string `json:"mode"`
}

// PermissionProjectRoot groups Git subdirectories; ordinary folders remain independent.
func PermissionProjectRoot(workspace string) (string, error) {
	root, err := NormalizeProjectRoot(workspace)
	if err != nil {
		return "", err
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return root, nil
}

func (c Config) projectPermissionPath() (string, string, error) {
	root, err := PermissionProjectRoot(c.Workspace)
	if err != nil {
		return "", "", err
	}
	home := filepath.Dir(c.SessionDir)
	if c.SessionDir == "" && c.sourcePath != "" {
		home = filepath.Dir(c.sourcePath)
	}
	if home == "." {
		home = filepath.Dir(ConfigPath())
	}
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(home, "project-permissions", hex.EncodeToString(digest[:])+".json"), root, nil
}

func (c Config) SaveProjectPermission(mode string) error {
	parsed, err := permissions.ParseMode(mode)
	if err != nil {
		return err
	}
	if parsed == permissions.ModePlan {
		return fmt.Errorf("plan is not a project permission policy")
	}
	path, root, err := c.projectPermissionPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(projectPermission{Root: root, Mode: string(parsed)}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0600)
}

func (c *Config) ApplyProjectPermission() error {
	if c.SourceOf("permission_mode") == SourceCLI || c.SourceOf("permission_mode") == SourceEnvironment {
		return nil
	}
	path, root, err := c.projectPermissionPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved projectPermission
	if err = json.Unmarshal(data, &saved); err != nil {
		return err
	}
	mode, err := permissions.ParseMode(saved.Mode)
	if err != nil {
		return err
	}
	if saved.Root != root || mode == permissions.ModePlan {
		return fmt.Errorf("invalid project permission record: %s", path)
	}
	// Plan is an independent workflow; preserve its explicit startup selection.
	if c.PermissionMode != string(permissions.ModePlan) {
		c.PermissionMode = string(mode)
	}
	return nil
}
