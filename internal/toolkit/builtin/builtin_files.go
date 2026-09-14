//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// readFileName is the opt-in built-in that reads one regular file under a
// configured root. It is defined in the config package on the same terms as
// base64EncodeName.
const readFileName = config.ReadFileToolName

// defaultReadFileMaxBytes is the read_file size cap when the entry sets none.
const defaultReadFileMaxBytes int64 = 1 << 20

// readFileOptions is the typed shape of the read_file entry's options block.
type readFileOptions struct {
	// Root is the directory every path the model sends resolves under and cannot
	// leave. Empty is cfg.RootDirectory, and the process working directory when that
	// is empty too. A relative Root joins under cfg.RootDirectory when one is set.
	Root string `json:"root"`
	// MaxBytes is the most a file may hold and still be returned. Zero is 1 MiB.
	MaxBytes int64 `json:"max_bytes"`
}

// readFileSpec builds the read_file spec from its harness.tools entry. It shares
// the opt-in constructor signature with base64EncodeSpec. options is the entry's raw
// options block, decoded strictly so a mistyped key such as roots fails at run start
// rather than being ignored; an absent block, null and {} are the zero options.
//
// The root is opened here once and closed again, so a root that is missing or not a
// directory fails before the loop starts; each call then opens and closes the root
// itself, since a tool has no lifecycle hook and agent.Run assembles the tool set per
// run.
func readFileSpec(cfg *config.Config, options json.RawMessage) (functool.Spec, error) {
	opts, err := decodeReadFileOptions(options)
	if err != nil {
		return functool.Spec{}, err
	}
	if opts.MaxBytes < 0 {
		return functool.Spec{}, fmt.Errorf("max_bytes must be zero or positive, got %d", opts.MaxBytes)
	}

	dir, err := readFileRoot(cfg, opts.Root)
	if err != nil {
		return functool.Spec{}, err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return functool.Spec{}, fmt.Errorf("root: %w", err)
	}
	err = root.Close()
	if err != nil {
		return functool.Spec{}, fmt.Errorf("root: %w", err)
	}

	tool := &readFileTool{
		dir:      dir,
		maxBytes: opts.MaxBytes,
	}
	if tool.maxBytes == 0 {
		tool.maxBytes = defaultReadFileMaxBytes
	}

	// The root and the cap stay out of the description: it is part of the tool-set
	// fingerprint a resumed session checks, and a description that changed with the
	// configured directory would fail that check for the same tool.
	return functool.Spec{
		Name: readFileName,
		// Not a2a, for the reason base64_encode is not: there is no a2a builtins
		// allowlist, so declaring it there would serve it the moment a2a is enabled.
		Expose: &functool.ExposeSpec{MCP: true},
		// It reads and never writes; the same path returns the same content while the
		// file is unchanged, and every read stays inside the root.
		Behavior: toolkit.Behavior{
			ReadOnly:   toolkit.HintTrue,
			Idempotent: toolkit.HintTrue,
			OpenWorld:  toolkit.HintFalse,
		},
		Description: "Read one file and return its content. " +
			"Paths are relative to a fixed directory you cannot leave: a leading slash is ignored, so /etc/hosts and etc/hosts name the same file under that directory, and .. cannot leave it. " +
			"It returns {\"path\": \"<as sent>\", \"found\": true, \"size\": <bytes>, \"encoding\": \"utf-8\" or \"base64\", \"content\": \"...\"}. " +
			"found false means there is no file at that path. " +
			"encoding utf-8 carries the text as content; base64 carries content that is not valid UTF-8, such as a binary file, standard base64 encoded. " +
			"It refuses a file larger than a configured size limit, and anything that is not a regular file, such as a directory.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file to read, relative to the fixed directory. A leading slash is ignored.",
				},
			},
			"required": []any{"path"},
		},
		Handler:          withPrompter(tool.handle),
		ValidateRequired: true,
		Trace:            readFileTrace,
	}, nil
}

// decodeReadFileOptions strictly decodes the read_file options block, refusing an
// unknown key the way memory.DecodeOptions does for a memory backend. An empty
// block, null and {} decode to the zero options.
func decodeReadFileOptions(options json.RawMessage) (readFileOptions, error) {
	var opts readFileOptions
	if len(options) == 0 {
		return opts, nil
	}

	dec := json.NewDecoder(bytes.NewReader(options))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		return opts, fmt.Errorf("invalid options: %w", err)
	}

	return opts, nil
}

// readFileRoot resolves the configured root by the rule every relative path in the
// configuration follows: empty is cfg.RootDirectory, and the process working
// directory when that is empty too; a relative root joins under cfg.RootDirectory
// when one is set and stays relative to the working directory otherwise; an absolute
// root is used as written.
func readFileRoot(cfg *config.Config, root string) (string, error) {
	base := ""
	if cfg != nil {
		base = cfg.RootDirectory
	}

	switch {
	case root == "" && base == "":
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("root: %w", err)
		}

		return wd, nil
	case root == "":
		return base, nil
	case base != "" && !filepath.IsAbs(root):
		return filepath.Join(base, root), nil
	default:
		return root, nil
	}
}

// readFileTool is the state one read_file tool holds across calls.
type readFileTool struct {
	// dir is the resolved directory every path opens under. Each call opens it as an
	// os.Root and closes it before returning, so a tool set that is built and dropped
	// per run leaves no descriptor behind.
	dir string
	// maxBytes is the most a file may hold and still be returned.
	maxBytes int64
}

// readFileTrace renders the one-line call trace as the tool name and the path, which
// is also the summary a confirm wrap shows the operator.
func readFileTrace(input json.RawMessage) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(input, &args); err != nil {
		return readFileName
	}

	return fmt.Sprintf("%s %s", readFileName, args.Path)
}

// readFileOutcome is the JSON result the read_file tool returns.
type readFileOutcome struct {
	// Path is the path as the model sent it.
	Path string `json:"path"`
	// Found is false when there is no file at the path, which is a normal result
	// rather than an error; Size is then 0 and Encoding and Content are empty.
	Found bool `json:"found"`
	// Size is the file's size in bytes.
	Size int64 `json:"size"`
	// Encoding is utf-8 when Content is the file's bytes as text, and base64 when
	// the bytes are not valid UTF-8 and Content is their standard base64 encoding.
	Encoding string `json:"encoding"`
	// Content is the file's content in the form Encoding names.
	Content string `json:"content"`
}

// handle is the read_file handler. The path is trimmed of every leading separator
// and, on Windows, its volume name, so /foo/bar, //foo/bar and foo/bar name one file
// under the root; the root then refuses a .. that escapes and a symlink whose target
// is absolute or leaves the root.
//
// It opens with O_NONBLOCK, so a FIFO with no writer returns at once instead of
// blocking the run, and then stats the open handle: anything that is not a regular
// file is refused on the handle itself, so a file swapped for a FIFO or a directory
// after any earlier check is still refused. A regular file ignores O_NONBLOCK, and
// Windows has no FIFOs and ignores the flag. The read stops at the cap plus one
// byte, so the limit holds whatever the size a stat reported.
func (t *readFileTool) handle(_ context.Context, input json.RawMessage, _ toolkit.Prompter) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(input, &args); err != nil {
		return "", fmt.Errorf("invalid %s input: %w", readFileName, err)
	}

	rel := strings.TrimPrefix(args.Path, filepath.VolumeName(args.Path))
	rel = strings.TrimLeftFunc(rel, func(r rune) bool { return r == '/' || r == filepath.Separator })
	if rel == "" {
		return "", fmt.Errorf("%s requires a non-empty path", readFileName)
	}

	root, err := os.OpenRoot(t.dir)
	if err != nil {
		return "", fmt.Errorf("%s root: %w", readFileName, err)
	}
	defer root.Close()

	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return outcomeJSON(readFileName, readFileOutcome{Path: args.Path})
	}
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", readFileName, args.Path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", readFileName, args.Path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %q is not a regular file (%s)", readFileName, args.Path, info.Mode().Type())
	}

	data, err := io.ReadAll(io.LimitReader(f, t.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", readFileName, args.Path, err)
	}
	if int64(len(data)) > t.maxBytes {
		return "", fmt.Errorf("%s %q is %d bytes, larger than the %d byte limit", readFileName, args.Path, info.Size(), t.maxBytes)
	}

	out := readFileOutcome{
		Path:  args.Path,
		Found: true,
		Size:  int64(len(data)),
	}
	if utf8.Valid(data) {
		out.Encoding = "utf-8"
		out.Content = string(data)
	} else {
		out.Encoding = "base64"
		out.Content = base64.StdEncoding.EncodeToString(data)
	}

	return outcomeJSON(readFileName, out)
}
