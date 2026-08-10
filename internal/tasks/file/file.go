//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package file stores task records as one JSON file per task in a directory.
//
// It is the right store for a single-process deployment and for tests, and it is
// honest about what it is: records on one machine's disk. A submitter and a worker on
// different machines need a store both can reach, which is a stream backend rather
// than this one.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/choria-io/fisk-ai/internal/tasks"
)

// BackendName is the name this backend registers under.
const BackendName = "file"

const (
	// defaultDir is the directory records land in when none is configured, rebased
	// under RuntimeEnv.StoreDir when that is set.
	defaultDir = "tasks"

	// tempPattern names the file a write stages into before it is atomically moved
	// into place. The leading dot keeps it out of a record listing.
	tempPattern = ".tasktmp-*"

	// dirMode and fileMode keep records readable only by the owner: a request carries
	// whatever a caller asked for and an answer carries whatever the agent found.
	dirMode  = 0o700
	fileMode = 0o600
)

func init() {
	tasks.Register(BackendName, newStore)
}

type options struct {
	// Directory is where records are kept. A relative path is rebased under
	// RuntimeEnv.StoreDir when that is set.
	Directory string `json:"directory"`
}

func newStore(env tasks.RuntimeEnv, raw json.RawMessage) (tasks.Store, error) {
	opts, err := tasks.DecodeOptions[options](raw, "file task store")
	if err != nil {
		return nil, err
	}

	dir := opts.Directory
	if dir == "" {
		dir = defaultDir
	}
	if !filepath.IsAbs(dir) && env.StoreDir != "" {
		dir = filepath.Join(env.StoreDir, dir)
	}

	return NewStore(dir)
}

// Store keeps one JSON record per task in a directory.
type Store struct {
	dir string
}

// NewStore creates the directory if needed and returns a store over it. A failure to
// create it is reported here rather than at the first write.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("a task directory is required")
	}

	err := os.MkdirAll(dir, dirMode)
	if err != nil {
		return nil, fmt.Errorf("creating the task directory: %w", err)
	}

	return &Store{dir: dir}, nil
}

// Info reports the backend name and no location: a filesystem path must never leave
// the process through Info.
func (s *Store) Info() tasks.Info {
	return tasks.Info{Backend: BackendName}
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Submit writes a new record, taking its id from the request.
func (s *Store) Submit(_ context.Context, request json.RawMessage) (*tasks.Task, error) {
	id, err := tasks.RequestID(request)
	if err != nil {
		return nil, err
	}

	task := &tasks.Task{
		ID:        id,
		State:     tasks.StateSubmitted,
		Request:   request,
		Submitted: time.Now().UTC(),
	}

	body, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("encoding the task record: %w", err)
	}

	err = s.write(s.path(id), body, false)
	if err != nil {
		return nil, err
	}

	return task, nil
}

// Load returns a record by id.
func (s *Store) Load(_ context.Context, id string) (*tasks.Task, error) {
	err := tasks.ValidateID(id)
	if err != nil {
		return nil, err
	}

	return s.read(id)
}

// Complete attaches an answer, refusing to replace one that is already there.
func (s *Store) Complete(_ context.Context, id string, result json.RawMessage) error {
	err := tasks.ValidateID(id)
	if err != nil {
		return err
	}

	task, err := s.read(id)
	if err != nil {
		return err
	}

	if task.State == tasks.StateCompleted {
		return fmt.Errorf("%w: %q", tasks.ErrCompleted, id)
	}

	task.State = tasks.StateCompleted
	task.Result = result
	task.Completed = time.Now().UTC()

	body, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("encoding the task record: %w", err)
	}

	return s.write(s.path(id), body, true)
}

// read loads a record without following a symlink and rejecting any non-regular
// file, so something planted in the directory cannot make the store return an
// unrelated file's contents as a task.
//
// The content is read from the descriptor the no-follow open returned rather than by
// re-opening the path, since a second open by name would resolve the path again and
// follow whatever was swapped in since.
func (s *Store) read(id string) (*tasks.Task, error) {
	f, err := os.OpenFile(s.path(id), os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", tasks.ErrNotFound, id)
		}

		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("task record %q is not a regular file", id)
	}

	body, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	var task tasks.Task
	err = json.Unmarshal(body, &task)
	if err != nil {
		return nil, fmt.Errorf("decoding task record %q: %w", id, err)
	}

	return &task, nil
}

// write stages a record in a temp file in the same directory and moves it into place,
// so a reader never observes a half-written record.
//
// Creating uses a hard link, which fails when the name already exists and so gives
// the exists-guard atomically rather than through a stat that another writer could
// race. Replacing uses rename, which atomically supersedes the previous record. Both
// the record and, on create, the directory are synced, so a crash leaves either the
// old record or the new one rather than neither.
func (s *Store) write(path string, body []byte, replace bool) error {
	tmp, err := os.CreateTemp(s.dir, tempPattern)
	if err != nil {
		return fmt.Errorf("staging the task record: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	err = writeAndSync(tmp, body)
	if err != nil {
		return fmt.Errorf("writing the task record: %w", err)
	}

	err = os.Chmod(tmpName, fileMode)
	if err != nil {
		return fmt.Errorf("writing the task record: %w", err)
	}

	if replace {
		err = os.Rename(tmpName, path)
		if err != nil {
			return fmt.Errorf("replacing the task record: %w", err)
		}

		return nil
	}

	err = os.Link(tmpName, path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %q", tasks.ErrExists, filepath.Base(path))
		}

		return fmt.Errorf("creating the task record: %w", err)
	}

	return s.syncDir()
}

func writeAndSync(f *os.File, body []byte) error {
	_, err := f.Write(body)
	if err != nil {
		f.Close()

		return err
	}

	err = f.Sync()
	if err != nil {
		f.Close()

		return err
	}

	return f.Close()
}

// syncDir makes a newly linked record survive a crash: the file's own contents are
// synced before the link, but the directory entry naming it is not durable until the
// directory itself is.
func (s *Store) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()

	return d.Sync()
}
