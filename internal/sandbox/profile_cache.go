package sandbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type profileValidation struct {
	done chan struct{}
	err  error
}

var profileValidations = struct {
	sync.Mutex
	entries map[[32]byte]*profileValidation
}{entries: map[[32]byte]*profileValidation{}}

// VerifyProfile caches successful fixed probes, never a user's command.
// Kernel policy application still occurs on every real process launch.
func VerifyProfile(ctx context.Context, profile string, probe ...string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(probe) == 0 {
		return fmt.Errorf("sandbox: missing profile probe")
	}
	backend, err := os.Stat(BackendPath)
	if err != nil {
		return err
	}
	executable, err := os.Stat(probe[0])
	if err != nil {
		return err
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%q\x00%d/%d\x00%d/%d", profile, probe, backend.ModTime().UnixNano(), backend.Size(), executable.ModTime().UnixNano(), executable.Size())))
	profileValidations.Lock()
	if entry := profileValidations.entries[key]; entry != nil {
		profileValidations.Unlock()
		select {
		case <-entry.done:
			return entry.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(profileValidations.entries) >= 128 {
		for k, entry := range profileValidations.entries {
			select {
			case <-entry.done:
				delete(profileValidations.entries, k)
			default:
			}
		}
	}
	entry := &profileValidation{done: make(chan struct{})}
	// If all slots are still validating, probe without caching. Capacity is
	// fixed even when many different policies arrive concurrently.
	if len(profileValidations.entries) < 128 {
		profileValidations.entries[key] = entry
	}
	profileValidations.Unlock()
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, BackendPath, append([]string{"-p", profile, "--"}, probe...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	if output, err := cmd.CombinedOutput(); err != nil {
		entry.err = fmt.Errorf("sandbox: Seatbelt rejected profile: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	profileValidations.Lock()
	if entry.err != nil && profileValidations.entries[key] == entry {
		delete(profileValidations.entries, key)
	}
	close(entry.done)
	profileValidations.Unlock()
	return entry.err
}
