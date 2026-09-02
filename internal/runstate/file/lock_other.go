//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

//go:build !unix

package file

import (
	"fmt"
	"os"
)

// LocksRuns reports whether this build excludes a second process from a run that is
// already open. It is false here: this platform has no flock, so the lock file is
// opened and nobody is excluded. Two processes can hold one run open at once, and
// CheckHeld answers held to both.
//
// A caller reads it to tell that answer from the unix one, where the kernel is behind
// it. Whoever cannot tolerate two writers on one run arranges exclusion elsewhere or
// declines to run.
const LocksRuns = false

// fileLock is a best-effort marker on platforms without flock. It does not
// prevent concurrent access; those platforms rely on the operator not resuming a
// run twice.
type fileLock struct {
	f *os.File
}

func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}

	return &fileLock{f: f}, nil
}

func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}

	l.f.Close()
}
