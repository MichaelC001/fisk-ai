//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// This example is the asynchronous caller the tasks package is for. A submitter writes
// a unit of work down and leaves, a worker somewhere else picks it up and answers it,
// and the submitter comes back for the answer. The four steps hold no connection to
// each other and share only the store on disk and the task id.
//
// It imports tasks, tasks/file and a2a, which is the whole set such a program needs. It
// reaches no network and no broker: the store is a directory of JSON files.
package taskstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/tasks"
	"github.com/choria-io/fisk-ai/internal/tasks/file"
)

// taskID names the turn. The submitter chooses it before it sends anything and writes
// it down, which is how a later process finds the record again.
const taskID = "example-task"

const (
	submitter = "example-submitter"
	worker    = "example-worker"
)

func Example() {
	err := run()
	if err != nil {
		fmt.Println("error:", err)
	}

	// Output:
	// the request validates against the v1 request.prompt schema
	// state after submit: submitted
	// state the worker found: submitted
	// the worker was asked for: prompt
	// prompt: summarize the change log
	// asked by: example-submitter
	// state the submitter came back to: completed
	// stop reason: end_turn
	// answer: the change log has three entries
	// answered by: example-worker
	// answers the request tagged: example-task
}

func run() error {
	dir, err := os.MkdirTemp("", "taskstore")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	steps := []func(string) error{submit, pickUp, answer, collect}

	for _, step := range steps {
		err = step(dir)
		if err != nil {
			return err
		}
	}

	return nil
}

// openStore binds to the records in dir, creating the directory when it is not there
// yet. Every step opens its own, since the steps stand in for separate processes.
func openStore(dir string) (tasks.Store, error) {
	return file.NewStore(dir)
}

// submit writes the work down. The request message is the whole of what the worker will
// be told, so the submitter builds it, validates it, and hands the store the bytes.
func submit(dir string) error {
	store, err := openStore(dir)
	if err != nil {
		return err
	}

	req := a2a.NewRequest("summarize the change log")

	// The constructor minted a request tag. This submitter names its own turns, so it
	// puts the id it will look the record up by in place of the minted one.
	req.Request = taskID

	// The framing the v1 schema requires of every message. A message going out over a
	// transport is stamped by the send; one written to a store is stamped here.
	req.ID = a2a.NewID()
	req.Conversation = a2a.NewID()
	req.Time = time.Now().UTC()
	req.Sender = a2a.Identity{Name: submitter}

	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	// The store reads the protocol id and the request tag to key a record, and stores the
	// body as it arrived. A submitter that wants its own mistake reported before the
	// record exists runs the schemas over the body itself.
	validator, err := a2a.NewValidator()
	if err != nil {
		return err
	}

	err = validator.Validate(body)
	if err != nil {
		return err
	}

	fmt.Println("the request validates against the v1 request.prompt schema")

	task, err := store.Submit(context.Background(), body)
	if err != nil {
		return err
	}

	fmt.Println("state after submit:", task.State)

	return nil
}

// pickUp is the worker taking the task on. It has the id and the store, and everything
// it must run is on the record.
func pickUp(dir string) error {
	store, err := openStore(dir)
	if err != nil {
		return err
	}

	task, err := store.Load(context.Background(), taskID)
	if err != nil {
		return err
	}

	fmt.Println("state the worker found:", task.State)

	req, err := decodeRequest(task.Request)
	if err != nil {
		return err
	}

	fmt.Println("the worker was asked for:", req.Kind)
	fmt.Println("prompt:", req.Prompt)
	fmt.Println("asked by:", req.Sender.Name)

	return nil
}

// answer attaches the result. The worker reads the request again to correlate the reply
// to it: StampReply copies the request and conversation tags across, so a reader can
// match the pair on the record without asking the store.
//
// Complete refuses a second answer, so the first one survives a queue handing the same
// task to two workers.
func answer(dir string) error {
	store, err := openStore(dir)
	if err != nil {
		return err
	}

	ctx := context.Background()

	task, err := store.Load(ctx, taskID)
	if err != nil {
		return err
	}

	req, err := decodeRequest(task.Request)
	if err != nil {
		return err
	}

	res := a2a.NewResult(a2a.StopEndTurn)
	res.Text = "the change log has three entries"
	a2a.StampReply(&res.Header, &req.Header, worker)

	body, err := json.Marshal(res)
	if err != nil {
		return err
	}

	return store.Complete(ctx, taskID, body)
}

// collect is the submitter coming back. It holds no connection to the worker and reads
// the answer off the record.
func collect(dir string) error {
	store, err := openStore(dir)
	if err != nil {
		return err
	}

	task, err := store.Load(context.Background(), taskID)
	if err != nil {
		return err
	}

	fmt.Println("state the submitter came back to:", task.State)

	// A completed task carries a result or an error message. DecodeTerminal returns the
	// failure as an error, since an ErrorMessage is one.
	res, err := a2a.DecodeTerminal(task.Result)

	var failed *a2a.ErrorMessage
	if errors.As(err, &failed) {
		fmt.Println("the run failed:", failed.Err)

		return nil
	}
	if err != nil {
		return err
	}

	fmt.Println("stop reason:", res.StopReason)
	fmt.Println("answer:", res.Text)
	fmt.Println("answered by:", res.Sender.Name)
	fmt.Println("answers the request tagged:", res.Request)

	return nil
}

// decodeRequest reads a stored body back into an a2a.Request. The protocol id says which
// of the four things a caller can ask for it carries, so a worker admits the ids it runs
// and reads the kind off the decoded request.
func decodeRequest(body json.RawMessage) (*a2a.Request, error) {
	msg, err := a2a.ExpectOneProtocol(body, a2a.RequestProtocols())
	if err != nil {
		return nil, err
	}

	req, ok := msg.(*a2a.Request)
	if !ok {
		return nil, fmt.Errorf("%w: the record holds a %T", a2a.ErrProtocolMismatch, msg)
	}

	return req, nil
}
