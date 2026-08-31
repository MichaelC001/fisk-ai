//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgentExamples(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Examples/Agent")
}
