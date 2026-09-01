//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentTest(t *testing.T) {
	RegisterFailHandler(Fail)

	// The specs here wait on fakes driven from goroutines of their own, and go test runs
	// packages in parallel, so Gomega's one second measures the machine's load rather
	// than this code. Eventually returns as soon as it is satisfied, so a longer wait
	// costs nothing when the assertion holds.
	SetDefaultEventuallyTimeout(30 * time.Second)

	RunSpecs(t, "Internal/AgentTest")
}
