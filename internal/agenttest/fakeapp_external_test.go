//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"
)

var _ = Describe("FakeApp", func() {
	streamApp := func() *fisk.Application {
		app := fisk.New("stream", "manages streams")
		app.Command("stream", "stream commands").Command("rm", "removes a stream")

		return app
	}

	It("Should replay the genuine introspection of the application on --fisk-introspect", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), streamApp())

		out, err := exec.Command(app.Path, "--fisk-introspect").Output()
		Expect(err).ToNot(HaveOccurred())

		// The agent reads this document over the process boundary and turns it into the
		// tools it offers, so a model that loads here is the model production sees.
		var model fisk.ApplicationModel
		Expect(json.Unmarshal(out, &model)).To(Succeed())

		tools, err := fisktool.ApplicationTools(&model)
		Expect(err).ToNot(HaveOccurred())
		Expect(tools).To(HaveLen(1))
		Expect(tools[0].Name()).To(Equal("stream_rm"))
	})

	It("Should report its working directory and echo its arguments one per line", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), streamApp())

		dir := GinkgoT().TempDir()
		cmd := exec.Command(app.Path, "stream", "rm", "orders")
		cmd.Dir = dir

		out, err := cmd.Output()
		Expect(err).ToNot(HaveOccurred())

		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		Expect(lines[0]).To(HavePrefix("PWD="))
		Expect(lines[1:]).To(Equal([]string{"stream", "rm", "orders"}))
	})

	// One executable per distinct command model, so a suite standing up the same
	// application in many tests writes one file rather than one per test.
	It("Should share one executable between applications with the same command model", func() {
		first := agenttest.NewFakeApp(GinkgoTB(), streamApp())
		second := agenttest.NewFakeApp(GinkgoTB(), streamApp())

		Expect(second.Path).To(Equal(first.Path))
	})

	It("Should write a separate executable for a different command model", func() {
		other := fisk.New("other", "does something else")
		other.Command("consumer", "consumer commands").Command("ls", "lists consumers")

		Expect(agenttest.NewFakeApp(GinkgoTB(), other).Path).
			ToNot(Equal(agenttest.NewFakeApp(GinkgoTB(), streamApp()).Path))
	})

	// Reaching this line at all is the assertion on the no-op Terminate: fisk's default
	// one calls os.Exit from the --fisk-introspect parse the constructor makes, which
	// would end the test binary rather than return an application here.
	It("Should write an executable file", func() {
		fake, err := agenttest.BuildFakeApp(streamApp())
		Expect(err).ToNot(HaveOccurred())

		info, err := os.Stat(fake.Path)
		Expect(err).ToNot(HaveOccurred())
		Expect(info.Mode().Perm() & 0o100).ToNot(BeZero())
	})
})
