//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"reflect"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// closedSchemas compiles a copy of the embedded schemas with every object closed, so a
// property a Go type marshals that no schema names is refused.
//
// The shipped schemas stay open, and must: a receiver ignores properties it does not
// recognize, which is what lets a peer built against an older copy read a message from
// one that has gained a field. Closing them on the wire would refuse that message. This
// closes them here, where the sender is this build and every property it writes is one
// the schemas are supposed to name.
//
// unevaluatedProperties rather than additionalProperties, because every message schema
// pulls the header in through allOf. additionalProperties sees only the properties
// declared beside it, so it would refuse every header field; unevaluatedProperties
// counts what allOf and $ref already matched.
//
// A subschema composed into another through allOf is left open. Evaluated on its own it
// sees only its own properties, so closing it would refuse the properties its parent
// declares. The parent's own closure covers it: a header field the schema does not name
// is unevaluated at the message root, which is where it is caught.
func closedSchemas() map[string]*jsonschema.Schema {
	GinkgoHelper()

	docs := map[string]any{}
	composed := map[string]bool{}

	entries, err := fs.ReadDir(schemaFS, schemaDir)
	Expect(err).ToNot(HaveOccurred())

	for _, entry := range entries {
		raw, err := schemaFS.ReadFile(schemaDir + "/" + entry.Name())
		Expect(err).ToNot(HaveOccurred())

		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		Expect(err).ToNot(HaveOccurred())

		obj, ok := doc.(map[string]any)
		Expect(ok).To(BeTrue(), entry.Name())

		collectComposed(obj, composed)
		docs[entry.Name()] = obj
	}

	compiler := jsonschema.NewCompiler()

	for name, doc := range docs {
		obj := doc.(map[string]any)
		closeObjects(obj, composed, "")

		id, ok := obj["$id"].(string)
		Expect(ok).To(BeTrue(), name)
		Expect(compiler.AddResource(id, obj)).To(Succeed(), name)
	}

	out := make(map[string]*jsonschema.Schema, len(protocolSchemaFile))
	for protocol, file := range protocolSchemaFile {
		sch, err := compiler.Compile(schemaBaseURL + "/" + file)
		Expect(err).ToNot(HaveOccurred(), file)

		out[protocol] = sch
	}

	return out
}

// collectComposed records the $defs names reached through an allOf, which are the
// subschemas that stay open.
func collectComposed(node any, into map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		all, ok := n["allOf"].([]any)
		if ok {
			for _, member := range all {
				sub, ok := member.(map[string]any)
				if !ok {
					continue
				}

				ref, ok := sub["$ref"].(string)
				if !ok {
					continue
				}

				_, name, found := strings.Cut(ref, "#/$defs/")
				if found {
					into[name] = true
				}
			}
		}

		for _, v := range n {
			collectComposed(v, into)
		}
	case []any:
		for _, v := range n {
			collectComposed(v, into)
		}
	}
}

// closeObjects adds unevaluatedProperties: false to every object that declares
// properties, skipping the $defs entries other schemas compose in. def is the name of
// the $defs entry this node sits under, empty everywhere else.
func closeObjects(node any, composed map[string]bool, def string) {
	switch n := node.(type) {
	case map[string]any:
		_, hasProperties := n["properties"]
		if hasProperties && !composed[def] {
			n["unevaluatedProperties"] = false
		}

		for key, v := range n {
			if key == "$defs" {
				defs, ok := v.(map[string]any)
				if ok {
					for name, sub := range defs {
						closeObjects(sub, composed, name)
					}

					continue
				}
			}

			closeObjects(v, composed, def)
		}
	case []any:
		for _, v := range n {
			closeObjects(v, composed, def)
		}
	}
}

// jsonTags is every property name a struct marshals, following embedded structs the way
// encoding/json does and skipping the fields tagged out of the document.
func jsonTags(t reflect.Type) []string {
	var out []string

	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")

		if f.Anonymous && tag == "" {
			out = append(out, jsonTags(f.Type)...)
			continue
		}

		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}

		out = append(out, name)
	}

	return out
}

// The schemas are what a peer writes its decoder against, so a field this build
// marshals that no schema names is a change to the wire format nobody announced. These
// specs are what turns schemas/v1 into a promise: a field added to a message type, or a
// JSON tag renamed under one, fails here.
var _ = Describe("Schema drift", func() {
	It("Should name every property every message type marshals", func() {
		closed := closedSchemas()

		for _, msg := range everyMessage() {
			data, err := json.Marshal(msg)
			Expect(err).ToNot(HaveOccurred(), "%T", msg)

			var probe struct {
				Protocol string `json:"protocol"`
			}
			Expect(json.Unmarshal(data, &probe)).To(Succeed())

			sch, ok := closed[probe.Protocol]
			Expect(ok).To(BeTrue(), "%q has no schema", probe.Protocol)

			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			Expect(err).ToNot(HaveOccurred())

			Expect(sch.Validate(inst)).To(Succeed(), "%T carries a property %s does not name", msg, protocolSchemaFile[probe.Protocol])
		}
	})

	// The spec above reads a marshaled sample, and a field left at its Go zero is
	// omitted from the document, so renaming its tag would change nothing the schema
	// sees. This is what closes that: every property a message type can write must be
	// written by one of its samples.
	//
	// The union across a type's samples rather than each one on its own, because the
	// four request kinds are one Go type and each schema refuses the fields belonging
	// to its siblings: an answer carries no prompt, and a prompt carries no replay.
	It("Should write every property of every message type in at least one sample", func() {
		written := map[reflect.Type]map[string]bool{}

		for _, msg := range everyMessage() {
			t := reflect.TypeOf(msg).Elem()
			if written[t] == nil {
				written[t] = map[string]bool{}
			}

			data, err := json.Marshal(msg)
			Expect(err).ToNot(HaveOccurred(), "%T", msg)

			var doc map[string]json.RawMessage
			Expect(json.Unmarshal(data, &doc)).To(Succeed())

			for key := range doc {
				written[t][key] = true
			}
		}

		var missing []string
		for t, keys := range written {
			for _, tag := range jsonTags(t) {
				if !keys[tag] {
					missing = append(missing, t.Name()+"."+tag)
				}
			}
		}

		slices.Sort(missing)
		Expect(missing).To(BeEmpty(), "no sample writes these, so renaming their tags would break nothing here")
	})

	// A sample is what the spec above validates, so an id with none is an id nothing
	// checks. This is what makes a new message type arrive with a sample rather than
	// silently going unchecked.
	It("Should carry a sample of every protocol id the schemas name", func() {
		var sampled []string
		for _, msg := range everyMessage() {
			data, err := json.Marshal(msg)
			Expect(err).ToNot(HaveOccurred())

			var probe struct {
				Protocol string `json:"protocol"`
			}
			Expect(json.Unmarshal(data, &probe)).To(Succeed())

			sampled = append(sampled, probe.Protocol)
		}

		for protocol := range protocolSchemaFile {
			Expect(slices.Contains(sampled, protocol)).To(BeTrue(), "no sample carries %q", protocol)
		}
	})
})
