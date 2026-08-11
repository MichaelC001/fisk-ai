//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/choria-io/fisk"
	"github.com/goccy/go-yaml"
)

// identityPattern constrains the agent identity to characters that are also legal
// in a single NATS subject token and queue group, since the identity is used as
// the discovery queue group. It rejects whitespace, '.', '*', '>', and anything
// else that would form an invalid or wildcard-bearing subject.
var identityPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// defaultIdentity is the identity used when neither an explicit identity nor an
// application_path (whose basename would otherwise supply one) is set. It keeps
// the identity a legal NATS token and the memory/knowledge store paths stable for
// an application-less agent.
const defaultIdentity = "fisk-ai"

// Default budget values applied wherever a config leaves a value unset.
const (
	defaultLLMMaxTokens     = 200000
	defaultLLMMaxIterations = 50
	defaultLLMCallTimeout   = 120 * time.Second
)

// Config is the top-level agent configuration.
type Config struct {
	// Identity is the name used in discovery; it doubles as a queue group so
	// multiple agents sharing an identity share the work. Optional if MCP.
	Identity string `json:"identity" yaml:"identity"`
	// ApplicationPath is the app to run and introspect for tools.
	ApplicationPath string `json:"application_path" yaml:"application_path"`
	// NatsContext is the name of a NATS context (as managed by `nats context`
	// and resolved by jsm.go/natscontext) used to connect to NATS for importing
	// remote tools and for the a2a server. Required when RemoteTools is set or in
	// server mode.
	NatsContext string `json:"nats_context,omitempty" yaml:"nats_context,omitempty"`
	// SystemPrompt describes what we are doing and may be long; think of it as a
	// single-skill agent where this is the skill. Optional if MCP.
	SystemPrompt string `json:"system_prompt" yaml:"system_prompt"`

	// Exclude filters tools out. By default the entire command becomes tools
	// (regex matching); this lets you take `nats` and only expose `nats auth`.
	Exclude *ToolFilter `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	// Include restricts tools to a specific set; it can never include `ai:deny`.
	Include *ToolFilter `json:"include,omitempty" yaml:"include,omitempty"`
	// GlobalFlags is an allowlist of the wrapped application's global (application-
	// level) flag names to expose to the model as an argument on every leaf command
	// tool. It is how an operator surfaces a safe global such as nats's --context,
	// which selects a stored connection profile, while keeping sensitive globals such
	// as --user and --password hidden from the model. A name is the long flag name,
	// with or without the leading dashes; each is validated against the application's
	// real global flags when tools are loaded, and a name matching no exposable global
	// is an error. Hidden and framework flags (help, version, ...) cannot be exposed. A
	// name that collides with a command's own flag or argument is skipped for that
	// command. A global the application marks required is always exposed, listed here
	// or not, since the command cannot run without it.
	GlobalFlags []string `json:"global_flags,omitempty" yaml:"global_flags,omitempty"`
	// RemoteAgents are remote agents we can talk to using a2a-like behaviors.
	RemoteAgents []RemoteAgent `json:"remote_agents,omitempty" yaml:"remote_agents,omitempty"`
	// RemoteTools are remote agents we pull in all the tools of.
	RemoteTools []RemoteToolHost `json:"remote_tools,omitempty" yaml:"remote_tools,omitempty"`
	// Expose makes this agent discoverable to other agents and/or over MCP.
	Expose *ExposeConfig `json:"expose,omitempty" yaml:"expose,omitempty"`
	// Harness groups the settings that govern how the agent harness itself behaves
	// during a run: the human-in-the-loop tools, the confirmation gate tags, and the
	// terminal UI switches. It is optional; its zero value leaves every setting at its
	// default (human-in-the-loop off, no extra confirm tags, TUI on, bell on).
	Harness HarnessConfig `json:"harness,omitempty" yaml:"harness,omitempty"`
	// Telemetry configures OpenTelemetry export. It sits at the top level rather than
	// under harness, even though only the agent run honors it today: harness settings
	// are defined as applying to the agent loop and being ignored by mcp and a2a mode,
	// while telemetry is a process concern that is expected to grow past the run path.
	// Moving a config key after operators have written it is a breaking change, so the
	// cost of one key that only run honors today is preferred to a rename later.
	Telemetry TelemetryConfig `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`

	// LLM is the model to use and general LLM setup. Always required.
	LLM LLMConfig `json:"llm" yaml:"llm"`
}

// TelemetryConfig configures OpenTelemetry trace and metric export over OTLP/HTTP.
//
// It carries only what belongs in a file that is committed and shared. Transport
// credentials and the finer transport settings come from the standard OTEL_*
// environment variables, so a bearer token never appears in the YAML, and an operator
// who already runs OpenTelemetry configures this the way they configure everything
// else. Those variables never enable export by themselves: a host-wide collector
// endpoint must not silently turn every fisk-ai process on the box into an exporter.
//
// The block is literal, like the rest of the config. Resolution against the
// environment and every validation of these values happens in internal/telemetry, so
// nothing here holds an effective value that the file did not state.
type TelemetryConfig struct {
	// Enabled turns export on. Nothing is exported unless it is true, whatever the
	// environment says.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Endpoint is the OTLP/HTTP base URL; the /v1/traces and /v1/metrics paths are
	// appended. Left empty, the endpoint comes from the standard
	// OTEL_EXPORTER_OTLP_ENDPOINT handling, defaulting to http://localhost:4318.
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	// ServiceName names this service to the telemetry backend. Left empty it falls
	// back to OTEL_SERVICE_NAME, then to the agent identity, then to fisk-ai. An
	// operator who set OTEL_SERVICE_NAME in a systemd unit or a Kubernetes manifest
	// said something explicit, so it wins over the identity; the identity reaches the
	// backend regardless, as gen_ai.agent.name.
	ServiceName string `json:"service_name,omitempty" yaml:"service_name,omitempty"`
	// SampleRatio is the head sampling ratio from 0.0 to 1.0, defaulting to 1.0.
	//
	// It is a pointer because zero is a meaningful value here and the zero value would
	// destroy it: an explicit "sample_ratio: 0", meaning sample nothing, would arrive
	// as the Go zero value, be indistinguishable from an absent key, be defaulted back
	// to 1.0, and send every trace to a paid backend. That is the exact inverse of what
	// was asked for, and it would be silent.
	SampleRatio *float64 `json:"sample_ratio,omitempty" yaml:"sample_ratio,omitempty"`
	// NoMetrics turns the metric pipeline off, leaving traces alone. Metrics are on
	// with telemetry, so this is a negative switch like no_tui and no_bell: a positive
	// "metrics: true" would be the only default-on positive key in the file, and
	// "metrics: false" would be indistinguishable from unset.
	NoMetrics bool `json:"no_metrics,omitempty" yaml:"no_metrics,omitempty"`
	// Capture exports the conversation itself alongside the structure and timing. It
	// is off unless the block says otherwise.
	Capture *TelemetryCaptureConfig `json:"capture,omitempty" yaml:"capture,omitempty"`
}

// TelemetryCaptureConfig turns on content capture: the system prompt, the
// conversation, the model's replies, tool arguments and tool results, exported as span
// attributes.
//
// Read this before enabling it. Everything the model saw and everything the tools
// returned reaches the collector, so whoever can read the traces can read the
// conversation, and an export cannot be recalled. Tool results are the verbatim output
// of whatever command the model chose to run, the system prompt carries the agent's
// whole durable memory index, and none of it is redacted or filtered: content capture
// bypasses every other protection in this area by construction, including the closed
// error vocabulary that keeps filesystem paths off spans. It is meant for a bounded
// investigation against a collector you control, not as a fleet default.
//
// A run with it on says so at startup and marks its summary line.
type TelemetryCaptureConfig struct {
	// Enabled turns content capture on. Nothing below it does anything while this is
	// false, and none of it is validated then either, so turning capture off never
	// fails a run over a setting that has stopped mattering.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Messages selects how much of the conversation each model call carries: "delta",
	// the default, exports only what that call added, and "full" exports the whole
	// conversation every time.
	//
	// The default is the delta because the alternative is quadratic: the conversation
	// only grows, so a thirty-iteration run would export thirty copies of a growing
	// transcript, and the batch processor drops spans silently once its queue fills.
	// The cost is that no single span holds a whole conversation; the span attribute
	// fisk.content.from_index says where each one starts, and uses these same two
	// words, so what a backend shows and what this file says are searchable for each
	// other.
	Messages string `json:"messages,omitempty" yaml:"messages,omitempty"`
	// MaxBytes caps each content attribute, measured on the encoded JSON. It defaults
	// to 8192 and must be between 256 and 65536.
	//
	// It bounds what one span can carry, which is what keeps a batch under a
	// collector's request limit: an over-large batch is refused whole, and OTLP being
	// fire and forget, that is close to invisible. Raising it raises the memory a run
	// holds and lowers how many spans fit in one export.
	MaxBytes int `json:"max_bytes,omitempty" yaml:"max_bytes,omitempty"`
}

// HarnessConfig groups the settings that govern how the agent harness behaves
// during a run, as distinct from the model (llm) or the tool selection. Every
// field is optional and its zero value is the default behavior.
type HarnessConfig struct {
	// HumanInTheLoop, when enabled, gives the model a built-in in-process tool to
	// put a question to the operator at the terminal. Agent mode only; it is never
	// exposed over MCP and needs an interactive terminal.
	HumanInTheLoop *HumanInTheLoopConfig `json:"human_in_the_loop,omitempty" yaml:"human_in_the_loop,omitempty"`
	// ConfirmTags lists command tags that, in addition to the always-on ai:confirm
	// tag, require the operator's explicit approval before a tagged command runs.
	// Matching is exact (not a regex) and additive to ai:confirm. In the agent loop
	// it gates against the operator; over MCP it gates through client elicitation as
	// governed by expose.agent.mcp.confirm_over_mcp.
	ConfirmTags []string `json:"confirm_tags,omitempty" yaml:"confirm_tags,omitempty"`
	// ToolTimeoutString bounds a single tool call in the agent loop as a duration
	// string (e.g. 5m, or 1d for the day, week, month and year units fisk parses on
	// top of Go's). Unset, or 0s, leaves tool execution unbounded, which is the
	// terminal's behavior: an operator watching a run can interrupt a command that will
	// never answer. A hosted worker has nobody to interrupt it, so internal/serve fills
	// in a default of its own when this is unset.
	//
	// It bounds the agent loop only, as every harness setting does.
	// expose.agent.mcp.tool_timeout and expose.agent.a2a.tool_timeout are separate
	// values bounding the same unit of work on the two serving paths.
	//
	// The bound is cooperative: it cancels the call's context. An application command
	// is killed along with its process group, and an in-process tool stops only if its
	// handler observes the context. A call whose duration is set by a person answering
	// is never bounded, since the bound would cancel the question rather than a
	// runaway.
	ToolTimeoutString string `json:"tool_timeout,omitempty" yaml:"tool_timeout,omitempty"`
	// ToolTimeoutParsed is the parsed form of ToolTimeoutString, filled by prepare().
	ToolTimeoutParsed time.Duration `json:"-" yaml:"-"`
	// NoTUI disables the full-screen terminal UI for this agent, always using the
	// line-by-line output even on an interactive terminal. It is a hard off switch
	// that the command line cannot re-enable, for agents whose operators rely on the
	// line UI (piping, screen readers). The TUI is otherwise the default on a terminal.
	NoTUI bool `json:"no_tui,omitempty" yaml:"no_tui,omitempty"`
	// NoBell silences the terminal bell the full-screen UI rings when a run blocks
	// waiting on an operator decision (an approval gate or an ask_human_* prompt). The
	// bell is on by default so an operator who looked away is alerted; set this for an
	// agent that prompts often, or where an audible bell is unwelcome. Like no_tui it
	// is a negative switch, and it has no effect in the line UI.
	NoBell bool `json:"no_bell,omitempty" yaml:"no_bell,omitempty"`
	// Memory, when enabled, gives the model built-in tools to keep durable notes in
	// a key/value store that survives across runs. Agent mode only; like the
	// human-in-the-loop tools the memory tools declare no serving exposure, so
	// neither MCP nor a2a carries them.
	Memory *MemoryConfig `json:"memory,omitempty" yaml:"memory,omitempty"`
	// RAG, when enabled, gives the model a built-in knowledge_search tool over a
	// locally-built index of the operator's markdown/text documents. The user-facing
	// name is "knowledge" (the config block, the CLI command, and the tool); the Go
	// identifiers keep the rag prefix since RAG is the technique. It has two tiers: a
	// lexical FTS5 baseline that is always on when enabled, and an opt-in vector tier
	// active only when the embeddings sub-block is present.
	RAG *RAGConfig `json:"knowledge,omitempty" yaml:"knowledge,omitempty"`
	// Sessions selects and configures the store that holds checkpointed run
	// journals. Like the memory and knowledge blocks its options are captured as a
	// raw block (through the same canonical JSON path, so an unknown option key fails
	// as loudly as an unknown top-level key) and decoded per backend at store
	// construction. It is optional: an absent block resolves to the file backend
	// under the XDG default (see SessionBackend). The --state-dir flag is folded in
	// last, after parsing, by ApplyStateDir, so an explicit flag still wins over a
	// configured file directory and is rejected against a non-file backend.
	Sessions *SessionConfig `json:"sessions,omitempty" yaml:"sessions,omitempty"`
}

// SessionConfig selects and configures the session store backend. Its shape
// mirrors MemoryConfig: a backend name and a raw per-backend options block decoded
// against a typed schema at store construction, so an unknown option key fails as
// loudly as an unknown top-level key. For the file backend the options accept
// {directory: <path>}, defaulting to the absolute XDG state directory.
type SessionConfig struct {
	// Backend selects the store implementation. It defaults to "file", which keeps
	// each run in a JSON-lines journal under a directory; "jetstream" keeps each
	// record on its own subject in a pre-existing NATS JetStream stream.
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`
	// Options carries backend-specific settings as a raw block, decoded against a
	// typed per-backend schema at store construction. For the file backend it
	// accepts {directory: <path>}.
	Options json.RawMessage `json:"options,omitempty" yaml:"options,omitempty"`
}

// BackendName returns the configured backend, defaulting to "file". It is
// nil-safe: sessions are always available (checkpointing is not a feature that can
// be disabled), so a nil config resolves to the file backend rather than to an
// empty name that would fail lookup.
func (s *SessionConfig) BackendName() string {
	if s == nil || s.Backend == "" {
		return "file"
	}

	return s.Backend
}

// DeclaredBackend returns the backend the operator named, or "" when the block is
// absent or leaves the backend unset. It is nil-safe.
func (s *SessionConfig) DeclaredBackend() string {
	if s == nil {
		return ""
	}

	return s.Backend
}

// RawOptions returns the raw backend options block, decoded per backend at store
// construction. It is nil-safe and nil when no options are set.
func (s *SessionConfig) RawOptions() json.RawMessage {
	if s == nil {
		return nil
	}

	return s.Options
}

// SessionConfigFromStateDir synthesizes the session config from the --state-dir
// flag. An empty dir yields the file backend with no options, so the file backend
// applies its default (the absolute XDG state directory); a set dir populates the
// file backend's directory option. The flag always wins over the harness.sessions
// block: this override is applied last, so an explicit --state-dir takes precedence
// over a configured directory.
func SessionConfigFromStateDir(dir string) *SessionConfig {
	if dir == "" {
		return &SessionConfig{Backend: "file"}
	}

	return &SessionConfig{
		Backend: "file",
		Options: json.RawMessage(fmt.Sprintf(`{"directory":%q}`, dir)),
	}
}

// ApplyStateDir folds the --state-dir flag into the session config after the file
// has been parsed, so an explicit flag wins over a configured directory. It applies
// only to the file backend: --state-dir names a filesystem directory, which a
// non-file backend (jetstream) has no place for, so combining the two is a hard
// error rather than a silently ignored flag. An empty dir is a no-op, leaving
// whatever the config set (or the nil default that resolves to the file backend).
func (c *Config) ApplyStateDir(dir string) error {
	if dir == "" {
		return nil
	}

	if c.SessionBackend() != "file" {
		return fmt.Errorf("--state-dir applies only to the file session backend, but harness.sessions.backend is %q: remove --state-dir, or set harness.sessions.backend to \"file\"", c.SessionBackend())
	}

	c.Harness.Sessions = SessionConfigFromStateDir(dir)

	return nil
}

// RAGConfig configures the built-in knowledge_search tool and the backing SQLite
// index. It is written by the operator as harness.knowledge. The lexical tier is
// always available when enabled; the vector tier turns on only when Embeddings is
// present. The index stores verbatim document text UNENCRYPTED on disk (file mode
// 0600), the same posture as the memory feature, so do not index secrets.
type RAGConfig struct {
	// Enabled turns the knowledge_search tool on. The block being absent, or present
	// with enabled false, leaves it off.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Paths are the default index roots used by knowledge index when no explicit path
	// is given. It is not an error for this to be empty, but then knowledge index
	// requires an explicit path argument.
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	// Directory is where the SQLite index lives. It is resolved relative to the
	// working directory when not absolute, and defaults to knowledge/<identity>,
	// mirroring harness.memory's directory. It is project-local and excluded from its
	// own index walk.
	Directory string `json:"directory,omitempty" yaml:"directory,omitempty"`
	// TopK is the default number of chunks knowledge_search returns when the model
	// does not request a specific count. It defaults to 5 and is clamped to a hard
	// ceiling of 20.
	TopK int `json:"top_k,omitempty" yaml:"top_k,omitempty"`
	// MaxInjectedTokens caps the total retrieved text fed to the model in one search
	// result. It defaults to 6000.
	MaxInjectedTokens int `json:"max_injected_tokens,omitempty" yaml:"max_injected_tokens,omitempty"`
	// Embeddings, when present, turns on the hybrid vector tier. Its absence leaves
	// the feature lexical-only, needing no model and no external service.
	Embeddings *RAGEmbeddingsConfig `json:"embeddings,omitempty" yaml:"embeddings,omitempty"`
}

// RAGEmbeddingsConfig configures the optional vector tier: a local
// OpenAI-compatible embeddings server contacted only when this block is present.
// The model is user-chosen; we make no assumptions about its dimension or prefix
// needs and pin whatever it emits in the index manifest.
type RAGEmbeddingsConfig struct {
	// BaseURL is the OpenAI-compatible base; embeddings are POSTed to
	// <base_url>/embeddings. A non-loopback base_url must use https.
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Model is the embedding model name passed to the server.
	Model string `json:"model" yaml:"model"`
	// APIKeyEnv is the NAME of an environment variable holding a bearer token, never
	// the secret itself. When set the token is sent as Authorization: Bearer.
	APIKeyEnv string `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	// TimeoutString is the per-request timeout as a duration string, e.g. 30s. It
	// defaults to 30s.
	TimeoutString string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// TimeoutParsed is the parsed form of TimeoutString.
	TimeoutParsed time.Duration `json:"-" yaml:"-"`
	// QueryPrefix is prepended to a query before embedding. Default empty, since the
	// model is user-chosen and a wrong prefix is worse than none.
	QueryPrefix string `json:"query_prefix,omitempty" yaml:"query_prefix,omitempty"`
	// DocumentPrefix is prepended to each chunk before embedding; it supports a
	// {title} placeholder filled from the chunk's heading. Default empty.
	DocumentPrefix string `json:"document_prefix,omitempty" yaml:"document_prefix,omitempty"`
}

// MemoryConfig configures the built-in memory tools and the backing store. The
// options block is decoded per backend, so its legal keys depend on backend; for
// the file backend it accepts a single directory.
type MemoryConfig struct {
	// Enabled turns the memory tools on. The block being absent, or present with
	// enabled false, leaves them off.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Backend selects the store implementation. It defaults to "file", which keeps
	// each memory in a markdown file under a directory; "jetstream" keeps each in a
	// pre-existing NATS KV bucket.
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`
	// NoIndex opts out of injecting the list of stored memories (key and
	// description) into the system prompt at run start. The index is on by default
	// so the model knows what it has saved without having to call memory_list; set
	// this to keep the store's contents out of the prompt. Like no_tui it is a
	// negative switch.
	NoIndex bool `json:"no_index,omitempty" yaml:"no_index,omitempty"`
	// Options carries backend-specific settings as a raw block, decoded against a
	// typed per-backend schema at store construction so an unknown option key fails
	// as loudly as an unknown top-level key. For the file backend it accepts
	// {directory: <path>}, defaulting to memory/<identity>.
	Options json.RawMessage `json:"options,omitempty" yaml:"options,omitempty"`
}

// Confirm-over-MCP policies for ExposedMCPConfig.ConfirmOverMCP.
const (
	// ConfirmOverMCPAuto asks a client that supports elicitation to approve each
	// confirm-tagged command and runs it ungated for a client that does not. It is
	// the default when confirm_over_mcp is unset.
	ConfirmOverMCPAuto = "auto"
	// ConfirmOverMCPAlways requires approval for every confirm-tagged command: a
	// client that cannot elicit is refused rather than allowed to run it ungated.
	ConfirmOverMCPAlways = "always"
	// ConfirmOverMCPNever never asks over MCP; confirm-tagged commands run ungated
	// regardless of client support, for operators who rely on the client's own
	// approval UI and want to avoid a second prompt.
	ConfirmOverMCPNever = "never"
)

// KnowledgeSearchToolName is the name of the read-only knowledge_search built-in
// tool. It is defined here, the lowest layer, so config can validate the
// expose.agent.mcp.builtins allowlist without importing the util package that
// implements the tool; util references this same constant so the two never drift.
const KnowledgeSearchToolName = "knowledge_search"

// KnowledgeEnumerateToolName is the name of the read-only knowledge_enumerate
// built-in tool, on the same terms as KnowledgeSearchToolName.
const KnowledgeEnumerateToolName = "knowledge_enumerate"

// mcpExposableBuiltins are the built-in tools an operator may name in
// expose.agent.mcp.builtins. Both are read-only knowledge tools that need no
// operator at a terminal, which is what the memory and ask_human_* built-ins
// cannot say. Membership here is only the selection half: a tool must also declare
// MCP exposure on its own spec, so this list can never widen what is servable.
//
// They are two halves of one capability and are meant to be served together.
// knowledge_search ranks and so cannot tell absence from a low score, which is the
// question knowledge_enumerate answers; a client given only the first has the
// defect the second exists to fix. Each is nonetheless selectable on its own,
// because selection stays per tool and an operator who wants one gets one.
var mcpExposableBuiltins = []string{KnowledgeSearchToolName, KnowledgeEnumerateToolName}

// HumanInTheLoopConfig configures the built-in human-in-the-loop tools, which let
// the model ask the operator a question at the terminal during an agent run.
type HumanInTheLoopConfig struct {
	// Enabled turns the human-in-the-loop tools on. The block being absent, or
	// present with enabled false, leaves them off.
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// Anthropic model identifiers usable as an LLMConfig.Model value.
const (
	// ModelClaudeFable5 is the Claude Fable 5 model, the most capable widely
	// released model for the most demanding reasoning and long-horizon agentic
	// work; the slowest and most expensive tier.
	ModelClaudeFable5 = "claude-fable-5"
	// ModelClaudeOpus48 is the Claude Opus 4.8 model, the most capable Opus tier;
	// slowest and most expensive Opus, best for hard reasoning and agentic work.
	ModelClaudeOpus48 = "claude-opus-4-8"
	// ModelClaudeOpus47 is the Claude Opus 4.7 model, the prior Opus release.
	ModelClaudeOpus47 = "claude-opus-4-7"
	// ModelClaudeOpus46 is the Claude Opus 4.6 model, an earlier Opus release.
	ModelClaudeOpus46 = "claude-opus-4-6"
	// ModelClaudeOpus45 is the Claude Opus 4.5 model, an earlier Opus release.
	ModelClaudeOpus45 = "claude-opus-4-5-20251101"
	// ModelClaudeSonnet5 is the Claude Sonnet 5 model, the mid tier balancing
	// capability, speed, and cost; a good general-purpose default.
	ModelClaudeSonnet5 = "claude-sonnet-5"
	// ModelClaudeSonnet46 is the Claude Sonnet 4.6 model, the prior Sonnet release.
	ModelClaudeSonnet46 = "claude-sonnet-4-6"
	// ModelClaudeSonnet45 is the Claude Sonnet 4.5 model, an earlier Sonnet release.
	ModelClaudeSonnet45 = "claude-sonnet-4-5-20250929"
	// ModelClaudeHaiku45 is the Claude Haiku 4.5 model, the fastest and cheapest
	// tier; best for high-throughput, latency-sensitive, or simpler tasks.
	ModelClaudeHaiku45 = "claude-haiku-4-5-20251001"

	// defaultLLMProvider is the model backend used when llm.provider is unset, so a
	// zero-config agent keeps working. It must match the name the provider registers
	// itself under (internal/llm/anthropic); a mismatch surfaces at run start as an
	// unknown-provider error rather than silently.
	defaultLLMProvider = "anthropic"
)

// LLMConfig holds the model to use and general LLM setup.
type LLMConfig struct {
	// Model is the LLM model to use, e.g. ModelClaudeSonnet5 ("claude-sonnet-5").
	Model string `json:"model" yaml:"model"`
	// Provider selects the model backend, for example "anthropic". It defaults to
	// "anthropic" when unset, so a zero-config agent keeps working and most operators
	// never set it. Set it only to target a different backend that has been linked
	// into this build; naming a provider that is not linked in fails at run start with
	// the list of providers that are available. The value is neutral and never names
	// an SDK, and it is stamped into the run fingerprint so a resume against a
	// different provider is refused.
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	// Budget bounds LLM usage; optional but recommended for long running agents.
	Budget LLMBudget `json:"budget" yaml:"budget"`
	// Thinking configures whether the model exposes its reasoning. Off by default:
	// when off, no thinking is requested and the model uses its default behavior.
	Thinking ThinkingConfig `json:"thinking" yaml:"thinking"`
	// NoPromptCache disables Anthropic prompt caching for this agent. Caching is on by
	// default (the zero value), mirroring no_tui / no_bell; set it only for a non-Anthropic
	// endpoint (ANTHROPIC_BASE_URL) whose proxy rejects or ignores cache_control. Disabling
	// only raises cost, it never changes output.
	NoPromptCache bool `json:"no_prompt_cache,omitempty" yaml:"no_prompt_cache,omitempty"`
	// NoToolSearch disables server-side tool search for this agent, so every tool is
	// sent to the model directly rather than deferred behind a search tool once ten or
	// more are available. It is the manual complement to a provider's own capability:
	// the provider reports whether tool search is possible at all, and this switch turns
	// it off for an endpoint where it is possible but unwanted, such as an
	// ANTHROPIC_BASE_URL proxy that does not implement the tool search tool. Left off
	// (the zero value), tool search is used whenever the active provider supports it and
	// the tool count crosses the threshold, mirroring no_prompt_cache. Disabling it only
	// sends more tools up front; it never removes a tool or changes what the model can call.
	NoToolSearch bool `json:"no_tool_search,omitempty" yaml:"no_tool_search,omitempty"`
}

// ThinkingConfig configures whether the model exposes its reasoning. It is a
// struct rather than a bare bool so further controls (e.g. effort) can be added
// later without changing the configuration shape. The setting is provider
// neutral: the active backend maps it to its own mechanism. Older Anthropic
// models that predate adaptive thinking (e.g. Sonnet 4.5, Haiku 4.5) reject it,
// so it is left off by default and opted into per agent.
type ThinkingConfig struct {
	// Enabled turns model thinking on. Off by default.
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// LLMBudget bounds how much an agent may spend on the LLM.
//
// MaxTokens is a tokens-processed cap, not a dollar cap: it sums the full input
// throughput (uncached input plus cache reads and cache writes) and the output,
// so its magnitude matches the pre-cache world and a resume stays consistent.
// Prompt caching makes a run far cheaper in dollars than the token count implies
// (a cache read costs roughly a tenth of an uncached input token), so MaxTokens
// intentionally over-counts real spend; a cost-weighted budget is a separate
// future feature.
type LLMBudget struct {
	// MaxTokens is the maximum number of tokens to spend. It is a soft cap: the
	// running total is checked after each call, so a single call can overshoot it
	// by up to that call's input plus its max output tokens before the run stops.
	MaxTokens int64 `json:"max_tokens" yaml:"max_tokens"`
	// MaxOutputTokens caps the tokens a single response may generate, distinct from
	// MaxTokens which bounds the whole run. Left 0 it uses a built-in default that is
	// raised when thinking is on so the reasoning and the answer both fit. Set it only
	// to fit an endpoint whose per-response limit is lower than that default, where an
	// oversized request would otherwise be rejected; an explicit value wins over the
	// default, including the thinking increase.
	MaxOutputTokens int64 `json:"max_output_tokens" yaml:"max_output_tokens"`
	// MaxIterations is the maximum number of LLM iterations to perform.
	MaxIterations int64 `json:"max_iterations" yaml:"max_iterations"`
	// CallTimeoutString is the per-call timeout as a duration string, e.g. 60s.
	CallTimeoutString string `json:"call_timeout" yaml:"call_timeout"`
	// CallTimeoutParsed is the parsed form of CallTimeoutString.
	CallTimeoutParsed time.Duration `json:"-" yaml:"-"`
}

// ExposeConfig controls how this agent is exposed to others.
type ExposeConfig struct {
	// Agent listens on a subject for a prompt, tools etc, making it discoverable.
	Agent *AgentExpose `json:"agent,omitempty" yaml:"agent,omitempty"`
}

// AgentExpose configures the agent-facing exposure of this agent.
type AgentExpose struct {
	// AgentToAgent opts this agent in to serving its tools to other agents over
	// a2a. It is the switch for the `fisk-ai a2a` command, which refuses to start
	// unless it is true; an agent that says nothing serves nothing.
	AgentToAgent bool `json:"agent_to_agent" yaml:"agent_to_agent"`
	// MCP opts this agent in to serving its tools over MCP and carries the listen
	// port. Its presence is the switch for the `fisk-ai mcp` command, which refuses
	// to start unless it is set.
	MCP *ExposedMCPConfig `json:"mcp,omitempty" yaml:"mcp,omitempty"`
	// A2A carries the a2a server's per-server tuning (concurrency, tool timeout). It is
	// optional and independent of the agent_to_agent switch above; agent_to_agent alone
	// still serves with defaults.
	A2A *ExposedA2AConfig `json:"a2a,omitempty" yaml:"a2a,omitempty"`
	// Jobs opts this agent in to taking whole units of work off a Choria asyncjobs work
	// queue. Its presence is the switch for the queued-jobs intake of `fisk-ai serve`,
	// which refuses to start without it, and every field under it has a default, so an
	// empty block is a working configuration.
	//
	// It differs in kind from the blocks above it, and the difference matters. Those
	// serve this agent's tools to a caller that drives them; this hands the agent a
	// whole task and runs the agent loop over it. So Tools below does NOT narrow it: a
	// job reaches every tool the top-level include/exclude selected, and an operator
	// who narrows the served set is narrowing MCP and a2a only.
	Jobs *ExposedJobsConfig `json:"jobs,omitempty" yaml:"jobs,omitempty"`
	// Tools optionally narrows the served set, applied on top of the top-level
	// include/exclude; it can only remove tools, never add them. When absent the
	// whole top-level-selected set is served.
	Tools *ExposedToolSelection `json:"tools,omitempty" yaml:"tools,omitempty"`
}

// ExposedToolSelection narrows the served tool set, on top of the top-level
// include/exclude. Each filter is honored only when it carries patterns or tags.
type ExposedToolSelection struct {
	// Exclude drops tools from the served set. By default the entire command
	// becomes tools (regex matching); this lets you take `nats` and only serve
	// `nats auth`.
	Exclude *ToolFilter `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	// Include restricts the served set to a specific subset; it can never re-add an
	// `ai:deny` tool or one the top-level filters already removed.
	Include *ToolFilter `json:"include,omitempty" yaml:"include,omitempty"`
}

// ExposedMCPConfig configures the MCP server.
type ExposedMCPConfig struct {
	// Port is the TCP port the MCP server listens on.
	Port int `json:"port" yaml:"port"`
	// Address is the host or IP the MCP server binds to. It defaults to loopback
	// (127.0.0.1) so the server is not reachable off the host unless an address is
	// set explicitly; use "0.0.0.0" to listen on all interfaces. It combines with
	// Port to form the listen address.
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	// Instructions is optional free text sent to clients at connection time to
	// describe how to use the server and its tools. Clients may pass it to the
	// LLM as a hint, so it is a place to add orientation that the individual tool
	// descriptions are too terse to carry. When empty nothing is sent.
	Instructions string `json:"instructions,omitempty" yaml:"instructions,omitempty"`
	// ConfirmOverMCP selects how confirm-tagged commands (ai:confirm and the
	// harness confirm_tags) are gated when served over MCP: "auto" (the default)
	// asks clients that support elicitation and runs the command ungated for clients
	// that do not, "always" refuses a confirm-tagged command when the client cannot
	// be asked, and "never" never asks and runs it ungated, delegating approval to
	// the client's own UI.
	ConfirmOverMCP string `json:"confirm_over_mcp,omitempty" yaml:"confirm_over_mcp,omitempty"`
	// Builtins additionally exposes the agent's built-in tools (currently only
	// knowledge_search) over MCP. The agent's wrapped CLI tools are always exposed;
	// the built-ins are off by default and must be listed here because they are
	// otherwise agent-run-only. Only the read-only knowledge_search is exposable;
	// the memory and human_in_the_loop built-ins are never exposable and listing one
	// is a config error. Any client that can reach the port can then query the
	// knowledge base, so this is an explicit, security-relevant opt-in.
	Builtins []string `json:"builtins,omitempty" yaml:"builtins,omitempty"`
	// MaxConcurrentTools bounds how many tool calls the MCP server runs at once, since
	// an MCP client has no iteration budget. It is per-server on purpose: MCP bounds
	// calls from anything that reaches the TCP port (address may be 0.0.0.0), a wider
	// trust boundary than a2a's NATS peers, so it must not share a2a's knob. Unset (or
	// 0) uses the server default; it is config-only, with no flag or envar override.
	MaxConcurrentTools int `json:"max_concurrent_tools,omitempty" yaml:"max_concurrent_tools,omitempty"`
	// ToolTimeoutString bounds a single served tool call as a duration string (e.g.
	// 60s). It is named tool_timeout, not call_timeout, to avoid colliding with
	// llm.budget.call_timeout, which bounds a different unit of work. Unset uses the
	// server default.
	ToolTimeoutString string `json:"tool_timeout,omitempty" yaml:"tool_timeout,omitempty"`
	// ToolTimeoutParsed is the parsed form of ToolTimeoutString, filled by prepare().
	ToolTimeoutParsed time.Duration `json:"-" yaml:"-"`
}

// ExposedA2AConfig carries the a2a server's per-server tuning. It is a sibling of the
// agent_to_agent switch (which stays a bare bool) rather than folded into it, so an
// agent opts in with agent_to_agent: true and tunes with an a2a: block. Its knobs are
// separate from the MCP block's because the two servers bound different trust
// boundaries (NATS peers vs anything reaching a TCP port).
type ExposedA2AConfig struct {
	// MaxConcurrentTools bounds how many tool calls the a2a server runs at once, since
	// an a2a caller has no iteration budget. Unset (or 0) uses the server default;
	// config-only, no flag or envar override.
	MaxConcurrentTools int `json:"max_concurrent_tools,omitempty" yaml:"max_concurrent_tools,omitempty"`
	// ToolTimeoutString bounds a single served tool call as a duration string (e.g.
	// 60s). Named tool_timeout to avoid colliding with llm.budget.call_timeout. Unset
	// uses the server default.
	ToolTimeoutString string `json:"tool_timeout,omitempty" yaml:"tool_timeout,omitempty"`
	// ToolTimeoutParsed is the parsed form of ToolTimeoutString, filled by prepare().
	ToolTimeoutParsed time.Duration `json:"-" yaml:"-"`
}

// Defaults for the queued-jobs intake. The queue and task type default to the values
// the documentation submits with, because both ends of a task type have to agree and
// nothing can validate the pairing: a default nobody has to think about is worth more
// than one that is merely tidy.
const (
	// DefaultJobsQueue is the work queue a jobs worker consumes when none is named.
	DefaultJobsQueue = "FISK_AI"
	// DefaultJobsTaskType is the asyncjobs task type a jobs worker handles when none
	// is named.
	DefaultJobsTaskType = "fisk-ai:run"
	// DefaultJobsWorkers is how many jobs one worker runs at once when none is set. It
	// is one because a run is expensive and a worker quietly running several multiplies
	// spend without anyone having asked for it.
	DefaultJobsWorkers = 1
)

// ExposedJobsConfig configures the queued-jobs intake: which work queue this agent
// takes whole units of work from, and how many it runs at once.
//
// What is deliberately not here is as much of the shape as what is. The queue's run
// time, retry cap and concurrency belong to the queue and are set with `ajc`, then read
// from the bound consumer at startup, because stating them in two places is one place
// to get them out of step. The session store is harness.sessions and the tool bound is
// harness.tool_timeout, both shared with every other surface.
type ExposedJobsConfig struct {
	// Queue is the work queue to consume. It must already exist: the worker binds to
	// it and creates nothing, so its run time and retry cap stay the operator's.
	Queue string `json:"queue,omitempty" yaml:"queue,omitempty"`
	// TaskType is the asyncjobs task type this worker handles. A task of another type
	// on the same queue is not this worker's and is left alone, so a submitter and a
	// worker that disagree produce a job nobody runs and nobody reports.
	TaskType string `json:"task_type,omitempty" yaml:"task_type,omitempty"`
	// Workers is how many jobs this process runs at once. The --workers flag overrides
	// it, since the number is a property of the process rather than of the agent and a
	// configuration is often shared by every container that reads it.
	//
	// It cannot raise throughput past the queue's own concurrency, which bounds every
	// worker on that queue together; above it this process simply holds slots it never
	// fills. Lowering it is the useful direction, and one per container is a deployment
	// shape rather than a degraded one.
	Workers int `json:"workers,omitempty" yaml:"workers,omitempty"`
	// NatsContext is the NATS context the queue is reached over, defaulting to the
	// top-level nats_context. It is dialed separately from the shared connection
	// because the queue engine requires a connection option nothing else wants, which
	// also means the queue may live on a different cluster from the session store and
	// remote tools.
	NatsContext string `json:"nats_context,omitempty" yaml:"nats_context,omitempty"`
	// MaxPayload bounds a task payload, in bytes, before anything decodes it; 0 uses
	// the worker's own default. It is configurable because it is the only bound an
	// operator has on a third party's input to a surface whose sole access control is
	// permission to write to the queue.
	MaxPayload int `json:"max_payload,omitempty" yaml:"max_payload,omitempty"`
}

// RemoteAgent is a remote agent we can talk to using a2a-like behaviors.
type RemoteAgent struct {
	// Name is the remote agent's identity.
	Name string `yaml:"name" json:"name"`
	// Alias is a short local name for the remote agent.
	Alias string `yaml:"alias,omitempty" json:"alias,omitempty"`
}

// RemoteToolHost is a remote agent we pull in all the tools of.
type RemoteToolHost struct {
	// Name is the remote agent's identity.
	Name string `yaml:"name" json:"name"`
	// Alias is a short local name for the remote tool host.
	Alias string `yaml:"alias,omitempty" json:"alias,omitempty"`
	// Exclude filters out tools from this host (same semantics as the top level).
	Exclude *ToolFilter `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	// Include restricts tools from this host (same semantics as the top level).
	Include *ToolFilter `json:"include,omitempty" yaml:"include,omitempty"`
}

// EffectiveAlias is the prefix applied to tools imported from this host: the
// configured Alias when set, otherwise the host's identity. Imported tools are
// named "<alias>_<remote tool name>" so they carry their provenance and stay
// distinct from local tools and from other hosts' tools.
func (h RemoteToolHost) EffectiveAlias() string {
	if h.Alias != "" {
		return h.Alias
	}

	return h.Name
}

// ToolFilter is a generic filter selecting tools by name or tag. It is used at
// several levels: top-level include/exclude, per remote tool host, and when
// exposing tools.
type ToolFilter struct {
	// Tools is an explicit list of tool names, regex matched.
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	// Tags matches tools by tag. `ai:deny` is always active and can never be
	// included; "" matches untagged commands.
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`
}

// Mode selects which set of required fields a config is validated against. The
// same file drives both the agent (run) and the MCP server, but each needs a
// different subset of fields.
type Mode int

const (
	// ModeAgent validates a config for running the LLM agent: it needs a prompt
	// and a model in addition to the application.
	ModeAgent Mode = iota
	// ModeMCP validates a config for serving tools over MCP: only the application
	// is needed, since there is no prompt or model in that mode.
	ModeMCP
	// ModeServer validates a config for the a2a server: it serves the application's
	// tools to remote agents over NATS, so it needs the application, a valid
	// identity (it keys the discovery and tool subjects and the queue group), and a
	// NATS context. Like MCP it uses neither a prompt nor a model.
	ModeServer
)

// NewConfig returns a Config with default budgets applied.
func NewConfig() *Config {
	cfg := &Config{}

	cfg.prepare()

	return cfg
}

// ParseConfigFile reads the YAML config at path and parses it for agent mode.
func ParseConfigFile(path string) (*Config, error) {
	return ParseConfigFileForMode(path, ModeAgent)
}

// ParseConfigFileForMode reads the YAML config at path and parses it for the
// given mode.
func ParseConfigFileForMode(path string, mode Mode) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	return ParseConfigForMode(data, mode)
}

// ParseConfig parses the YAML config in data for agent mode.
func ParseConfig(data []byte) (*Config, error) {
	return ParseConfigForMode(data, ModeAgent)
}

// ParseConfigForMode parses the YAML config in data, applies default budgets,
// parses duration strings, and validates the result against the given mode.
func ParseConfigForMode(data []byte, mode Mode) (*Config, error) {
	cfg := &Config{}
	// UseJSONUnmarshaler makes goccy populate json.RawMessage fields (such as a
	// memory backend's options block) with canonical JSON, so a raw sub-block can
	// be decoded later against a typed per-backend schema regardless of whether the
	// config was YAML or, in future, JSON.
	if err := yaml.UnmarshalWithOptions(data, cfg, yaml.DisallowUnknownField(), yaml.UseJSONUnmarshaler()); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.prepare(); err != nil {
		return nil, err
	}

	if err := ValidateForMode(cfg, mode); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that required fields are set for agent mode.
func Validate(cfg *Config) error {
	return ValidateForMode(cfg, ModeAgent)
}

// ValidateForMode checks that the fields required by mode are set. application_path
// is optional for ModeAgent and ModeMCP, which can run on built-in and remote tools
// alone; ModeServer still requires it, since no built-in declares a2a exposure and
// an application-less a2a server would therefore serve nothing. ModeMCP needs nothing more, since it
// serves tools and uses neither a prompt nor a model. ModeServer needs a valid
// identity and a NATS context but, like MCP, no prompt or model. ModeAgent
// additionally needs a model, and a prompt and identity unless the agent is also
// exposed over MCP.
func ValidateForMode(cfg *Config, mode Mode) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	// global_flags names flags on the wrapped application, so it has nothing to
	// attach to without one. GlobalFlags is normalized by prepare, so an empty
	// slice here means no usable flag was configured.
	if len(cfg.GlobalFlags) > 0 && cfg.ApplicationPath == "" {
		return fmt.Errorf("global_flags is set but application_path is not: global flags are the wrapped application's own globals and have nothing to attach to without an application; remove global_flags or set application_path")
	}

	// The identity doubles as the discovery queue group, so when set it must be a
	// legal NATS subject token. It defaults to the application binary's basename,
	// which can carry illegal characters (a dot, a space), so check it in every
	// mode whenever it is non-empty rather than only where it is required.
	if cfg.Identity != "" && !identityPattern.MatchString(cfg.Identity) {
		return fmt.Errorf("identity %q is invalid: it must contain only letters, digits, '-' or '_' (it doubles as a NATS queue group); set an explicit identity if the application binary name contains other characters", cfg.Identity)
	}

	// Remote tool hosts are consulted whenever the agent runs or is inspected, so
	// validate them in every mode rather than only where they are imported.
	if err := validateRemoteToolHosts(cfg.RemoteTools); err != nil {
		return err
	}

	if mode == ModeServer {
		// The a2a server can carry any tool kind, but no built-in declares a2a
		// exposure today, so an application-less a2a server would start with an empty
		// tool set. This is an earlier, clearer version of the empty-set error
		// a2a_command already produces, not a correctness gate: when a built-in first
		// opts into a2a, delete this and let the downstream check do the work.
		if cfg.ApplicationPath == "" {
			return fmt.Errorf("application_path is required for the a2a server: no built-in tool declares a2a exposure, so an a2a server with no wrapped application would have nothing to serve; set application_path to the fisk application whose commands you want to serve")
		}
		if cfg.NatsContext == "" {
			return fmt.Errorf("nats_context is required for the a2a server")
		}
		return nil
	}

	if mode == ModeMCP {
		return nil
	}

	// ModeAgent: a config that imports remote tools must say how to reach NATS.
	if len(cfg.RemoteTools) > 0 && cfg.NatsContext == "" {
		return fmt.Errorf("nats_context is required when remote_tools is set")
	}

	// An MCP-only agent needs neither: it serves tools and runs no agent loop. A jobs
	// intake runs the whole loop, so it needs both, and the waiver must not extend to a
	// config that carries an mcp block as well. Without this a config with both parses
	// clean and fails later inside the channel, naming no key in the file.
	mcpOnly := cfg.Expose != nil && cfg.Expose.Agent != nil && cfg.Expose.Agent.MCP != nil
	jobs := cfg.Expose != nil && cfg.Expose.Agent != nil && cfg.Expose.Agent.Jobs != nil
	if !mcpOnly || jobs {
		if cfg.Identity == "" {
			return fmt.Errorf("identity is required unless exposed over MCP")
		}
		if cfg.SystemPrompt == "" {
			return fmt.Errorf("prompt is required unless exposed over MCP")
		}
	}

	// The queue is reached over NATS, so an intake with no way to get there is a
	// configuration that cannot start rather than one that starts and does nothing.
	if jobs && cfg.NatsContext == "" && cfg.Expose.Agent.Jobs.NatsContext == "" {
		return fmt.Errorf("nats_context is required when expose.agent.jobs is set, either at the top level or under the block")
	}

	if cfg.LLM.Model == "" {
		return fmt.Errorf("llm.model is required")
	}

	return nil
}

// validateRemoteToolHosts checks each remote tool host's identity, alias, and
// filters. The host name keys the NATS subjects, so it must be a legal subject
// token; the alias, when set, prefixes imported tool names, so it must be a legal
// tool-name token. A tag-based exclude is rejected outright: discovery does not
// carry tags (a ToolDescriptor has only a name, description and input schema), so
// an exclude-by-tag could never be honored and would silently leave a tool the
// operator meant to remove imported anyway. An include-by-tag is not an error
// here; the importing command warns and ignores it.
func validateRemoteToolHosts(hosts []RemoteToolHost) error {
	for _, host := range hosts {
		if host.Name == "" {
			return fmt.Errorf("remote_tools host is missing a name")
		}
		if !identityPattern.MatchString(host.Name) {
			return fmt.Errorf("remote_tools host name %q is invalid: it must contain only letters, digits, '-' or '_' (it keys the NATS subjects)", host.Name)
		}
		if host.Alias != "" && !identityPattern.MatchString(host.Alias) {
			return fmt.Errorf("remote_tools host %q has an invalid alias %q: it must contain only letters, digits, '-' or '_' (it prefixes imported tool names)", host.Name, host.Alias)
		}
		if host.Exclude != nil && len(host.Exclude.Tags) > 0 {
			return fmt.Errorf("remote_tools host %q has an exclude.tags filter, which cannot be honored: discovery does not carry tags, so a tool excluded by tag would be imported anyway; exclude by tool name instead", host.Name)
		}
	}

	return nil
}

// HumanInTheLoopEnabled reports whether the built-in human-in-the-loop tools are
// enabled. They are only ever active in agent mode.
func (c *Config) HumanInTheLoopEnabled() bool {
	return c.Harness.HumanInTheLoop != nil && c.Harness.HumanInTheLoop.Enabled
}

// MemoryEnabled reports whether the built-in memory tools are enabled. Like the
// human-in-the-loop tools they are only ever active in agent mode.
func (c *Config) MemoryEnabled() bool {
	return c.Harness.Memory != nil && c.Harness.Memory.Enabled
}

// MemoryIndexEnabled reports whether the list of stored memories should be
// injected into the system prompt at run start. It requires memory to be enabled
// and no_index to be unset.
func (c *Config) MemoryIndexEnabled() bool {
	return c.MemoryEnabled() && !c.Harness.Memory.NoIndex
}

// MemoryBackend returns the configured memory backend, defaulting to "file" when
// memory is enabled but no backend is named. It returns "" when memory is off.
func (c *Config) MemoryBackend() string {
	if !c.MemoryEnabled() {
		return ""
	}
	if c.Harness.Memory.Backend == "" {
		return "file"
	}

	return c.Harness.Memory.Backend
}

// MemoryBackendDeclared returns the memory backend the operator wrote in the file,
// or "" when they named none and the default applies. It differs from MemoryBackend
// in reporting what was asked for rather than what will be used.
//
// It exists so a caller injecting a store can tell an operator who chose a backend
// from one who left the choice open. Injecting a store against a declared backend
// names two stores and is refused; injecting against no declaration replaces nothing.
func (c *Config) MemoryBackendDeclared() string {
	if !c.MemoryEnabled() {
		return ""
	}

	return c.Harness.Memory.Backend
}

// MemoryRawOptions returns the raw backend options block, decoded per backend at
// store construction. It is nil when memory is off or no options are set.
func (c *Config) MemoryRawOptions() json.RawMessage {
	if !c.MemoryEnabled() {
		return nil
	}

	return c.Harness.Memory.Options
}

// SessionBackend returns the configured session store backend, defaulting to
// "file". Unlike MemoryBackend it never returns "": sessions are not a feature
// that can be disabled, so an unset config still resolves to the file backend.
func (c *Config) SessionBackend() string {
	return c.Harness.Sessions.BackendName()
}

// SessionBackendDeclared returns the session backend the operator wrote in the
// file, or "" when they named none and the default applies. It is the session
// counterpart of MemoryBackendDeclared and exists for the same reason.
//
// ApplyStateDir declares the file backend, so a --state-dir run reports "file"
// rather than "": the flag is an operator naming a backend as much as the block is.
func (c *Config) SessionBackendDeclared() string {
	return c.Harness.Sessions.DeclaredBackend()
}

// SessionRawOptions returns the raw session backend options block, decoded per
// backend at store construction. It is nil when no options are set.
func (c *Config) SessionRawOptions() json.RawMessage {
	return c.Harness.Sessions.RawOptions()
}

// RAGEnabled reports whether the built-in knowledge_search tool is enabled. Like
// the other harness tools it is only ever active in agent mode. It is the
// block-only gate for the lexical baseline; the vector tier has its own gate.
func (c *Config) RAGEnabled() bool {
	return c.Harness.RAG != nil && c.Harness.RAG.Enabled
}

// RAGVectorEnabled reports whether the opt-in vector tier is on: RAG enabled and
// an embeddings sub-block present. It is the second, independent gate; a lexical
// index needs neither a model nor a server.
func (c *Config) RAGVectorEnabled() bool {
	return c.RAGEnabled() && c.Harness.RAG.Embeddings != nil
}

// otlpCredentialEnvNames are the OpenTelemetry export variables that carry a
// credential. They are stripped from every tool subprocess unconditionally, whether
// or not this agent enables telemetry, because they are ambient operator variables
// that are present regardless of what one agent's config says, a tool subprocess
// never needs them, and gating on config would mean --no-telemetry re-exposes the
// token it was reached for to suppress. This follows the same precedent as the
// anthropic provider registering its credential variables with no gate on whether
// custom headers are configured.
//
// The four headers variables hold a bearer token directly. The mTLS variables name a
// *file path* rather than holding a key, so hiding the name does not make the key
// file unreadable by the same uid; they are stripped anyway because a tool that
// cannot find the path is a tool that has to work for it, but this is the documented
// limit of a name-based scrub rather than a claim that the mTLS case is closed.
//
// internal/telemetry lists the four headers names again for two startup checks. The
// duplication is deliberate and matches mcpExposableBuiltins above: config is the
// lowest layer and imports nothing from the tree. A spec asserts this list is a
// superset of that one, so the two cannot drift apart unnoticed.
var otlpCredentialEnvNames = []string{
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	// Included even though fisk-ai exports no logs: it will be in a real operator's
	// shell alongside the others, and a scrub that leaves one of a set behind is worse
	// than no scrub, because it reads as covered.
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	"OTEL_EXPORTER_OTLP_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY",
	"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE",
}

// CredentialEnvNames returns the names of the environment variables that config
// identifies as holding a credential, so a caller can strip them from the
// environment of a subprocess whose command line the model chooses (see
// internal/toolkit/fisk). It is the single seam a future provider extends: any
// operator-named secret variable belongs here, never a static denylist. Names are
// trimmed, empties dropped, and duplicates removed.
//
// It is never empty: the OpenTelemetry export credentials are always included, for
// the reasons on otlpCredentialEnvNames. The operator-named additions today are the
// optional RAG embeddings bearer-token variable.
func (c *Config) CredentialEnvNames() []string {
	names := slices.Clone(otlpCredentialEnvNames)
	if c.RAGVectorEnabled() {
		names = append(names, c.Harness.RAG.Embeddings.APIKeyEnv)
	}

	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}

	return out
}

// TelemetryEnabled reports whether the agent config turns OpenTelemetry export on.
// It is only the config half of the answer: a --no-telemetry flag, NO_TELEMETRY, or
// OTEL_SDK_DISABLED still vetoes it, which internal/telemetry resolves.
func (c *Config) TelemetryEnabled() bool {
	return c.Telemetry.Enabled
}

// TelemetryMetricsEnabled reports whether the metric pipeline should run alongside
// traces. It is on unless the agent config sets no_metrics, mirroring BellEnabled.
// Like TelemetryEnabled it says nothing about whether telemetry is on at all.
func (c *Config) TelemetryMetricsEnabled() bool {
	return !c.Telemetry.NoMetrics
}

// TelemetryCaptureEnabled reports whether the run exports the conversation itself and
// not only its structure and timing. Like the two above it says nothing about whether
// telemetry is on at all; capture without export does nothing.
func (c *Config) TelemetryCaptureEnabled() bool {
	return c.Telemetry.Capture != nil && c.Telemetry.Capture.Enabled
}

// TelemetryCaptureMessages returns the configured message scope, empty when the block
// is absent or says nothing. Resolution and defaulting belong to internal/telemetry,
// which is where every other effective telemetry value is decided; this returns what
// the file said.
func (c *Config) TelemetryCaptureMessages() string {
	if c.Telemetry.Capture == nil {
		return ""
	}

	return c.Telemetry.Capture.Messages
}

// TelemetryCaptureMaxBytes returns the configured per-attribute cap, zero when unset.
func (c *Config) TelemetryCaptureMaxBytes() int {
	if c.Telemetry.Capture == nil {
		return 0
	}

	return c.Telemetry.Capture.MaxBytes
}

// ConfirmTags returns the extra confirmation gate tags configured under the
// harness block, additive to the always-on ai:confirm tag. It is nil when none
// are set; prepare normalizes the stored slice (trim, de-duplicate, drop empties).
func (c *Config) ConfirmTags() []string {
	return c.Harness.ConfirmTags
}

// TUIDisabled reports whether the agent config turns off the full-screen terminal
// UI. When true the run uses the line UI even on an interactive terminal, and no
// command-line flag can re-enable it.
func (c *Config) TUIDisabled() bool {
	return c.Harness.NoTUI
}

// BellEnabled reports whether the full-screen UI should ring the terminal bell when a
// run blocks on an operator decision. It is on unless the agent config sets no_bell.
func (c *Config) BellEnabled() bool {
	return !c.Harness.NoBell
}

// PromptCacheEnabled reports whether Anthropic prompt caching should be applied to
// this agent's requests. It is on unless the agent config sets no_prompt_cache, which
// is the escape hatch for a non-Anthropic endpoint whose proxy rejects cache_control.
func (c *Config) PromptCacheEnabled() bool {
	return !c.LLM.NoPromptCache
}

// ThinkingEnabled reports whether the model should be asked to expose its
// reasoning. Off unless explicitly enabled under llm.thinking.
func (c *Config) ThinkingEnabled() bool {
	return c.LLM.Thinking.Enabled
}

// ToolSearchEnabled reports whether server-side tool search may be used for this
// agent. It is on unless the agent config sets no_tool_search, the escape hatch for
// an endpoint that does not implement the tool search tool. It is only the operator
// half of the gate: tool search is used when the active provider also supports it and
// the tool count crosses the threshold.
func (c *Config) ToolSearchEnabled() bool {
	return !c.LLM.NoToolSearch
}

// LLMProvider returns the configured model backend, defaulting to "anthropic" when
// llm.provider is unset so a zero-config agent keeps working. An unlinked value is
// not rejected here; it fails at run start when the provider is resolved from the
// registry, with the list of providers linked into this build.
func (c *Config) LLMProvider() string {
	if c.LLM.Provider != "" {
		return c.LLM.Provider
	}
	return defaultLLMProvider
}

// MCPPort returns the MCP server port configured under expose.agent.mcp, or 0 if
// none is set. Callers layer their own default and flag override on top.
func (c *Config) MCPPort() int {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil {
		return 0
	}

	return c.Expose.Agent.MCP.Port
}

// MCPAddress returns the host or IP the MCP server binds to as configured under
// expose.agent.mcp, or "" if none is set. Callers layer their own flag override
// and loopback default on top.
func (c *Config) MCPAddress() string {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil {
		return ""
	}

	return c.Expose.Agent.MCP.Address
}

// MCPMaxConcurrentTools returns the configured MCP tool concurrency, or 0 when unset;
// the server applies its own default for 0.
func (c *Config) MCPMaxConcurrentTools() int {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil {
		return 0
	}

	return c.Expose.Agent.MCP.MaxConcurrentTools
}

// MCPToolTimeout returns the configured MCP per-tool-call timeout, or 0 when unset;
// the server applies its own default for 0.
func (c *Config) MCPToolTimeout() time.Duration {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil {
		return 0
	}

	return c.Expose.Agent.MCP.ToolTimeoutParsed
}

// A2AMaxConcurrentTools returns the configured a2a tool concurrency, or 0 when unset;
// the server applies its own default for 0.
func (c *Config) A2AMaxConcurrentTools() int {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.A2A == nil {
		return 0
	}

	return c.Expose.Agent.A2A.MaxConcurrentTools
}

// ToolTimeout returns the configured per-tool-call timeout for the agent loop, or 0
// when unset. Zero means unbounded on the run path; a hosted worker applies a default
// of its own for 0, as the MCP and a2a servers do for theirs.
func (c *Config) ToolTimeout() time.Duration {
	return c.Harness.ToolTimeoutParsed
}

// A2AToolTimeout returns the configured a2a per-tool-call timeout, or 0 when unset;
// the server applies its own default for 0.
func (c *Config) A2AToolTimeout() time.Duration {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.A2A == nil {
		return 0
	}

	return c.Expose.Agent.A2A.ToolTimeoutParsed
}

// MCPInstructions returns the optional instructions configured under
// expose.agent.mcp, or "" if none is set. The MCP server sends them to clients
// at connection time only when non-empty.
func (c *Config) MCPInstructions() string {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil {
		return ""
	}

	return c.Expose.Agent.MCP.Instructions
}

// ConfirmOverMCPMode returns the configured confirm-over-MCP policy from
// expose.agent.mcp, defaulting to auto when no MCP block or value is set. prepare
// normalizes and validates the stored value, so this returns one of the three
// known policies.
func (c *Config) ConfirmOverMCPMode() string {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil || c.Expose.Agent.MCP.ConfirmOverMCP == "" {
		return ConfirmOverMCPAuto
	}

	return c.Expose.Agent.MCP.ConfirmOverMCP
}

// A2AEnabled reports whether this agent opts in to serving its tools to other
// agents over a2a, set under expose.agent.agent_to_agent. Serving is off unless
// explicitly enabled, so a config that says nothing exposes nothing.
func (c *Config) A2AEnabled() bool {
	return c.Expose != nil && c.Expose.Agent != nil && c.Expose.Agent.AgentToAgent
}

// A2ATransportName is the a2a transport binding in use. It is fixed to NATS until
// a second transport lands, at which point it becomes a configurable field; see
// A2ATransport.
const A2ATransportName = "nats"

// A2ATransport returns the name of the a2a transport binding to use, looked up in
// the a2a transport registry. It is fixed to NATS for now; a transport: config
// field is deferred until a second transport exists.
func (c *Config) A2ATransport() string {
	return A2ATransportName
}

// MCPEnabled reports whether this agent opts in to serving its tools over MCP.
// Presence of the expose.agent.mcp block is the switch; it also carries the
// listen port. Like a2a, a config that says nothing exposes nothing.
func (c *Config) MCPEnabled() bool {
	return c.Expose != nil && c.Expose.Agent != nil && c.Expose.Agent.MCP != nil
}

// JobsEnabled reports whether this agent takes work off a queue, which is the
// presence of expose.agent.jobs. Every field under it defaults, so an empty block
// enables the intake.
func (c *Config) JobsEnabled() bool {
	return c.Expose != nil && c.Expose.Agent != nil && c.Expose.Agent.Jobs != nil
}

// JobsQueue returns the work queue the jobs intake consumes, or the default when
// unset. It is empty when the intake is not configured.
func (c *Config) JobsQueue() string {
	if !c.JobsEnabled() {
		return ""
	}
	if c.Expose.Agent.Jobs.Queue == "" {
		return DefaultJobsQueue
	}

	return c.Expose.Agent.Jobs.Queue
}

// JobsTaskType returns the task type the jobs intake handles, or the default when
// unset. It is empty when the intake is not configured.
func (c *Config) JobsTaskType() string {
	if !c.JobsEnabled() {
		return ""
	}
	if c.Expose.Agent.Jobs.TaskType == "" {
		return DefaultJobsTaskType
	}

	return c.Expose.Agent.Jobs.TaskType
}

// JobsWorkers returns how many jobs the intake runs at once, or the default when
// unset. A caller layers its own flag override on top, since the flag wins.
func (c *Config) JobsWorkers() int {
	if !c.JobsEnabled() || c.Expose.Agent.Jobs.Workers <= 0 {
		return DefaultJobsWorkers
	}

	return c.Expose.Agent.Jobs.Workers
}

// JobsNatsContext returns the NATS context the queue is reached over, falling back
// to the top-level nats_context when the block does not name one.
func (c *Config) JobsNatsContext() string {
	if !c.JobsEnabled() || c.Expose.Agent.Jobs.NatsContext == "" {
		return c.NatsContext
	}

	return c.Expose.Agent.Jobs.NatsContext
}

// JobsMaxPayload returns the configured payload cap in bytes, or 0 to leave the
// worker's own default in place.
func (c *Config) JobsMaxPayload() int {
	if !c.JobsEnabled() {
		return 0
	}

	return c.Expose.Agent.Jobs.MaxPayload
}

// MCPBuiltins returns the built-in tools opted in to MCP exposure via
// expose.agent.mcp.builtins, normalized and validated by prepare. It is nil when
// none are set.
func (c *Config) MCPBuiltins() []string {
	if c.Expose == nil || c.Expose.Agent == nil || c.Expose.Agent.MCP == nil {
		return nil
	}

	return c.Expose.Agent.MCP.Builtins
}

// MCPExposesKnowledge reports whether any read-only knowledge built-in is
// allowlisted for MCP exposure. It is the gate mcp_command uses to decide whether
// to open the knowledge store, so it asks about the whole group rather than one
// name: an operator who allowlisted only knowledge_enumerate still needs the store
// opened, and gating on knowledge_search alone would serve them nothing.
func (c *Config) MCPExposesKnowledge() bool {
	for _, name := range mcpExposableBuiltins {
		if slices.Contains(c.MCPBuiltins(), name) {
			return true
		}
	}

	return false
}

// prepare fills in default budgets and parses all duration strings.
func (c *Config) prepare() error {
	if c.Identity == "" {
		if c.ApplicationPath != "" {
			c.Identity = filepath.Base(c.ApplicationPath)
		} else {
			c.Identity = defaultIdentity
		}
	}

	c.Harness.ConfirmTags = normalizeTags(c.Harness.ConfirmTags)
	c.GlobalFlags = normalizeGlobalFlags(c.GlobalFlags)

	if c.Expose != nil && c.Expose.Agent != nil && c.Expose.Agent.MCP != nil {
		mode, err := normalizeConfirmOverMCP(c.Expose.Agent.MCP.ConfirmOverMCP)
		if err != nil {
			return err
		}
		c.Expose.Agent.MCP.ConfirmOverMCP = mode

		builtins, err := c.normalizeMCPBuiltins(c.Expose.Agent.MCP.Builtins)
		if err != nil {
			return err
		}
		c.Expose.Agent.MCP.Builtins = builtins

		d, err := prepareServerToolLimits("expose.agent.mcp", c.Expose.Agent.MCP.MaxConcurrentTools, c.Expose.Agent.MCP.ToolTimeoutString)
		if err != nil {
			return err
		}
		c.Expose.Agent.MCP.ToolTimeoutParsed = d
	}

	if c.Expose != nil && c.Expose.Agent != nil && c.Expose.Agent.A2A != nil {
		d, err := prepareServerToolLimits("expose.agent.a2a", c.Expose.Agent.A2A.MaxConcurrentTools, c.Expose.Agent.A2A.ToolTimeoutString)
		if err != nil {
			return err
		}
		c.Expose.Agent.A2A.ToolTimeoutParsed = d
	}

	// 0s parses to zero and so reads as unset, which is unbounded on the run path and
	// the server default on a hosted worker. A negative is rejected rather than
	// silently producing an already-expired context that fails every tool call.
	if c.Harness.ToolTimeoutString != "" {
		d, err := fisk.ParseDuration(c.Harness.ToolTimeoutString)
		if err != nil {
			return fmt.Errorf("invalid harness.tool_timeout %q: %w", c.Harness.ToolTimeoutString, err)
		}
		if d < 0 {
			return fmt.Errorf("invalid harness.tool_timeout %q: must not be negative", c.Harness.ToolTimeoutString)
		}
		c.Harness.ToolTimeoutParsed = d
	}

	if err := c.LLM.Budget.prepare(); err != nil {
		return err
	}

	if c.Harness.RAG != nil && c.Harness.RAG.Embeddings != nil {
		if err := c.Harness.RAG.Embeddings.prepare(); err != nil {
			return err
		}
	}

	return nil
}

// defaultRAGEmbedTimeout is the per-request embeddings timeout applied when
// harness.knowledge.embeddings.timeout is unset.
const defaultRAGEmbedTimeout = 30 * time.Second

// prepare parses the embeddings request timeout, defaulting it when unset. A
// malformed duration fails loudly at parse time rather than on the first embed.
// The base_url and model are validated later at rag.Open, before the agent loop.
func (e *RAGEmbeddingsConfig) prepare() error {
	if e.TimeoutString == "" {
		e.TimeoutParsed = defaultRAGEmbedTimeout
		return nil
	}

	d, err := fisk.ParseDuration(e.TimeoutString)
	if err != nil {
		return fmt.Errorf("invalid knowledge.embeddings.timeout %q: %w", e.TimeoutString, err)
	}
	if d <= 0 {
		return fmt.Errorf("invalid knowledge.embeddings.timeout %q: must be positive", e.TimeoutString)
	}
	e.TimeoutParsed = d

	return nil
}

// normalizeConfirmOverMCP lower-cases and trims the confirm_over_mcp value,
// defaulting an empty value to auto and rejecting anything that is not one of the
// three known policies, so a typo fails loudly at parse time rather than silently
// selecting a weaker or stronger gate than the operator intended.
func normalizeConfirmOverMCP(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", ConfirmOverMCPAuto:
		return ConfirmOverMCPAuto, nil
	case ConfirmOverMCPAlways:
		return ConfirmOverMCPAlways, nil
	case ConfirmOverMCPNever:
		return ConfirmOverMCPNever, nil
	default:
		return "", fmt.Errorf("invalid confirm_over_mcp %q: must be auto, always or never", v)
	}
}

// normalizeMCPBuiltins trims and de-duplicates the expose.agent.mcp.builtins
// allowlist and validates every entry against the names an operator may select.
//
// This is the SELECTION half of MCP exposure: which of the servable built-ins this
// operator wants served. The CAPABILITY half, whether a tool may ever be served at
// all, is declared on the tool itself and applied by the serving surface, which can
// only narrow this list further and never widen it. Both gates apply, so a tool
// added to a built-in set is not served on the strength of a neighbour's selection.
// The duplication is deliberate: config is the lowest layer and cannot import the
// tools it names, so this list is maintained here and every entry must also declare
// MCP exposure on its Spec, or no operator can ever select it.
//
// A non-empty allowlist with knowledge disabled is rejected, since there would be
// nothing to serve.
func (c *Config) normalizeMCPBuiltins(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if !slices.Contains(mcpExposableBuiltins, name) {
			return nil, fmt.Errorf("expose.agent.mcp.builtins: %q is not an accepted built-in name; accepted: %s. The memory and ask_human_* built-ins are not offered over MCP because they need operator state or an operator at a terminal, and an MCP client is neither", name, strings.Join(mcpExposableBuiltins, ", "))
		}
		seen[name] = true
		out = append(out, name)
	}

	if len(out) > 0 && !c.RAGEnabled() {
		return nil, fmt.Errorf("expose.agent.mcp.builtins lists %s but knowledge is not enabled; add a harness.knowledge block with 'enabled: true' or remove them from builtins", strings.Join(out, ", "))
	}

	return out, nil
}

// AppToolFiltersConfigured reports whether the top-level include or exclude tool
// filters carry any patterns or tags. They only ever narrow the wrapped
// application's tools, so with no application_path they match nothing; callers use
// this to warn rather than silently ignore an operator's filter.
func (c *Config) AppToolFiltersConfigured() bool {
	if c.Include != nil && (len(c.Include.Tools) > 0 || len(c.Include.Tags) > 0) {
		return true
	}
	if c.Exclude != nil && (len(c.Exclude.Tools) > 0 || len(c.Exclude.Tags) > 0) {
		return true
	}

	return false
}

// GlobalFlagNames returns the configured allowlist of application global flag
// names to expose to the model. prepare normalizes the stored slice (trim, strip
// leading dashes, de-duplicate, drop empties). It is nil when none are set.
func (c *Config) GlobalFlagNames() []string {
	return c.GlobalFlags
}

// normalizeGlobalFlags trims each global flag name, strips its leading dashes so
// an operator can write the name as they type it on the command line (--context
// or context), drops empties, and removes duplicates while preserving first-seen
// order.
func normalizeGlobalFlags(names []string) []string {
	if len(names) == 0 {
		return names
	}

	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimLeft(strings.TrimSpace(name), "-")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}

	return out
}

// normalizeTags trims surrounding whitespace from each tag, drops empties, and
// removes duplicates while preserving first-seen order. Trimming matters for
// confirm tags: a trailing space would make a tag silently fail to match a real
// command tag, leaving a command the operator believes is gated able to run
// without confirmation.
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return tags
	}

	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}

	return out
}

// maxConcurrentToolsCeiling caps a server's max_concurrent_tools so a typo cannot
// request an absurd number of concurrent tool subprocesses.
const maxConcurrentToolsCeiling = 1024

// prepareServerToolLimits validates and parses the per-server tool limits shared by
// the MCP and a2a server blocks. path is the config path for error messages (e.g.
// "expose.agent.mcp"). A zero max_concurrent_tools or an empty tool_timeout is left
// for the server to default (an omitted key unmarshals to zero, so zero must be
// treated as unset rather than rejected); a negative or over-ceiling count, or an
// unparseable or negative duration, is rejected. It returns the parsed timeout, zero
// when unset.
func prepareServerToolLimits(path string, maxConcurrent int, timeout string) (time.Duration, error) {
	if maxConcurrent < 0 {
		return 0, fmt.Errorf("invalid %s.max_concurrent_tools %d: must not be negative", path, maxConcurrent)
	}
	if maxConcurrent > maxConcurrentToolsCeiling {
		return 0, fmt.Errorf("invalid %s.max_concurrent_tools %d: must not exceed %d", path, maxConcurrent, maxConcurrentToolsCeiling)
	}

	if timeout == "" {
		return 0, nil
	}

	d, err := fisk.ParseDuration(timeout)
	if err != nil {
		return 0, fmt.Errorf("invalid %s.tool_timeout %q: %w", path, timeout, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid %s.tool_timeout %q: must not be negative", path, timeout)
	}

	return d, nil
}

// prepare applies LLM budget defaults and parses the call timeout.
func (b *LLMBudget) prepare() error {
	if b.MaxTokens < 0 {
		return fmt.Errorf("invalid llm max_tokens %d: must not be negative", b.MaxTokens)
	}
	if b.MaxOutputTokens < 0 {
		return fmt.Errorf("invalid llm max_output_tokens %d: must not be negative", b.MaxOutputTokens)
	}
	if b.MaxIterations < 0 {
		return fmt.Errorf("invalid llm max_iterations %d: must not be negative", b.MaxIterations)
	}

	if b.MaxTokens == 0 {
		b.MaxTokens = defaultLLMMaxTokens
	}
	if b.MaxIterations == 0 {
		b.MaxIterations = defaultLLMMaxIterations
	}

	if b.CallTimeoutString == "" {
		b.CallTimeoutParsed = defaultLLMCallTimeout
		return nil
	}

	d, err := fisk.ParseDuration(b.CallTimeoutString)
	if err != nil {
		return fmt.Errorf("invalid llm call_timeout %q: %w", b.CallTimeoutString, err)
	}
	b.CallTimeoutParsed = d

	return nil
}
