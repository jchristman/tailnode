package main

import (
	"context"

	"golang.org/x/sync/semaphore"
)

var dialSem *semaphore.Weighted

func initDialLimiter(maxConcurrent int64) {
	if maxConcurrent <= 0 {
		return
	}
	dialSem = semaphore.NewWeighted(maxConcurrent)
}

func acquireDialSlot() {
	if dialSem != nil {
		_ = dialSem.Acquire(context.Background(), 1)
	}
}

func releaseDialSlot() {
	if dialSem != nil {
		dialSem.Release(1)
	}
}
