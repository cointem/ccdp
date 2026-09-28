package fsops

import (
	"syscall"
	"time"
)

func changeTime(s *syscall.Stat_t) time.Time { return time.Unix(s.Ctim.Sec, s.Ctim.Nsec) }
