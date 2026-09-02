//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestA2A(t *testing.T) {
	RegisterFailHandler(Fail)

	// The specs here wait on a shell the served tool starts and on the goroutine that
	// owns a reply set, and go test runs packages in parallel, so Gomega's one second
	// measures the machine's load rather than this code. Waiting longer costs nothing
	// when the assertion holds, since Eventually returns as soon as it is satisfied.
	SetDefaultEventuallyTimeout(30 * time.Second)

	RunSpecs(t, "A2A")
}
