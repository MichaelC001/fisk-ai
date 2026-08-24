//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/pii"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/telemetry/bootstrap"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	fisktool "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
	"github.com/choria-io/fisk-ai/internal/util"
	"github.com/choria-io/ui/columns"
	"github.com/choria-io/ui/table"
)

// maxInfoDescriptionLen is the width at which tool descriptions are truncated in
// the info table before an ellipsis is appended.
const maxInfoDescriptionLen = 50

func registerInfoAction(cmd *fisk.Application) {
	info := cmd.Command("info", "Shows the tools and prompt loaded from a configuration").Action(infoAction)
	info.Flag("config", "Path to the agent configuration file").Default("agent.yaml").StringVar(&configFile)
	info.Flag("no-color", "Disable markdown rendering of the prompt, emitting raw text").Envar("NO_COLOR").UnNegatableBoolVar(&noColor)
}

// infoAction reports, without contacting the LLM, the tools that the
// configuration resolves to and the system prompt that would be sent.
//
// The config is validated in ModeMCP, the most lenient mode: info introspects a
// configuration without running it, so it must work for an MCP-only config that
// carries no prompt or model as well as for a full agent config. Requiring a
// model or prompt here, as ModeAgent does, would reject a valid MCP config it is
// meant to inspect.
func infoAction(_ *fisk.ParseContext) error {
	ctx, cancel := interruptContext()
	defer cancel()

	cfg, err := config.ParseConfigFileForMode(configFile, config.ModeMCP)
	if err != nil {
		return err
	}

	if cfg.ApplicationPath == "" && cfg.AppToolFiltersConfigured() {
		fmt.Fprintln(os.Stderr, "warning: include/exclude have no effect without application_path; they filter the wrapped application's tools")
	}

	tools, err := fisktool.LoadTools(ctx, cfg)
	if err != nil {
		return err
	}

	// The memory and knowledge tools are enumerated with a nil store: info only needs
	// their names and descriptions, and never invokes a handler.
	hitlTools := builtin.HITLTools(cfg)
	memTools := builtin.MemoryTools(cfg, nil)
	ragTools := builtin.RAGTools(cfg, nil)

	// Names already claimed by local tools and the built-ins, so remote tools are
	// named (and prefixed on clash) exactly as a run would name them.
	taken := make(map[string]bool, len(tools))
	for _, t := range tools {
		taken[t.Name()] = true
	}
	for _, b := range hitlTools {
		taken[b.Name()] = true
	}
	for _, b := range memTools {
		taken[b.Name()] = true
	}
	for _, b := range ragTools {
		taken[b.Name()] = true
	}

	// Discover remote tools best-effort: info must stay usable offline and when a
	// remote agent is down, so a connection or discovery failure is reported as a
	// warning and the local tools are still shown.
	imports, err := remotetools.DiscoverForInfo(cfg, taken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot connect to NATS context %q to discover remote tools: %v\n", cfg.NatsContext, err)
	}

	// The names the a2a import settled on are never written into taken, so the MCP
	// naming pass is given both lookups, as a run gives it both. Passing taken alone
	// would let an MCP tool take a name an imported remote tool already answers to,
	// and info would then show a name no run would use.
	remoteByName := make(map[string]*functool.Tool)
	for _, imp := range imports {
		for _, rt := range imp.Tools {
			remoteByName[rt.Name()] = rt
		}
	}

	// Discover the configured MCP servers best-effort, for the same reason the remote
	// hosts are: a server that will not answer is reported with its error and the rest
	// of the configuration is still shown. Nothing is connected when none are declared.
	mcpImports := mcpclient.DiscoverForInfo(ctx, mcpclient.Options{
		Servers:            cfg.MCPServers,
		Identity:           cfg.Identity,
		Version:            util.Version(),
		CredentialEnvNames: cfg.CredentialEnvNames(),
	}, mcpclient.NewClaimedNames(taken, remoteByName))

	totalTools := len(tools) + len(hitlTools) + len(memTools) + len(ragTools)
	for _, imp := range imports {
		totalTools += len(imp.Tools)
	}
	for _, imp := range mcpImports {
		totalTools += len(imp.Tools)
	}

	c := columns.New()
	defer c.WriteTo(os.Stdout)

	printModelSection(c, cfg, totalTools)
	printMemorySection(c, cfg)
	printSessionsSection(c, cfg)
	printPIISection(c, cfg)
	printTelemetrySection(c, cfg)

	tbl := table.NewTableWriter("")
	tbl.AddHeaders("Tool", "Source", "Confirm", "Description", "Tags")
	// The Confirm column marks the commands a run would gate behind operator
	// confirmation, so an author can see confirm_tags resolves to the commands they
	// expect rather than discovering a typo (an unmatched tag) only mid-run. Only the
	// introspected local tools carry tags; the built-ins and remote tools are not gated
	// here, so their cell stays blank.
	// A tag that does nothing renders in the Tags column exactly like one that works,
	// so the tags that are misspelled or contradictory are called out under the table
	// rather than left for the operator to spot.
	var tagIssues []string
	for _, t := range tools {
		confirm := ""
		if t.NeedsConfirm(cfg.ConfirmTags()) {
			confirm = "Yes"
		}
		tbl.AddRow(t.Name(), "local", confirm, util.TruncateString(t.Description(), maxInfoDescriptionLen), strings.Join(t.Tags(), ", "))

		unknown, conflicting := toolkit.TagIssues(t)
		if len(unknown) > 0 {
			tagIssues = append(tagIssues, fmt.Sprintf("%s carries unknown reserved tag(s) %s, which do nothing; the recognized tags are %s", t.Name(), strings.Join(unknown, ", "), strings.Join(toolkit.ReservedTags(), ", ")))
		}
		if len(conflicting) > 0 {
			tagIssues = append(tagIssues, fmt.Sprintf("%s carries contradictory behavior tags %s; the more dangerous reading is used", t.Name(), strings.Join(conflicting, ", ")))
		}
	}
	// Built-in human-in-the-loop tools are not introspected from the application,
	// so list them too when enabled, to show the full tool set a run would expose.
	// They carry no tags.
	for _, b := range hitlTools {
		tbl.AddRow(b.Name(), "local", "", util.TruncateString(b.Description(), maxInfoDescriptionLen), "")
	}
	// Built-in memory tools are likewise not introspected from the application, so
	// list them when enabled to show the full tool set a run would expose.
	for _, b := range memTools {
		tbl.AddRow(b.Name(), "local", "", util.TruncateString(b.Description(), maxInfoDescriptionLen), "")
	}
	// The built-in knowledge tools, likewise, when RAG is enabled.
	for _, b := range ragTools {
		tbl.AddRow(b.Name(), "local", "", util.TruncateString(b.Description(), maxInfoDescriptionLen), "")
	}
	// Imported remote tools are listed with the host alias as their source, so the
	// provenance of a tool the prompt may reference is clear.
	for _, imp := range imports {
		alias := imp.Host.EffectiveAlias()
		for _, rt := range imp.Tools {
			// A remote tool's description is the serving agent's model-facing
			// description, which has the command's tags appended as a trailing
			// "Tags:" block. Split that back out so the table shows a clean,
			// single-line description and the tags in their own column, matching the
			// local rows.
			desc, tags := splitRemoteDescription(rt.Description())
			desc = strings.ReplaceAll(desc, "\n", " ")
			tbl.AddRow(rt.Name(), alias, "", util.TruncateString(desc, maxInfoDescriptionLen), tags)
		}
	}
	addMCPToolRows(tbl, mcpImports)

	c.Section("Tools", func(c *columns.Document) {
		c.Embed(tbl)
	})

	for _, issue := range tagIssues {
		fmt.Fprintf(os.Stderr, "warning: %s\n", issue)
	}

	printRemoteToolStatus(c, cfg, imports)
	printMCPServerStatus(c, mcpImports)

	// List the application's exposable global flags so an operator can see which
	// exist and which they have allowlisted under global_flags, closing the loop
	// between "what can I expose" and the error a bad name would raise at load.
	globals, err := fisktool.AppGlobalFlags(ctx, cfg)
	if err != nil {
		return err
	}

	if len(globals) > 0 {
		c.Blank()
		c.Section("Exposable application global flags (add names under global_flags to expose to the model)", func(c *columns.Document) {
			keys := make([]string, len(globals))
			for i, g := range globals {
				marker := ""
				switch {
				case g.Required:
					marker = " [exposed, required]"
				case g.Exposed:
					marker = " [exposed]"
				}
				keys[i] = g.Name + marker
			}
			c.Blank()

			for i, g := range globals {
				c.Item(keys[i], g.Help)
			}
		})
	}

	c.Section("Prompt", func(c *columns.Document) {
		if len(cfg.SystemPrompt) > 0 {
			c.Print(util.RenderAnswer(cfg.SystemPrompt, noColor))
		} else {
			c.Print("No system_prompt defined")
		}
	})

	return nil
}

// printModelSection shows the resolved model, provider, thinking state and how tool
// search will behave, so an operator can confirm the backend and the feature gates
// without starting a run. It is skipped for a config with no model (an MCP-only config
// parsed in ModeMCP), which has no LLM run to describe.
//
// totalTools is how many tools a run would put in front of the model, which decides
// whether the tool search state costs anything worth reporting.
func printModelSection(c *columns.Document, cfg *config.Config, totalTools int) {
	if cfg.LLM.Model == "" {
		return
	}

	c.Section("Model", func(c *columns.Document) {
		c.Item("Model", cfg.LLM.Model)
		c.Item("Provider", cfg.LLMProvider())

		// Three states, and the two that are not "enabled" are worth telling apart here:
		// one sends nothing and leaves the model to itself, the other asks it to stop.
		// An operator whose model reasons anyway needs to know which one they configured.
		thinking := "provider default"
		switch {
		case cfg.ThinkingEnabled():
			thinking = "enabled"
		case cfg.ThinkingDisabled():
			thinking = "disabled"
		}
		c.Item("Thinking", thinking)

		c.Item("Tool search", toolSearchStatus(cfg, totalTools))
	})
}

// printMemorySection shows the memory backend and, for the jetstream backend, the
// KV bucket, the NATS context it lives on, and the key prefix that namespaces this
// agent's memories, so an operator can confirm where memory is stored (and whether
// it is shared with other agents) without starting a run. It resolves everything
// from config and never dials NATS, so it works offline. It is skipped when memory
// is disabled.
func printMemorySection(c *columns.Document, cfg *config.Config) {
	if !cfg.MemoryEnabled() {
		return
	}

	c.Section("Memory", func(c *columns.Document) {
		backend := cfg.MemoryBackend()
		c.Item("Backend", backend)

		if backend == memory.BackendJetStream {
			var opts struct {
				Bucket string  `json:"bucket"`
				Prefix *string `json:"prefix"`
			}
			// Best-effort: info is a display surface, so a decode error (an operator's
			// typo the run path rejects strictly) leaves the fields blank rather than
			// failing the listing.
			_ = json.Unmarshal(cfg.MemoryRawOptions(), &opts)

			bucket := opts.Bucket
			if bucket == "" {
				bucket = "(not set)"
			}
			c.Item("Bucket", bucket)

			context := cfg.NatsContext
			if context == "" {
				context = "(default)"
			}
			c.Item("Context", context)

			prefix := cfg.Identity
			if opts.Prefix != nil {
				prefix = *opts.Prefix
			}
			if prefix == "" {
				prefix = "none (keys shared flat with other agents on this bucket)"
			}
			c.Item("Prefix", prefix)
		}

		c.Item("Limits", fmt.Sprintf("%d KB per entry, %d entries", memory.MaxContentBytes/1024, memory.MaxEntries))
	})
}

// printSessionsSection shows where checkpointed run journals are stored, the parallel
// of the Memory section, so an operator can confirm the session backend without
// starting a run. Like the Memory section it resolves everything from config and never
// dials NATS, so it works offline. It is gated on a model being set (the same rule as
// the Model section): an MCP-only config has no run to checkpoint, so it stays quiet.
func printSessionsSection(c *columns.Document, cfg *config.Config) {
	if cfg.LLM.Model == "" {
		return
	}

	c.Section("Sessions", func(c *columns.Document) {
		backend := cfg.SessionBackend()
		c.Item("Backend", backend)

		switch backend {
		case runstate.BackendJetStream:
			var opts struct {
				Stream string `json:"stream"`
			}
			// Best-effort, like the Memory section: a decode error (an operator's typo the
			// run path rejects strictly) leaves the field blank rather than failing info.
			_ = json.Unmarshal(cfg.SessionRawOptions(), &opts)

			stream := opts.Stream
			if stream == "" {
				stream = "(not set)"
			}
			c.Item("Stream", stream)

			context := cfg.NatsContext
			if context == "" {
				context = "(default)"
			}
			c.Item("Context", context)

			// The subject prefix is derived by binding the stream at connect time (its
			// single wildcard subject), so it is not knowable offline; naming that here
			// mirrors Memory's Prefix so the omission reads as by-design, not a bug.
			c.Item("Prefix", "(derived from the stream at connect time)")
		case runstate.BackendFile:
			var opts struct {
				Directory string `json:"directory"`
			}
			_ = json.Unmarshal(cfg.SessionRawOptions(), &opts)

			directory := opts.Directory
			if directory == "" {
				directory = "(XDG default)"
			}
			c.Item("Directory", directory)
		}
	})
}

// printPIISection shows what a run would scan for personal data and what it would do
// with what it found, the parallel of the Memory and Sessions sections and resolved the
// same way, from config alone.
//
// It prints in every mode, including off, where Memory prints in none. PII is the one
// harness feature that acts without being asked for and the one that changes what the
// model receives, so "is my data being scanned?" is a question this has to answer rather
// than leave to a missing section, which is also what a misspelled key looks like.
//
// It says plainly what detection is worth. An operator who reads redaction as a guarantee
// will send data somewhere on the strength of it.
func printPIISection(c *columns.Document, cfg *config.Config) {
	c.Section("PII scanning", func(c *columns.Document) {
		mode := cfg.PIIMode()
		c.Item("Mode", mode)

		if mode == config.PIIModeOff {
			c.Item("Scanned", "nothing")

			return
		}

		c.Item("Scanned", "prompts and tool results, before the model, the session store or telemetry see them")
		c.Item("Not scanned", "the system prompt, memory, the model's replies, tool arguments and resumed history")
		c.Item("Checks", strings.Join(pii.DefaultChecks, ", "))
		c.Item("Credentials", "API keys, bearer tokens and NATS credentials, by shape, wherever they appear")
		c.Item("Detection", "best-effort pattern matching: it misses real values and flags text that is not personal data")
	})
}

// printTelemetrySection shows what a run would export and, for every value, where
// that value came from. The origin is the point of the section: telemetry is
// configured across a config file and half a dozen environment variables, so "the
// endpoint is X" is far less useful to someone debugging than "the endpoint is X,
// because OTEL_EXPORTER_OTLP_ENDPOINT says so". Like the Memory and Sessions sections
// it resolves everything locally and contacts nothing.
//
// It appears only when something mentions telemetry, so an operator who does not use
// it sees no new output, and it deliberately still appears when telemetry is off but
// configured, which is the confusing case worth surfacing.
func printTelemetrySection(c *columns.Document, cfg *config.Config) {
	resolved, resolveErr := telemetry.Resolve(bootstrap.SettingsFrom(cfg, ""), os.Getenv)
	if !telemetryMentioned(cfg, resolved) {
		return
	}

	c.Section("Telemetry", func(c *columns.Document) {
		if resolved.Enabled {
			c.Item("Enabled", "yes (telemetry.enabled)")
		} else {
			c.Item("Enabled", fmt.Sprintf("no (%s)", resolved.DisabledBy))
		}

		// A rejected configuration is shown alongside the values rather than instead of
		// them, since seeing what was resolved is most of what makes the message
		// actionable. info never fails on it: this command inspects a configuration
		// without running it.
		if resolveErr != nil {
			c.Item("Invalid", resolveErr.Error())
		}

		c.Item("Endpoint", withOrigin(resolved.Endpoint.Value, resolved.Endpoint.Origin))
		c.Item("Service name", withOrigin(resolved.ServiceName.Value, resolved.ServiceName.Origin))
		c.Item("Sample ratio", withOrigin(fmt.Sprintf("%g", resolved.SampleRatio.Value), resolved.SampleRatio.Origin))

		metrics := "enabled"
		if !resolved.Metrics.Value {
			metrics = "disabled"
		}
		c.Item("Metrics", withOrigin(metrics, resolved.Metrics.Origin))

		printTelemetryCaptureItems(c, resolved)

		c.Item("Credential scrub", telemetryScrubStatus(cfg))
	})
}

// printTelemetryCaptureItems shows what content capture would export.
//
// Off is one line rather than four, because the settings underneath it mean nothing
// then and four lines of inert configuration is how an operator comes to believe a
// feature is on. On is four, and the fourth is the point: the export batch size is
// derived from the content cap rather than configured, so it is invisible everywhere
// else and it moves underneath an operator the moment they change the cap. This command
// exists to show exactly that class of value.
func printTelemetryCaptureItems(c *columns.Document, resolved telemetry.Resolved) {
	if !resolved.Capture.Value {
		c.Item("Content capture", "off (default): spans carry structure and timing only")
		return
	}

	c.Item("Content capture", withOrigin("on", resolved.Capture.Origin)+
		": prompts, model output, tool arguments and tool results are exported to this endpoint")
	c.Item("Content messages", withOrigin(resolved.Messages.Value.String(), resolved.Messages.Origin))
	c.Item("Content limit", withOrigin(fmt.Sprintf("%d bytes per attribute", resolved.MaxBytes.Value), resolved.MaxBytes.Origin))
	c.Item("Export batch", withOrigin(fmt.Sprintf("%d spans", resolved.ExportBatch.Value), resolved.ExportBatch.Origin))
}

// addMCPToolRows lists every imported MCP tool in the tool table, so one table answers
// what the model can call whether a tool came from the application, a built-in, an a2a
// peer or an MCP server. The source is the server's alias, the way a remote tool's is
// its host's alias, and it is the prefix the tool's own name carries.
//
// An MCP tool carries no tags and is never confirm gated locally, so both of those
// cells stay blank. Its description is the server's own text, which is not split the
// way a remote agent's is: no tag block is appended to it.
func addMCPToolRows(tbl *table.Table, imports []mcpclient.ServerImport) {
	for _, imp := range imports {
		alias := imp.Server.EffectiveAlias()
		for _, mt := range imp.Tools {
			desc := strings.ReplaceAll(mt.Description(), "\n", " ")
			tbl.AddRow(mt.Name(), alias, "", util.TruncateString(desc, maxInfoDescriptionLen), "")
		}
	}
}

// printMCPServerStatus prints a per-server block after the tool table, the parallel of
// the remote host block, so an operator can tell why an MCP tool is or is not present:
// where the server is reached and over which transport, whether it answered and how
// long it took, how many tools it advertised, how many its filters kept, what each
// survivor is named, and any tool that was left out with the reason.
//
// A server that could not be started, reached or listed is printed with its error.
// Discovery is best-effort here, and this is where an operator inspecting a
// configuration finds out that one of its servers is down.
func printMCPServerStatus(c *columns.Document, imports []mcpclient.ServerImport) {
	if len(imports) == 0 {
		return
	}

	c.Blank()
	c.Heading("MCP servers")

	for _, imp := range imports {
		if imp.Err != nil {
			c.Printf("  %s (%s): UNAVAILABLE: %v\n", imp.Server.Name, mcpServerTarget(imp.Server), imp.Err)
			continue
		}

		c.Printf("  %s (%s): reachable in %s, advertised %d tool(s), kept %d after filtering, imported %d as %q\n",
			imp.Server.Name, mcpServerTarget(imp.Server), imp.RTT.Round(time.Millisecond),
			imp.Discovered, len(imp.Kept), len(imp.Tools), imp.Server.EffectiveAlias())

		if len(imp.Tools) > 0 {
			names := make([]string, 0, len(imp.Tools))
			for _, mt := range imp.Tools {
				names = append(names, mt.Name())
			}
			c.Printf("    tools: %s\n", strings.Join(names, ", "))
		}

		for _, skipped := range imp.Skipped {
			c.Printf("warning: mcp server %q: tool %q was not imported: %s\n", imp.Server.Name, skipped.Name, skipped.Reason)
		}
	}
}

// mcpServerTarget names the transport a server is reached over and what it is reached
// at: the command line for a stdio server, and for an HTTP one the endpoint as the
// configuration wrote it, through config.MCPServer.SafeURL. The expanded url is never
// printed. It holds whatever each "${VAR}" resolved to, and a service that
// authenticates by query parameter keeps its credential right there.
//
// SafeURL replaces the userinfo, the value of every query parameter and the fragment,
// and leaves the path, so a credential an operator wrote into a path segment as a
// literal is printed as they wrote it: nothing can tell that segment from one naming a
// route. A reference is left as written, so it names the variable.
//
// The command and each argument of a stdio entry go through config.RedactURL as well.
// The bridge pattern an operator reaches for, "npx -y mcp-remote
// https://host/sse?key=...", carries the endpoint in an argument, and without this the
// key is printed in full while the same url written under url: is redacted. An argument
// carrying no url comes back unchanged: RedactURL rewrites only the userinfo before a
// host, the value of every query parameter and the fragment, so "-y", "--port=8080" and
// "/usr/local/bin/server" are printed as they were configured.
func mcpServerTarget(server config.MCPServer) string {
	if server.URL != "" {
		return "http " + server.SafeURL()
	}

	command := make([]string, 0, len(server.Args)+1)
	command = append(command, config.RedactURL(server.Command))
	for _, arg := range server.Args {
		command = append(command, config.RedactURL(arg))
	}

	return strings.TrimSpace("stdio " + strings.Join(command, " "))
}

// withOrigin renders a resolved value with the config key or environment variable
// that decided it.
func withOrigin(value string, origin string) string {
	return fmt.Sprintf("%s (%s)", value, origin)
}

// telemetryMentioned reports whether the config or the environment says anything
// about telemetry, which is the gate on showing the section at all.
func telemetryMentioned(cfg *config.Config, resolved telemetry.Resolved) bool {
	// Capture counts even with export off, and that pairing is the reason it is named
	// here rather than folded into the first condition: a file that turns content
	// capture on and telemetry off is precisely the configuration an operator is
	// staring at when they ask whether this thing is running.
	if cfg.TelemetryEnabled() || cfg.TelemetryCaptureEnabled() || len(resolved.TransportEnvSet) > 0 {
		return true
	}

	for _, name := range []string{telemetry.EnvServiceName, telemetry.EnvSDKDisabled, telemetry.EnvNoTelemetry} {
		if os.Getenv(name) != "" {
			return true
		}
	}

	return false
}

// telemetryScrubStatus names the OpenTelemetry credential variables that are set in
// this environment and will therefore be stripped from tool subprocesses.
//
// It lists what is actually set rather than the whole known list, which would be
// sixteen names of noise: what an operator wants to confirm is that the token they
// exported is the one being hidden from the model's tools. The scrub is unconditional,
// so this reads the same whether or not telemetry is enabled.
func telemetryScrubStatus(cfg *config.Config) string {
	var set []string
	for _, name := range cfg.CredentialEnvNames() {
		if os.Getenv(name) != "" {
			set = append(set, name)
		}
	}

	if len(set) == 0 {
		return "no credential variables are set in this environment"
	}

	return fmt.Sprintf("%s (stripped from tool subprocesses)", strings.Join(set, ", "))
}

// toolSearchStatus describes how server-side tool search will behave for a run of
// cfg: disabled by the operator, unsupported by (or unknown to) the provider, or
// enabled and used once the tool count crosses the threshold. It resolves the
// provider only to read its capabilities, never to make a call, so it works offline
// and with no credentials.
//
// A run does not warn about no_tool_search, since the operator chose it, so the cost
// of that choice is reported here instead: with totalTools at or above the threshold
// every one of them is sent on every request. totalTools comes from the caller's
// resolved tool set, which counts the tools a2a and MCP discovery reached; an a2a
// peer that could not be discovered and an MCP server that would not answer both
// leave it short of what a run would send.
func toolSearchStatus(cfg *config.Config, totalTools int) string {
	if !cfg.ToolSearchEnabled() {
		if totalTools >= util.ToolSearchThreshold {
			return fmt.Sprintf("disabled (no_tool_search), %d tools are sent to the model directly and use more context on each request; unset it to defer them behind the search tool, supported on Anthropic models only", totalTools)
		}
		return "disabled (no_tool_search)"
	}

	provider, err := llm.NewProvider(cfg.LLMProvider(), llm.Config{})
	if err != nil {
		return fmt.Sprintf("unknown (provider %q is not available)", cfg.LLMProvider())
	}
	if !provider.Capabilities().SupportsToolSearch {
		return fmt.Sprintf("unavailable (provider %q does not support it)", cfg.LLMProvider())
	}

	return fmt.Sprintf("enabled (used when %d or more tools are available)", util.ToolSearchThreshold)
}

// splitRemoteDescription separates a remote tool's advertised description into its
// human-facing text and its tag list. A serving agent advertises the model-facing
// description, which is the command help followed by a "\n\nTags: ..." block (or,
// when the help is empty, just that block). This recovers the two parts for
// display so a remote row matches a local one: clean description, tags column. A
// description with no tag block is returned unchanged with empty tags.
func splitRemoteDescription(s string) (desc string, tags string) {
	const sep = "\n\nTags: "
	if idx := strings.LastIndex(s, sep); idx >= 0 {
		return s[:idx], s[idx+len(sep):]
	}

	const prefix = "Tags: "
	if strings.HasPrefix(s, prefix) {
		return "", strings.TrimPrefix(s, prefix)
	}

	return s, ""
}
