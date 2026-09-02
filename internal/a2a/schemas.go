//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/v1/*.json
var schemaFS embed.FS

const (
	schemaDir     = "schemas/v1"
	schemaBaseURL = "https://choria.io/schemas/io.choria.fisk-ai.v1"
)

// protocolSchemaFile maps each message protocol id to the schema that validates
// it. common.json holds shared definitions and validates no message directly.
var protocolSchemaFile = map[string]string{
	// One per thing a caller can ask for, since each is a message id of its own. Each
	// states what its own shape requires and refuses the fields belonging to its
	// siblings, so a body disagreeing with its id is refused before it is decoded.
	RequestPromptProtocol: "request.prompt.json",
	RequestAnswerProtocol: "request.answer.json",
	RequestResumeProtocol: "request.resume.json",
	RequestReadProtocol:   "request.read.json",

	ResultProtocol:      "result.json",
	ErrorProtocol:       "error.json",
	CancelProtocol:      "cancel.json",
	AckProtocol:         "ack.json",
	ToolRequestProtocol: "tool.request.json",
	ToolReplyProtocol:   "tool.reply.json",

	// One per kind of question and one per answer, since each is a message id of its
	// own. A question this build does not name is refused rather than validated on its
	// framing: a caller that cannot read a question cannot put it to anybody.
	ElicitRequestApproveProtocol: "elicit.request.approve.json",
	ElicitRequestConfirmProtocol: "elicit.request.confirm.json",
	ElicitRequestSelectProtocol:  "elicit.request.select.json",
	ElicitRequestInputProtocol:   "elicit.request.input.json",

	ElicitReplyApproveProtocol:    "elicit.reply.approve.json",
	ElicitReplyConfirmProtocol:    "elicit.reply.confirm.json",
	ElicitReplySelectProtocol:     "elicit.reply.select.json",
	ElicitReplyInputProtocol:      "elicit.reply.input.json",
	ElicitReplyNoOperatorProtocol: "elicit.reply.no_operator.json",

	ElicitWaitingProtocol: "elicit.waiting.json",

	DiscoveryRequestProtocol: "discovery.request.json",
	DiscoveryReplyProtocol:   "discovery.reply.json",

	// One per kind of block, since each is a message id of its own.
	EventThinkingProtocol:   "event.thinking.json",
	EventTextProtocol:       "event.text.json",
	EventToolCallProtocol:   "event.tool_call.json",
	EventToolResultProtocol: "event.tool_result.json",
	EventAgentCallProtocol:  "event.agent_call.json",
	EventStatusProtocol:     "event.status.json",
	EventWarningProtocol:    "event.warning.json",
	EventPromptProtocol:     "event.prompt.json",

	EventTextDeltaProtocol:     "event.text_delta.json",
	EventThinkingDeltaProtocol: "event.thinking_delta.json",
}

// eventFallbackSchemaFile validates an event whose kind this build does not name. It
// is keyed by no id, being what an id with no schema of its own falls back to, and it
// checks the framing rather than the block: the header, the sequence number and that
// a block is there at all.
const eventFallbackSchemaFile = "event.json"

// Validator validates message bodies against the embedded v1 JSON schemas.
//
// The schemas accept properties they do not name and a stop reason they do not name, so
// a peer on a newer schema does not lose a whole message to one thing this build has
// never heard of. An event of a kind they do not name is accepted on its framing alone,
// since the kind is the message's protocol id and a schema for it is what is missing.
// Everything else is enforced as before: required fields, types, patterns, the protocol
// const, and every kind of block the schemas do name, each against its own file.
type Validator struct {
	schemas map[string]*jsonschema.Schema
	// eventFallback validates an event of a kind this build does not name, which has
	// no schema of its own to be looked up by.
	eventFallback *jsonschema.Schema
}

// NewValidator compiles the embedded schemas. The compiled Validator is
// safe for concurrent use and is intended to be built once and reused.
func NewValidator() (*Validator, error) {
	compiler := jsonschema.NewCompiler()

	entries, err := fs.ReadDir(schemaFS, schemaDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		raw, err := schemaFS.ReadFile(schemaDir + "/" + entry.Name())
		if err != nil {
			return nil, err
		}

		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parsing schema %s: %w", entry.Name(), err)
		}

		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: schema %s is not an object", ErrInvalidMessage, entry.Name())
		}

		id, ok := obj["$id"].(string)
		if !ok {
			return nil, fmt.Errorf("%w: schema %s has no $id", ErrInvalidMessage, entry.Name())
		}

		err = compiler.AddResource(id, doc)
		if err != nil {
			return nil, fmt.Errorf("adding schema %s: %w", entry.Name(), err)
		}
	}

	v := &Validator{schemas: make(map[string]*jsonschema.Schema, len(protocolSchemaFile))}

	for protocol, file := range protocolSchemaFile {
		sch, err := compiler.Compile(schemaBaseURL + "/" + file)
		if err != nil {
			return nil, fmt.Errorf("compiling schema %s: %w", file, err)
		}

		v.schemas[protocol] = sch
	}

	v.eventFallback, err = compiler.Compile(schemaBaseURL + "/" + eventFallbackSchemaFile)
	if err != nil {
		return nil, fmt.Errorf("compiling schema %s: %w", eventFallbackSchemaFile, err)
	}

	return v, nil
}

// sharedValidator compiles the schema set on first use and returns that same Validator
// to every later caller. NewClient and NewServer use it when the caller supplied none,
// so a process hosting several agents compiles the set once rather than once per
// endpoint. A compile failure is returned to every caller, since the second call
// repeats the first call's answer rather than recompiling.
var sharedValidator = sync.OnceValues(NewValidator)

// SharedValidator returns the Validator this package builds for a caller that supplied
// none, compiling the schema set on the first call and answering with that same one
// afterwards. A Validator holds compiled schemas and no per-message state, so one
// serves every client, server and endpoint in a process.
//
// It is for a caller that validates bodies itself rather than through a Client or a
// Server: the prompts channel and the job worker each check what arrives before
// decoding it, and without this each would compile the set again. A caller wanting a
// Validator of its own calls NewValidator.
func SharedValidator() (*Validator, error) { return sharedValidator() }

// Validate checks a raw message body against the schema for its protocol id. It
// returns ErrUnknownProtocol when the protocol id has no schema.
//
// A property the schema does not name is accepted and, on decode, discarded.
// Passing validation therefore says the fields this build knows about are
// well formed, not that the body carried only those fields.
func (v *Validator) Validate(data []byte) error {
	var probe struct {
		Protocol string `json:"protocol"`
	}

	err := json.Unmarshal(data, &probe)
	if err != nil {
		return err
	}

	sch, ok := v.schemas[probe.Protocol]
	if !ok {
		// An event of a kind this build does not name is framing it can still check.
		// Anything else decides what the message means, so not knowing it is the end
		// of the matter.
		_, isEvent := blockTypeOf(probe.Protocol)
		if !isEvent {
			return fmt.Errorf("%w: %q", ErrUnknownProtocol, probe.Protocol)
		}

		sch = v.eventFallback
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}

	return sch.Validate(inst)
}

// ValidateMessage marshals a message and validates the result against the schema
// for its protocol id.
func (v *Validator) ValidateMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return v.Validate(data)
}
