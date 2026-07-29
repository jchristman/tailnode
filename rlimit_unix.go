//go:build linux || darwin

package main

import "syscall"

// darwinOpenMax is the ceiling Darwin enforces on RLIMIT_NOFILE regardless of
// the reported hard limit.
const darwinOpenMax = 10240

// raiseFileLimit raises the soft RLIMIT_NOFILE toward the hard limit and
// returns the effective soft and hard limits.
func raiseFileLimit() (soft, hard uint64, err error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, 0, err
	}
	soft, hard = lim.Cur, lim.Max
	if soft >= hard {
		return soft, hard, nil
	}

	// Darwin rejects a soft limit above OPEN_MAX even when the hard limit is
	// reported as unlimited, so fall back to that ceiling.
	for _, want := range []uint64{hard, darwinOpenMax} {
		if want <= soft {
			continue
		}
		try := lim
		try.Cur = want
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &try); err == nil {
			return want, hard, nil
		}
	}
	return soft, hard, nil
}
