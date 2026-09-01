//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

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
		// BuildFakeApp has no testing.TB to skip with, so it reports Windows as an error.
		if runtime.GOOS == "windows" {
			Skip("the fake application is a /bin/sh script")
		}

		fake, err := agenttest.BuildFakeApp(streamApp())
		Expect(err).ToNot(HaveOccurred())

		info, err := os.Stat(fake.Path)
		Expect(err).ToNot(HaveOccurred())
		Expect(info.Mode().Perm() & 0o100).ToNot(BeZero())
	})

	// ToolsForApp runs the binary to introspect it, so this drives the fake the way the
	// agent's LoadTools path does rather than by decoding its output here.
	It("Should satisfy ToolsForApp, which runs the binary to introspect it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), streamApp())

		tools, err := fisktool.ToolsForApp(context.Background(), app.Path, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(tools).To(HaveLen(1))
		Expect(tools[0].Name()).To(Equal("stream_rm"))
		Expect(tools[0].AppPath()).To(Equal(app.Path))

		res, err := tools[0].RunCommand(context.Background(), json.RawMessage(`{}`), GinkgoT().TempDir())
		Expect(err).ToNot(HaveOccurred())
		Expect(res.ExitCode).To(Equal(0))
		Expect(res.Output).To(ContainSubstring("stream\nrm"))
	})

	// RunCommand sets cmd.Dir and PWD to the directory it was handed, and the run reads
	// the tool's output to tell one run's work directory from another's, so the fake has
	// to report the spelling it was given rather than the symlink-resolved one os.Getwd
	// returns.
	It("Should report the working directory it was handed, symlinks unresolved", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), streamApp())

		tools, err := fisktool.ToolsForApp(context.Background(), app.Path, nil)
		Expect(err).ToNot(HaveOccurred())

		dir := GinkgoT().TempDir()
		res, err := tools[0].RunCommand(context.Background(), json.RawMessage(`{}`), dir)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Output).To(ContainSubstring("PWD=" + dir))
	})

	// A goroutine printing to stdout while the model is captured would corrupt it the same
	// way, but only when it wins the race. A PreAction prints from inside app.Parse, ahead
	// of fisk's introspect action, so the extra bytes reach the pipe on every run.
	It("Should refuse a model that another writer to stdout corrupted", func() {
		if runtime.GOOS == "windows" {
			Skip("the fake application is a /bin/sh script")
		}

		app := streamApp()
		app.PreAction(func(*fisk.ParseContext) error {
			fmt.Println("output from elsewhere in the test process")
			return nil
		})

		_, err := agenttest.BuildFakeApp(app)
		Expect(err).To(MatchError(ContainSubstring("printed to stdout during the parse")))
	})

	// The introspection swaps the process os.Stdout, since fisk's introspect action
	// writes there and takes no writer, so BuildFakeApp serializes it. Under -race this
	// fails if that lock is dropped.
	It("Should build fake applications from several goroutines at once", func() {
		if runtime.GOOS == "windows" {
			Skip("the fake application is a /bin/sh script")
		}

		const workers = 8

		paths := make([]string, workers)
		errs := make([]error, workers)

		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				app := fisk.New(fmt.Sprintf("app%d", i), "concurrent build")
				app.Command("stream", "stream commands").Command(fmt.Sprintf("rm%d", i), "removes a stream")

				fake, err := agenttest.BuildFakeApp(app)
				errs[i] = err
				if err == nil {
					paths[i] = fake.Path
				}
			}()
		}
		wg.Wait()

		seen := map[string]bool{}
		for i := range workers {
			Expect(errs[i]).ToNot(HaveOccurred())
			Expect(seen[paths[i]]).To(BeFalse(), "distinct models shared a path")
			seen[paths[i]] = true

			out, err := exec.Command(paths[i], "--fisk-introspect").Output()
			Expect(err).ToNot(HaveOccurred())

			var model fisk.ApplicationModel
			Expect(json.Unmarshal(out, &model)).To(Succeed())
			Expect(model.Name).To(Equal(fmt.Sprintf("app%d", i)))
		}
	})
})
