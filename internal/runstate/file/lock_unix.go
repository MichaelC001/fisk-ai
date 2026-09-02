//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

//go:build unix

package file

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

// LocksRuns reports whether this build excludes a second process from a run that is
// already open. It is true here: Open takes an exclusive flock on a per-run lock file,
// so a second opener is refused with runstate.ErrLocked and CheckHeld's structural
// answer rests on the kernel.
//
// A caller reads it to tell that answer from an unconditional one. Where it is false
// the store still opens, still answers held, and excludes nobody, so whoever cannot
// tolerate two writers on one run arranges exclusion elsewhere or declines to run.
const LocksRuns = true

// fileLock is an advisory flock on a per-run lock file. The kernel releases it
// automatically when the process exits, so a crashed run leaves no stale lock.
type fileLock struct {
	f *os.File
}

func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, runstate.ErrLocked
		}
		return nil, fmt.Errorf("locking run: %w", err)
	}

	return &fileLock{f: f}, nil
}

func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}

	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
