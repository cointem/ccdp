package config

import (
	"fmt"
	"strings"
)

type DeliveryCheck struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Optional       bool     `json:"optional,omitempty"`
}
type DeliveryConfig struct {
	Checks []DeliveryCheck `json:"checks,omitempty"`
	Remote string          `json:"remote,omitempty"`
	Base   string          `json:"base,omitempty"`
}

func (c DeliveryConfig) Clone() DeliveryConfig {
	c.Checks = append([]DeliveryCheck(nil), c.Checks...)
	for i := range c.Checks {
		c.Checks[i].Argv = append([]string(nil), c.Checks[i].Argv...)
	}
	return c
}
func (c DeliveryConfig) Validate() error {
	if strings.HasPrefix(c.Remote, "-") || strings.HasPrefix(c.Base, "-") || strings.ContainsAny(c.Remote+c.Base, "\x00\n\r") {
		return fmt.Errorf("invalid delivery remote or base")
	}
	seen := map[string]bool{}
	for _, v := range c.Checks {
		if seen[v.Name] {
			return fmt.Errorf("duplicate delivery check %q", v.Name)
		}
		seen[v.Name] = true
		if v.Name == "" || len(v.Argv) == 0 || v.Argv[0] == "" || v.TimeoutSeconds < 0 {
			return fmt.Errorf("invalid delivery check %q", v.Name)
		}
	}
	return nil
}

// Zero values select conservative defaults; overrides remain explicit.
type ReviewConfig struct {
	FileBytes      int64 `json:"file_bytes,omitempty"`
	TotalBytes     int64 `json:"total_bytes,omitempty"`
	MaxPaths       int   `json:"max_paths,omitempty"`
	PrepareSeconds int   `json:"prepare_seconds,omitempty"`
}

func (c ReviewConfig) Limits() ReviewConfig {
	if c.FileBytes == 0 {
		c.FileBytes = 32 << 20
	}
	if c.TotalBytes == 0 {
		c.TotalBytes = 512 << 20
	}
	if c.MaxPaths == 0 {
		c.MaxPaths = 100000
	}
	if c.PrepareSeconds == 0 {
		c.PrepareSeconds = 30
	}
	return c
}
func (c ReviewConfig) Validate() error {
	c = c.Limits()
	if c.FileBytes < 1 || c.TotalBytes < c.FileBytes || c.MaxPaths < 1 || c.PrepareSeconds < 1 || c.PrepareSeconds > 3600 {
		return fmt.Errorf("invalid review resource limits")
	}
	return nil
}
