package fsops

import (
	"syscall"
	"time"
)

func changeTime(s *syscall.Stat_t) time.Time { return time.Unix(s.Ctimespec.Sec, s.Ctimespec.Nsec) }
