//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/choria-io/asyncjobs"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// Job is one unit of work to put on a queue a fisk-ai worker consumes.
//
// A caller submits through the engine's own client and no code of ours is in that
// path, so a caller assembles a v1 request by hand and the framing a one-shot job does
// not care about is still required of it. What NewJob is for is the three mistakes that
// otherwise fail at the worker, on another machine, as one line in the task's LastErr.
type Job struct {
	// Prompt is what the agent is asked to do. Required.
	Prompt string

	// Context is optional supporting material, offered to the model alongside the
	// prompt.
	Context string

	// Caller names whoever is submitting. Required, and it must match what the v1
	// schema allows an identity to be: letters, digits, '-' and '_'.
	//
	// Nothing verifies it. The worker logs it and records it as the caller, so it is a
	// label for a person reading later rather than a claim anything acts on.
	Caller string

	// ID is the task id. Empty mints one.
	//
	// It also names the session the worker journals under, so it has to satisfy the
	// session store's rule as well as the queue's, and the store's is the stricter.
	// Setting it is how a caller makes a submission idempotent: a redelivery resumes
	// that journal rather than paying for the model calls a previous attempt made.
	ID string

	// Conversation groups several requests together. Empty uses the request id, which
	// is what a one-shot job wants: the schema requires the field and a job with one
	// turn has nothing to group.
	//
	// The worker drops it rather than passing it on, so it never reaches the journal and
	// no caller can name another's session with it.
	Conversation string

	// Budget lowers the limits the run executes under, and may only lower them: the
	// worker clamps whatever arrives against its own configuration, which stays the
	// ceiling. Nil leaves them alone.
	Budget *wire.Budget

	// TaskType is the asyncjobs task type the worker handles. Empty uses
	// config.DefaultJobsTaskType, which is what a worker with no task_type configured
	// consumes.
	TaskType string
}

// NewJob builds the queue task for a job, carrying a v1 request as its payload.
//
// It does not enqueue. The queue, the client and the retry policy are the caller's, and
// opts are the engine's own task options passed through untouched; submit the task it
// returns with Client.EnqueueTask.
func NewJob(job Job, opts ...asyncjobs.TaskOpt) (*asyncjobs.Task, error) {
	if job.Prompt == "" {
		return nil, fmt.Errorf("a job needs a prompt")
	}
	if job.Caller == "" {
		return nil, fmt.Errorf("a job needs a caller name")
	}
	if !wire.ValidIdentityName(job.Caller) {
		return nil, fmt.Errorf("the caller name %q is not valid (use letters, digits, '-' or '_')", job.Caller)
	}

	taskType := job.TaskType
	if taskType == "" {
		taskType = config.DefaultJobsTaskType
	}

	req := wire.NewRequest(job.Prompt)
	req.Context = job.Context
	req.Budget = job.Budget
	req.Conversation = job.Conversation
	a2a.StampRequest(context.Background(), &req.Header, job.Caller, "")

	// A one-shot job is one message, so the turn and the message carry the same id, and a
	// job with nothing to group groups with itself.
	req.Request = req.ID
	if job.Conversation == "" {
		req.Conversation = req.ID
	}

	// This binding carries no event stream, so the request says so rather than leaving
	// the field at its default of asking for one the worker cannot send.
	stream := false
	req.Stream = &stream

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding the request: %w", err)
	}

	task, err := asyncjobs.NewTask(taskType, json.RawMessage(payload), opts...)
	if err != nil {
		return nil, err
	}

	if job.ID != "" {
		task.ID = job.ID
	}

	// Checked after the options rather than before, so an id one of them set is caught
	// too. The engine's own name rule allows a leading dash, a colon and any length
	// where the session store's does not, and the worker refuses such a job on its first
	// delivery having run nothing.
	err = runstate.ValidateID(task.ID)
	if err != nil {
		return nil, fmt.Errorf("the job id cannot name a session: %w", err)
	}

	return task, nil
}

// ParseAnswer decodes what a worker stored on a task. It unwraps the task and leaves
// what the message means to wire.DecodeTerminal, so a failed run comes back as the stored
// *wire.ErrorMessage and errors.As reaches its stop reason and code.
//
// Load the task with the engine's Client.LoadTaskByID, and wait for its
// TaskStateCompleted.
func ParseAnswer(task *asyncjobs.Task) (*wire.Result, error) {
	if task == nil {
		return nil, fmt.Errorf("there is no task to read")
	}
	if task.Result == nil {
		return nil, fmt.Errorf("the task carries no answer, its state is %q", task.State)
	}

	// The payload is an any that arrived as JSON, so reading it means encoding it again:
	// what the worker stored is a v1 message rather than a Go value of ours.
	raw, err := json.Marshal(task.Result.Payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the stored answer: %w", err)
	}

	return wire.DecodeTerminal(raw)
}
