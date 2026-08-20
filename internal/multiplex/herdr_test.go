//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package multiplex

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// envOf reads a table rather than the process environment, which is what Detect takes.
func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// herdrEnvironment is what a pane herdr started exports.
func herdrEnvironment() map[string]string {
	return map[string]string{
		herdrEnv:     "1",
		herdrPaneID:  "pane-7",
		herdrBinPath: "/opt/herdr/bin/herdr",
	}
}

// foundOnPath stands in for exec.LookPath, and notFound for a path without herdr on it.
func foundOnPath(name string) (string, error) {
	return "/usr/local/bin/" + name, nil
}

func notFound(string) (string, error) {
	return "", exec.ErrNotFound
}

// commandRecorder stands in for the binary, keeping every command line it was asked to
// run. It locks because the worker goroutine runs them while a spec reads them.
type commandRecorder struct {
	mu   sync.Mutex
	runs [][]string

	// fail, when set, is returned instead of running, so a spec can see what a
	// multiplexer that is not answering costs.
	fail error
	// panics makes the delivery panic, which must not take the process with it.
	panics bool
	// started is closed by the first run, and held is waited on by it, so a spec can
	// hold a delivery open and post over the top of it.
	started chan struct{}
	held    chan struct{}
}

func (c *commandRecorder) run(_ context.Context, bin string, args ...string) error {
	c.mu.Lock()
	c.runs = append(c.runs, append([]string{bin}, args...))
	first := len(c.runs) == 1
	c.mu.Unlock()

	if first && c.started != nil {
		close(c.started)
	}
	if c.held != nil {
		<-c.held
	}
	if c.panics {
		panic("the multiplexer went away")
	}

	return c.fail
}

func (c *commandRecorder) commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, 0, len(c.runs))
	for _, r := range c.runs {
		out = append(out, strings.Join(r, " "))
	}

	return out
}

var _ = Describe("Herdr", func() {
	var (
		rec *commandRecorder
		h   *herdr
	)

	BeforeEach(func() {
		rec = &commandRecorder{}
		h = newHerdr(envOf(herdrEnvironment()), "cowsay", notFound, rec.run)
		Expect(h).ToNot(BeNil())
	})

	Describe("Detection", func() {
		// The switch and the pane are required, so a stale variable or one exported by
		// hand leaves the integration doing nothing at all.
		DescribeTable("Should claim the process only where herdr started it",
			func(vars map[string]string, claimed bool) {
				Expect(newHerdr(envOf(vars), "cowsay", notFound, rec.run) != nil).To(Equal(claimed))
			},
			Entry("a pane herdr started", herdrEnvironment(), true),
			Entry("no herdr at all", map[string]string{}, false),
			Entry("herdr switched off", map[string]string{
				herdrEnv:     "0",
				herdrPaneID:  "pane-7",
				herdrBinPath: "/opt/herdr/bin/herdr",
			}, false),
			Entry("no pane to report on", map[string]string{
				herdrEnv:     "1",
				herdrBinPath: "/opt/herdr/bin/herdr",
			}, false),
		)

		// A released herdr exports the pane and the socket without naming its binary, so
		// requiring the name would leave the integration silent in the terminal it was
		// written for.
		Describe("The binary to report through", func() {
			It("Should take the one the environment names", func() {
				h := newHerdr(envOf(herdrEnvironment()), "cowsay", foundOnPath, rec.run)
				Expect(h.bin).To(Equal("/opt/herdr/bin/herdr"))
			})

			It("Should find it on the path where the environment names none", func() {
				h := newHerdr(envOf(map[string]string{
					herdrEnv:    "1",
					herdrPaneID: "pane-7",
				}), "cowsay", foundOnPath, rec.run)
				Expect(h.bin).To(Equal("/usr/local/bin/herdr"))
			})

			It("Should claim nothing where there is no binary at all", func() {
				Expect(newHerdr(envOf(map[string]string{
					herdrEnv:    "1",
					herdrPaneID: "pane-7",
				}), "cowsay", notFound, rec.run)).To(BeNil())
			})
		})

		// The pane says which agent is in it, so a person watching several is told what
		// each one is rather than being told six times that it is fisk-ai.
		Describe("The agent the pane is labeled with", func() {
			It("Should be the one the caller named", func() {
				Expect(h.agent).To(Equal("cowsay"))
			})

			It("Should fall back to this program where the caller named none", func() {
				h := newHerdr(envOf(herdrEnvironment()), "", notFound, rec.run)
				Expect(h.agent).To(Equal("fisk-ai"))
			})
		})

		It("Should claim nothing outside a pane", func() {
			Expect(Detect(envOf(map[string]string{}), "cowsay")).To(BeNil())
		})

		// The whole path, binary and all: a report is a process this one starts, and the
		// spec above stops one step short of proving that works.
		It("Should report through the binary the environment names", func() {
			bin, err := exec.LookPath("true")
			if err != nil {
				Skip("no true(1) to report through")
			}

			r := Detect(envOf(map[string]string{
				herdrEnv:     "1",
				herdrPaneID:  "pane-7",
				herdrBinPath: bin,
			}), "cowsay")
			Expect(r).ToNot(BeNil())
			Expect(r.Name()).To(Equal("herdr"), "which is what the operator is shown")

			r.Working()
			r.Close()
		})
	})

	Describe("Reporting", func() {
		It("Should name the pane, the source and the state", func() {
			Expect(h.deliver(context.Background(), report{state: stateWorking})).To(Succeed())

			Expect(rec.commands()).To(HaveExactElements(
				"/opt/herdr/bin/herdr pane report-agent pane-7 --source custom:fisk-ai --agent cowsay --state working",
			))
		})

		// Herdr refuses a flag joined to its value, answering "unknown option" and
		// reporting nothing, so each flag and value are separate arguments.
		It("Should keep every flag apart from its value", func() {
			Expect(h.deliver(context.Background(), report{
				state:   stateBlocked,
				message: "--force will drop the table",
			})).To(Succeed())

			Expect(rec.runs[0]).To(HaveExactElements(
				"/opt/herdr/bin/herdr", "pane", "report-agent", "pane-7",
				"--source", "custom:fisk-ai", "--agent", "cowsay", "--state", "blocked",
				"--message", "--force will drop the table",
			))
		})

		It("Should send no message where there is none", func() {
			Expect(h.deliver(context.Background(), report{state: stateIdle})).To(Succeed())

			Expect(rec.commands()[0]).ToNot(ContainSubstring("--message"))
		})

		// Herdr discards a stale sequence from a source it already knows, and the source
		// is fixed, so a run that never released would have the next one's reports thrown
		// away. Nothing here can arrive out of order, so nothing is numbered.
		It("Should number nothing", func() {
			Expect(h.deliver(context.Background(), report{state: stateWorking})).To(Succeed())

			Expect(rec.commands()[0]).ToNot(ContainSubstring("--seq"))
		})

		It("Should give the pane up on release", func() {
			Expect(h.release(context.Background())).To(Succeed())

			Expect(rec.commands()).To(HaveExactElements(
				"/opt/herdr/bin/herdr pane release-agent pane-7 --source custom:fisk-ai --agent cowsay",
			))
		})
	})

	// The binary is a process this one starts, so it has to be one that cannot hold the
	// run up or write over the screen it is drawing on.
	Describe("The command", func() {
		It("Should give up on a binary that does not answer", func() {
			bin, err := exec.LookPath("sleep")
			if err != nil {
				Skip("no sleep(1) to hold a report open")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			started := time.Now()
			Expect(runCommand(ctx, bin, "30")).To(HaveOccurred())
			Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second))
		})

		It("Should report a binary that is not there rather than panic", func() {
			Expect(runCommand(context.Background(), "/nonexistent/herdr", "pane")).To(
				MatchError(ContainSubstring("reporting to the multiplexer")))
		})
	})
})
