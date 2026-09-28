//go:build !unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// staleLockAge is how old a lock file must be before it is assumed abandoned
// by a crashed process. Every locked section is a few HTTP calls at most.
const staleLockAge = 2 * time.Minute

// lockFile uses an exclusively-created lock file where flock is unavailable.
func lockFile(ctx context.Context, path string) (func(), error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("locking accounts: %w", err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > staleLockAge {
			os.Remove(path)
			continue
		}
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return nil, fmt.Errorf("waiting for another codex-usage to finish: %w", err)
		}
	}
}
