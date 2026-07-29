//go:build !linux && !darwin

package main

// raiseFileLimit is a no-op on platforms without RLIMIT_NOFILE. The reported
// budget falls back to a conservative default so auto-derived limits stay sane.
func raiseFileLimit() (soft, hard uint64, err error) {
	return 4096, 4096, nil
}
