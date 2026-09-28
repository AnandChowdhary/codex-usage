//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("locking accounts: %w", err)
		}
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			f.Close()
			return nil, fmt.Errorf("waiting for another codex-usage to finish: %w", err)
		}
	}
}
