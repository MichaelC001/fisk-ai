//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// This example embeds the agent in a Go program: it builds a configuration in code,
// supplies its own tool, memory store, session store and knowledge index, scripts the
// model from the agenttest harness, runs one agent turn and prints what came back.
//
// It lives in an external test package on purpose, so it reaches only exported API
// and is the shortest path an embedder has from an empty program to a completed run.
// It reaches no network and no broker: the model provider answers from a script, and
// the knowledge index is embedded by hashing words rather than by calling a server.
package runagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	memoryfile "github.com/choria-io/fisk-ai/internal/memory/file"
	"github.com/choria-io/fisk-ai/internal/rag"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// providerName is the llm.Provider registration this example answers model calls
// from, named in the configuration as llm.provider.
const providerName = "example-scripted"

// init registers the model provider. llm.Register must run before a configuration
// naming it is used, and registering the same name twice panics, so init is where a
// provider package puts it.
func init() {
	llm.Register(providerName, func(llm.Config) (llm.Provider, error) {
		return scriptedProvider()
	}, nil)
}

// Example runs an agent from start to finish in one process. The model is scripted
// to search the knowledge index, call a tool the program wrote in Go, write a
// memory, and answer.
func Example() {
	err := runAgent()
	if err != nil {
		fmt.Println("error:", err)
	}

	// Output:
	// index: files=1 chunks=1 embeddings=1
	// knowledge_search results: 1
	// lookup_ticket returned {"owner":"supply team","status":"open","ticket":"T-42"}
	// memory_write returned {"written":true}
	// answer: Ticket T-42 is open and the supply team owns the widget inventory.
	// outcome: completed
	// llm calls: 4
	// tool calls: 3
	// memory read back: The supply team owns it and reconciles it every Monday.
}

func runAgent() error {
	ctx := context.Background()

	// One temp directory holds the corpus, the knowledge index, the memories and the
	// run journal, so the example leaves nothing behind.
	root, err := os.MkdirTemp("", "runagent")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	cfg, err := buildConfig(root)
	if err != nil {
		return err
	}

	corpus, err := writeCorpus(root)
	if err != nil {
		return err
	}

	embedder := &hashEmbedder{model: "example-embedder", dim: 32}

	err = indexCorpus(ctx, cfg, corpus, embedder)
	if err != nil {
		return err
	}

	// The read-only handle the run searches. The same embedder is supplied here: an
	// index built with one is refused by an open that configures a different embedding
	// identity, and an open with none searches the index lexically.
	knowledge, err := rag.Open(cfg, "", rag.Options{Embedder: embedder})
	if err != nil {
		return err
	}
	defer knowledge.Close()

	memories, err := memoryfile.NewFileStore(filepath.Join(root, "memory"))
	if err != nil {
		return err
	}

	journal, err := runstatefile.NewFileStore(filepath.Join(root, "sessions"))
	if err != nil {
		return err
	}

	tool, err := ticketTool()
	if err != nil {
		return err
	}

	// agent.Events is how a run reports what it is doing. This recorder comes from the
	// agenttest harness and records every event for the program to read once the run
	// ends; a program wanting a log as the run goes hands agent.NewSlogEvents a logger,
	// and one wanting its own rendering implements the interface itself.
	events := agenttest.NewRecordingEvents()

	// The stores are supplied rather than built from the configuration, which is what
	// a process hosting many runs does: it opens each one once and hands the same
	// handle to every run. Run borrows them and closes none of them.
	res, err := agent.Run(ctx, agent.Options{
		Config:       cfg,
		ConfigFile:   "(built in Go)",
		Prompt:       []string{"look up ticket T-42 and tell me who owns the widget inventory"},
		StoreDir:     root,
		Checkpoint:   agent.Checkpoint{Enabled: true},
		RAGStore:     knowledge,
		MemoryStore:  memories,
		SessionStore: journal,
		CustomTools:  []toolkit.Tool{tool},
	}, events, toolkit.DefaultDenyPrompter())
	if err != nil {
		return err
	}

	narrate(events)

	fmt.Println("answer:", res.Text)
	fmt.Println("outcome:", res.Reason)
	fmt.Println("llm calls:", res.Stats.LlmCalls)
	fmt.Println("tool calls:", res.Stats.ToolCalls)

	// The memory the model wrote is readable through the caller's own handle after
	// the run, since Run used the store it was given.
	_, content, err := memories.Read(ctx, "widget.inventory")
	if err != nil {
		return err
	}
	fmt.Println("memory read back:", strings.TrimSpace(content))

	return nil
}

// buildConfig assembles the configuration in Go rather than reading a file. Prepare
// derives the identity, fills the default budgets and parses the duration strings,
// so it runs after the last field is set.
func buildConfig(root string) (*config.Config, error) {
	cfg, err := config.NewConfig()
	if err != nil {
		return nil, err
	}

	cfg.Identity = "runagent-example"
	cfg.SystemPrompt = "You answer questions about the widget inventory."
	cfg.LLM.Provider = providerName
	cfg.LLM.Model = "example-model"
	cfg.LLM.Budget.MaxIterations = 10
	cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}
	cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: filepath.Join(root, "knowledge")}

	err = cfg.Prepare()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// writeCorpus lays down the documents the knowledge index is built from.
func writeCorpus(root string) (string, error) {
	dir := filepath.Join(root, "docs")

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return "", err
	}

	doc := "# Widget inventory\n\nThe widget inventory is owned by the supply team and reconciled every Monday.\n"

	err = os.WriteFile(filepath.Join(dir, "inventory.md"), []byte(doc), 0o600)
	if err != nil {
		return "", err
	}

	return dir, nil
}

// indexCorpus builds the knowledge index. A writer holds a cross-process advisory
// lock for as long as it is open, so it is closed before the run reads the index.
func indexCorpus(ctx context.Context, cfg *config.Config, corpus string, embedder rag.Embedder) error {
	writer, err := rag.OpenWriter(cfg, "", rag.Options{Embedder: embedder})
	if err != nil {
		return err
	}
	defer writer.Close()

	stats, err := writer.Index(ctx, []string{corpus}, rag.IndexOptions{Reconcile: true})
	if err != nil {
		return err
	}

	fmt.Printf("index: files=%d chunks=%d embeddings=%d\n", stats.Files, stats.Chunks, stats.Embeddings)

	return writer.Close()
}

// ticketTool is a tool the program implements in Go. The model addresses it by name
// beside the built-ins, and the handler runs in-process with the program's own
// privileges rather than in a subprocess.
func ticketTool() (*functool.Tool, error) {
	return functool.New(functool.Spec{
		Name:        "lookup_ticket",
		Description: "look up a support ticket by id",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "the ticket id"},
			},
			"required": []any{"id"},
		},
		ValidateRequired: true,
		Handler: func(_ context.Context, input json.RawMessage, _ *functool.CallContext) (string, error) {
			var args struct {
				ID string `json:"id"`
			}

			err := json.Unmarshal(input, &args)
			if err != nil {
				return "", err
			}

			return functool.Result(map[string]any{"ticket": args.ID, "status": "open", "owner": "supply team"})
		},
		// A tool with no Trace renderer runs silently: its call and result are shown
		// only under verbose. This one renders its call as a single line, the way a
		// command tool's is rendered.
		Trace: func(input json.RawMessage) string {
			var args struct {
				ID string `json:"id"`
			}

			err := json.Unmarshal(input, &args)
			if err != nil {
				return "lookup_ticket"
			}

			return "lookup_ticket " + args.ID
		},
	})
}

// scriptedProvider answers each model call with the next response in a fixed list,
// which is what makes the example deterministic and keeps it off the network. A real
// provider renders the request to its backend's wire format and converts the reply
// back.
//
// A func Example is handed no testing.TB, so it calls agenttest.BuildScriptedProvider
// rather than NewScriptedProvider.
func scriptedProvider() (llm.Provider, error) {
	search := json.RawMessage(`{"query":"who owns the widget inventory"}`)
	ticket := json.RawMessage(`{"id":"T-42"}`)
	write := json.RawMessage(`{"key":"widget.inventory","description":"who owns the widget inventory","content":"The supply team owns it and reconciles it every Monday."}`)

	p, err := agenttest.BuildScriptedProvider(
		agenttest.ToolUseResponse("call-1", "knowledge_search", search),
		agenttest.ToolUseResponse("call-2", "lookup_ticket", ticket),
		agenttest.ToolUseResponse("call-3", "memory_write", write),
		agenttest.TextResponse("Ticket T-42 is open and the supply team owns the widget inventory."),
	)
	if err != nil {
		return nil, err
	}

	// Capabilities are declared rather than discovered. Provider is the id a checkpoint
	// pins, so a resume against a different provider is refused.
	p.SetCapabilities(llm.Caps{Provider: providerName, SemconvProvider: providerName})

	return p, nil
}

// narrate prints what the run reported once it has ended: the warnings it raised, any
// crash it recovered from, and what each tool answered.
//
// It pairs each result with the call it answers through the tool_use id, since a turn
// may carry several calls and a call may produce no result at all.
func narrate(events *agenttest.RecordingEvents) {
	for _, w := range events.Warnings() {
		fmt.Println("warning:", w.Kind)
	}

	// The stack a crash carries holds absolute paths and frame arguments, so this
	// prints the recovered value and leaves the stack where it was captured.
	for _, p := range events.Panics() {
		fmt.Println("the run crashed:", p.Value)
	}

	names := make(map[string]string)
	for _, c := range events.ToolCalls() {
		names[c.ID] = c.Name
	}

	for _, t := range events.ToolResults() {
		name := names[t.CallID]

		// The knowledge result carries the absolute path of the document it cites, so
		// this prints how many results came back rather than the result itself.
		if name != "knowledge_search" {
			fmt.Printf("%s returned %s\n", name, t.Output)

			continue
		}

		var hits struct {
			Results []json.RawMessage `json:"results"`
		}

		err := json.Unmarshal([]byte(t.Output), &hits)
		if err != nil {
			fmt.Println("knowledge_search returned unreadable output:", err)

			continue
		}

		fmt.Printf("knowledge_search results: %d\n", len(hits.Results))
	}
}

// hashEmbedder embeds text by hashing its words into a fixed number of dimensions,
// so a query sharing words with a chunk lands near it under L2 distance and no
// request leaves the process. A real embedder calls a model.
type hashEmbedder struct {
	model string
	dim   int
}

func (e *hashEmbedder) Model() string                    { return e.model }
func (e *hashEmbedder) QueryPrefix() string              { return "query: " }
func (e *hashEmbedder) DocumentPrefix() string           { return "title: {title} | " }
func (e *hashEmbedder) Dim(context.Context) (int, error) { return e.dim, nil }

func (e *hashEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}

func (e *hashEmbedder) EmbedDocuments(_ context.Context, docs []rag.Document) ([][]float32, error) {
	out := make([][]float32, len(docs))
	for i, d := range docs {
		out[i] = e.vec(d.Text)
	}

	return out, nil
}

func (e *hashEmbedder) vec(text string) []float32 {
	v := make([]float32, e.dim)

	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%uint32(e.dim)]++
	}
	// A zero vector has no direction, so every text gets a small constant component.
	v[0] += 0.001

	return v
}
