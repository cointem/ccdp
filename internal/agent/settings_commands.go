package agent

import (
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (a *Agent) applyPermissionPolicy(cmd protocol.Command, owned ...bool) protocol.Receipt {
	p := cmd.PermissionPolicy.Policy
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		mode := permissions.Mode(target.PermissionPolicy)
		if p.Mode != "" {
			parsed, err := permissions.ParseMode(p.Mode)
			if err != nil {
				return err
			}
			mode = parsed
		}
		allow := target.AlwaysAllow
		if p.AlwaysAllow != nil {
			allow = p.AlwaysAllow
		}
		deny := target.AlwaysDeny
		if p.AlwaysDeny != nil {
			deny = p.AlwaysDeny
		}
		target.PermissionPolicy = string(mode)
		target.AlwaysAllow = cloneStringSlice(allow)
		target.AlwaysDeny = cloneStringSlice(deny)
		return nil
	}, owned...)
}

func (a *Agent) applySandboxPolicy(cmd protocol.Command, owned ...bool) protocol.Receipt {
	p := cmd.SandboxPolicy.Policy
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		target.NetworkAccess = p.NetworkAccess
		target.LocalDevelopmentNetwork = p.LocalDevelopmentNetwork
		target.AdditionalDirectories = cloneStringSlice(p.AdditionalDirectories)
		target.AdditionalReadOnlyDirectories = cloneStringSlice(p.AdditionalReadOnlyDirectories)
		target.DisallowedDirectories = cloneStringSlice(p.DisallowedDirectories)
		return nil
	}, owned...)
}
