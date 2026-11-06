package fsops

import "golang.org/x/sys/unix"

// Darwin's sys/fcntl.h defines O_SEARCH as O_EXEC (0x40000000) | O_DIRECTORY.
// x/sys does not expose it yet. Pin traversal descriptors without requesting
// directory contents; Seatbelt can then permit metadata while denying listings.
const directorySearchFlags = 0x40000000 | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
