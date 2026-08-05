//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"io"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/telemetry"
)

func TestBootstrap(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/Telemetry/Bootstrap")
}

// The specs that drive a refused export would otherwise print through
// OpenTelemetry's default handler, which writes to the log package's own os.Stderr and
// so lands in the suite's output rather than anywhere a spec can see. That the SDK does
// this at all is the reason a terminal-owning program installs a destination; here it
// is only noise, so it goes to io.Discard.
var _ = BeforeSuite(func() {
	telemetry.SetErrorHandler(io.Discard)
})

// noEnv is the environment a spec resolves against: none. Reading the developer's shell
// would make these specs pass or fail on the machine rather than on the code, which is
// why Resolve takes an env reader at all.
func noEnv(string) string { return "" }

// envWith returns an env reader answering only for the given names.
func envWith(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}
