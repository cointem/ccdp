package execution

import (
	"errors"
	"golang.org/x/sys/unix"
)

// kqueue observes exit without reaping the child. A child already exited at
// registration has no live proc to attach to (ESRCH); its wait status remains.
func waitProcessExit(pid int) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	_, err = unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(fd, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
}
