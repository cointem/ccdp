package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/jsonvalue"
	"ccdp/internal/protocol"
)

// cloneConfig deep-copies mutable maps/slices and intentionally keeps no
// secret-bearing runtime pointers. It is used for step and model binding
// snapshots so /reload cannot mutate a request in flight.
func cloneConfig(src *config.Config) config.Config {
	if src == nil {
		return config.Config{}
	}
	return src.Clone()
}

func (a *Agent) configSnapshot() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneConfig(a.services.cfg)
}

func cloneMap(src map[string]any) map[string]any {
	return jsonvalue.Clone(src)
}

var _ protocol.SessionClient = (*Agent)(nil)
