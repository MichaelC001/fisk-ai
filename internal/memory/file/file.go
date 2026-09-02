//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package file is the file-backed memory backend: one markdown file per key
// under a directory, each carrying its one-line description in YAML frontmatter.
// Importing this package registers the backend under memory.BackendFile, so the
// program links it in by importing it (usually for its side effect). Beyond that
// registration it exports NewFileStore, the constructor a Go caller embedding the
// agent uses to build a built-in store to hand to agent.Options.MemoryStore.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/choria-io/fisk-ai/internal/memory"
)

func init() {
	memory.Register(memory.BackendFile, newStore)
}

const (
	// defaultDirectory is the base directory used when the directory option is
	// unset. The agent identity is appended so two agents run from the same
	// working directory do not share a namespace unless the operator points them
	// at the same explicit directory.
	defaultDirectory = "memory"

	// fileExtension is appended to a key to form its filename, so the on-disk name
	// is the key verbatim and an operator can read the directory directly.
	fileExtension = ".md"

	// tempPattern names the temporary file a write stages before atomically linking
	// or renaming it into place. The leading dot keeps it out of List, which only
	// considers names ending in fileExtension whose stem is a valid key.
	tempPattern = ".memtmp-*"
)

// options is the typed shape of harness.memory.options for the file backend.
type options struct {
	// Directory is where memory files live. It is resolved relative to the
	// working directory when not absolute, and defaults to memory/<identity>.
	Directory string `json:"directory"`
}

// newStore is the memory.Factory for the file backend: it decodes the options
// block, resolves the directory, and opens the store. A construction failure
// (bad options, an unwritable directory) surfaces here at run start.
//
// The directory is the configured one, else the default memory/<identity>. A
// relative result is rebased under env.StoreDir when the caller set one, so runs
// sharing a process place their stores deterministically; an absolute configured
// directory is honored verbatim and ignores StoreDir, and an empty StoreDir keeps
// today's process-working-directory behavior.
func newStore(env memory.RuntimeEnv, identity string, raw json.RawMessage) (memory.Store, error) {
	opts, err := memory.DecodeOptions[options](raw, "file memory")
	if err != nil {
		return nil, err
	}

	dir := opts.Directory
	if dir == "" {
		dir = filepath.Join(defaultDirectory, identity)
	}
	if env.StoreDir != "" && !filepath.IsAbs(dir) {
		dir = filepath.Join(env.StoreDir, dir)
	}

	return NewFileStore(dir)
}

// FileStore is the file-backed memory.Store: one markdown file per key under a
// directory, each carrying its one-line description in YAML frontmatter. Build one
// with NewFileStore.
type FileStore struct {
	dir string
}

// NewFileStore opens the file-backed memory store rooted at dir, creating the directory
// if needed. It is the constructor a Go caller embedding the agent uses to build a
// built-in store to hand to agent.Options.MemoryStore, without importing this package
// for its registration side effect. dir is used verbatim (no identity namespacing, no
// StoreDir rebasing); the caller resolves the path it wants. The directory is private
// since a memory may hold operator notes. The store is safe for concurrent use across
// runs: writes stage in a temp file and link or rename into place atomically, so a
// reader never observes a half-written value and concurrent updates of one key are
// last-write-wins.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating memory directory %q: %w", dir, err)
	}

	return &FileStore{dir: dir}, nil
}

// path returns the filename for key. The key is validated by the caller, so it
// carries no separator and cannot escape dir.
func (s *FileStore) path(key string) string {
	return filepath.Join(s.dir, key+fileExtension)
}

// Read implements memory.Store. It takes no context: the read is one file open on
// local disk.
func (s *FileStore) Read(_ context.Context, key string) (string, string, error) {
	if err := memory.ValidateKey(key); err != nil {
		return "", "", err
	}

	data, err := s.readFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", memory.ErrNotExist
	}
	if err != nil {
		return "", "", err
	}

	description, content := memory.Parse(data)

	return description, content, nil
}

// Create implements memory.Store. The link this writes under fails when the name is
// taken, so two processes creating one key cannot both succeed, and the capacity check
// runs here rather than on Update because only a create adds a key.
func (s *FileStore) Create(_ context.Context, key, description, content string) error {
	data, err := entryValue(key, description, content)
	if err != nil {
		return err
	}

	count, err := s.count()
	if err != nil {
		return err
	}

	err = memory.CheckCapacity(count)
	if err != nil {
		return err
	}

	return s.writeAtomic(s.path(key), data, false)
}

// Update implements memory.Store, and writes a key holding nothing yet. This backend
// enforces no read-before-update, so it never returns memory.ErrStale and the last
// write wins.
func (s *FileStore) Update(_ context.Context, key, description, content string) error {
	data, err := entryValue(key, description, content)
	if err != nil {
		return err
	}

	return s.writeAtomic(s.path(key), data, true)
}

// entryValue validates a write against the shared rules and serializes the bytes
// both verbs store.
func entryValue(key, description, content string) ([]byte, error) {
	description, err := memory.ValidateWrite(key, description, content)
	if err != nil {
		return nil, err
	}

	return memory.Serialize(description, content)
}

// Delete implements memory.Store. Removing an absent key is not an error and reports
// that nothing was removed.
func (s *FileStore) Delete(_ context.Context, key string) (bool, error) {
	if err := memory.ValidateKey(key); err != nil {
		return false, err
	}

	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deleting memory %q: %w", key, err)
	}

	return true, nil
}

// Info reports the backend and, deliberately, no location.
//
// This store's container is its directory, an absolute local filesystem path, and Info
// is exported on a telemetry span. A path there is high cardinality and describes the
// operator's machine rather than the run, which is the same reason the knowledge store's
// data source id must not fall back to its database path. Nothing is lost: the directory
// is in the config and in fisk info, where it is not leaving the process.
func (s *FileStore) Info() memory.Info {
	return memory.Info{Backend: memory.BackendFile}
}

// List implements memory.Store. It reads the directory and then opens each file, so
// its cost grows with the number of memories rather than with their size.
func (s *FileStore) List(_ context.Context) ([]memory.Item, error) {
	names, err := s.keyFiles()
	if err != nil {
		return nil, err
	}

	entries := make([]memory.Item, 0, len(names))
	for _, key := range names {
		data, err := s.readFile(s.path(key))
		if err != nil {
			// A file that races with a concurrent delete, or is unreadable, is
			// skipped rather than failing the whole listing.
			continue
		}
		description, _ := memory.Parse(data)
		entries = append(entries, memory.Item{Key: key, Description: description})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	return entries, nil
}

// keyFiles returns the keys of every valid memory file in the directory, filtering
// out temp files, subdirectories, and any name whose stem is not a valid key.
func (s *FileStore) keyFiles() ([]string, error) {
	dirEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading memory directory %q: %w", s.dir, err)
	}

	var keys []string
	for _, de := range dirEntries {
		if !de.Type().IsRegular() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, fileExtension) {
			continue
		}
		key := strings.TrimSuffix(name, fileExtension)
		if memory.ValidateKey(key) != nil {
			continue
		}
		keys = append(keys, key)
	}

	return keys, nil
}

// count reports how many valid memory files the directory holds, for the
// create-time entry cap.
func (s *FileStore) count() (int, error) {
	keys, err := s.keyFiles()
	if err != nil {
		return 0, err
	}

	return len(keys), nil
}

// readFile reads a memory file without following a symlink and rejecting any
// non-regular file, so a symlink or device planted in the directory cannot make
// the store return the contents of an unrelated file to the model.
//
// The content is read from the descriptor the no-follow open returned, never by
// re-opening the path: the open and the Stat below establish what this descriptor
// is, and a second open by name would resolve the path again and follow whatever
// was swapped in since, which is exactly the substitution the no-follow open is
// there to prevent.
func (s *FileStore) readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("memory file %q is not a regular file", path)
	}

	return io.ReadAll(f)
}

// writeAtomic stages data in a temp file in the same directory and moves it into
// place so a concurrent reader never observes a half-written file. Create
// (overwrite false) uses a hard link, which fails if the name already exists,
// giving the create-guard atomically; replace (overwrite true) uses rename,
// which atomically supersedes any existing value.
func (s *FileStore) writeAtomic(path string, data []byte, overwrite bool) error {
	tmp, err := os.CreateTemp(s.dir, tempPattern)
	if err != nil {
		return fmt.Errorf("staging memory write: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing memory value: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing memory value: %w", err)
	}

	if overwrite {
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("replacing memory value: %w", err)
		}
		return nil
	}

	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return memory.ErrExists
		}
		return fmt.Errorf("creating memory value: %w", err)
	}

	return nil
}
