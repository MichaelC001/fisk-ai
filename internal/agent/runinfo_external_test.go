//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// RunInfo is what an embedder renders a startup banner from, so its counts have to
// describe the run rather than one source of tools within it.
var _ = Describe("RunInfo", func() {
	// injectedTool is a custom tool the model is offered, reachable only through
	// Options.CustomTools.
	injectedTool := func(name string) toolkit.Tool {
		tool, err := functool.New(functool.Spec{
			Name:        name,
			Description: "reports the state of the shift",
			Schema:      map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
				return "quiet", nil
			},
		})
		Expect(err).ToNot(HaveOccurred())

		return tool
	}

	It("Should count the built-ins and the injected tools a run with no application offers", func() {
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		events := agenttest.NewRecordingEvents()

		// A nil app is the run that wraps no application, so every tool the model sees
		// comes from the memory built-ins and the one the caller injected.
		cfg := agenttest.Config(GinkgoTB(), nil, agenttest.WithMemory())

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"how did the shift go"},
			Provider:    provider,
			StoreDir:    GinkgoT().TempDir(),
			CustomTools: []toolkit.Tool{injectedTool("shift_report")},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())
		Expect(res).ToNot(BeNil())

		starts := events.Starts()
		Expect(starts).To(HaveLen(1))
		Expect(starts[0].NoApplication).To(BeTrue())

		// The provider records what it was sent, so the count is checked against the
		// tools the model actually received rather than against a number this spec
		// worked out for itself.
		reqs := provider.Requests()
		Expect(reqs).ToNot(BeEmpty())
		Expect(reqs[0].Tools).ToNot(BeEmpty())
		Expect(starts[0].Tools).To(Equal(len(reqs[0].Tools)))

		names := make([]string, 0, len(reqs[0].Tools))
		for _, t := range reqs[0].Tools {
			names = append(names, t.Name)
		}
		Expect(names).To(ContainElement("shift_report"))
	})
})
