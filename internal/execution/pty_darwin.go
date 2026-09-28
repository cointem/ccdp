//go:build darwin

package execution

import (
	"golang.org/x/sys/unix"
	"os"
	"unsafe"
)

func openProcessPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			master.Close()
		}
	}()
	for _, op := range []uintptr{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), op, 0)
		if errno != 0 {
			err = errno
			return
		}
	}
	var name [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		err = errno
		return
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	slave, err = os.OpenFile(string(name[:end]), os.O_RDWR|unix.O_NOCTTY, 0)
	return
}
