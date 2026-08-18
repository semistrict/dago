//go:build (!unix || js || wasip1) && !windows

package daplugin

import (
	"context"
	"sync"
	"time"
)

var fallbackStoreLocks sync.Map

func acquireStoreLock(ctx context.Context, path string) (func(), error) {
	value, _ := fallbackStoreLocks.LoadOrStore(path, make(chan struct{}, 1))
	lock := value.(chan struct{})
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	}
}
