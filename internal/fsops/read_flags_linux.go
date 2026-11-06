package fsops

import "golang.org/x/sys/unix"

const directorySearchFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
