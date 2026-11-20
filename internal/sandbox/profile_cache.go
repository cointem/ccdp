package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

var ErrBackendUnavailable = errors.New("sandbox backend unavailable")

// VerifyHost checks the ability to install Seatbelt before runtime directories
// are prepared. In an enclosing sandbox, path errors can otherwise hide this
// independent process-level limitation. Only a fixed, side-effect-free probe
// runs here; a successful result shares the bounded profile cache below.
func VerifyHost(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("%w: local execution requires macOS Seatbelt", ErrBackendUnavailable)
	}
	const profile = `(version 1)(deny default)(import "system.sb")(allow file-read*)(allow process-exec (literal "/usr/bin/true"))`
	return VerifyProfile(ctx, profile, "/usr/bin/true")
}

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
		diagnostic := strings.TrimSpace(string(output))
		if strings.Contains(diagnostic, "sandbox_apply: Operation not permitted") || strings.Contains(diagnostic, "sandbox_apply: Permission denied") {
			entry.err = fmt.Errorf("%w: host policy or an enclosing sandbox prevents Seatbelt initialization; run CCDP and its sandbox integration tests from a host terminal outside the enclosing sandbox (adding writable directories cannot fix this): %w (%s)", ErrBackendUnavailable, err, diagnostic)
		} else {
			entry.err = fmt.Errorf("sandbox: Seatbelt rejected profile: %w (%s)", err, diagnostic)
		}
	}
	profileValidations.Lock()
	if entry.err != nil && profileValidations.entries[key] == entry {
		delete(profileValidations.entries, key)
	}
	close(entry.done)
	profileValidations.Unlock()
	return entry.err
}
