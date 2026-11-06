package main

import (
	"fmt"
	"io"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
)

// Application consent happens once before constructing an executable runtime.
// Library callers provide an already-authorized Config at their own boundary.
func prepareDevelopmentEnvironment(cfg *config.Config, input io.Reader, output io.Writer, interactive bool) error {
	if err := cfg.ApplyProjectPermission(); err != nil {
		return err
	}
	if cfg.PermissionMode != string(permissions.ModeAuto) && !cfg.LocalDevelopmentNetwork {
		return nil
	}
	accepted, err := cfg.DevelopmentEnvironmentAccepted()
	if err != nil {
		return err
	}
	if accepted {
		return nil
	}
	if cfg.SourceOf("permission_mode") == config.SourceCLI && cfg.PermissionMode == string(permissions.ModeAuto) {
		return cfg.AcceptDevelopmentEnvironment()
	}
	if !interactive {
		return fmt.Errorf("development environment requires one-time acceptance: %s. Run interactively, or pass --mode auto to accept this boundary; or explicitly configure local_development_network=false with manual mode", cfg.DevelopmentEnvironmentDescription())
	}
	fmt.Fprintf(output, "Development environment for %s:\n%s\nAllow automatic development in this environment? [y/N] ", cfg.Workspace, cfg.DevelopmentEnvironmentDescription())
	var answer string
	if _, err := fmt.Fscanln(input, &answer); err != nil && err != io.EOF {
		answer = ""
	}
	if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		return cfg.AcceptDevelopmentEnvironment()
	}
	// Declining leaves a usable session under manual approval.
	cfg.LocalDevelopmentNetwork = false
	return cfg.ApplyCLIOverride("permission_mode", string(permissions.ModeManual))
}
