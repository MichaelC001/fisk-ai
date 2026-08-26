//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/choria-io/asyncjobs"
	"github.com/choria-io/fisk"
	"github.com/nats-io/jsm.go"
	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/a2aendpoint"
)

const (
	testQueue    = "TESTQ"
	testTaskType = "fisk-ai:run"
)

// runJetStream starts an embedded JetStream-enabled NATS server and returns a
// connection built the way the asyncjobs client requires. Both are torn down with the
// spec.
func runJetStream() *nats.Conn {
	GinkgoHelper()

	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: GinkgoT().TempDir()})
	Expect(err).ToNot(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL(), nats.UseOldRequestStyle())
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(nc.Close)

	return nc
}

// newQueue creates the work queue the channel later binds to, and returns a client a
// spec submits and inspects with. The channel never creates a queue: its run time and
// try limit are the operator's, which here is this function, so it is called once per
// spec with the settings that spec needs.
func newQueue(nc *nats.Conn, maxRunTime time.Duration, maxTries int) *asyncjobs.Client {
	GinkgoHelper()

	client, err := asyncjobs.NewClient(
		asyncjobs.NatsConn(nc),
		asyncjobs.WorkQueue(&asyncjobs.Queue{
			Name:          testQueue,
			MaxRunTime:    maxRunTime,
			MaxTries:      maxTries,
			MaxConcurrent: 10,
		}),
	)
	Expect(err).ToNot(HaveOccurred())

	return client
}

// enqueue submits a job under a chosen id. The payload is handed over as a
// json.RawMessage so it lands as the request object rather than a string wrapping it.
func enqueue(client *asyncjobs.Client, id string, payload []byte) {
	GinkgoHelper()

	task, err := asyncjobs.NewTask(testTaskType, json.RawMessage(payload))
	Expect(err).ToNot(HaveOccurred())

	if id != "" {
		task.ID = id
	}

	Expect(client.EnqueueTask(context.Background(), task)).To(Succeed())
}

func loadTask(client *asyncjobs.Client, id string) *asyncjobs.Task {
	GinkgoHelper()

	task, err := client.LoadTaskByID(id)
	Expect(err).ToNot(HaveOccurred())

	return task
}

func taskState(client *asyncjobs.Client, id string) func() asyncjobs.TaskState {
	return func() asyncjobs.TaskState {
		task, err := client.LoadTaskByID(id)
		if err != nil {
			return asyncjobs.TaskStateUnknown
		}

		return task.State
	}
}

// answerOf decodes what the worker stored on the task, independently of ParseAnswer.
//
// The specs below assert what the worker produced, so they must not read it through the
// helper a caller would use: a bug there that happened to mirror one in the worker would
// cancel out and leave them passing. The round trip that does exercise ParseAnswer is
// the one spec whose subject it is.
func answerOf(task *asyncjobs.Task) any {
	GinkgoHelper()

	Expect(task.Result).ToNot(BeNil())

	raw, err := json.Marshal(task.Result.Payload)
	Expect(err).ToNot(HaveOccurred())

	msg, err := a2a.Decode(raw)
	Expect(err).ToNot(HaveOccurred())

	return msg
}

func testApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing")
	do.Arg("subject", "the subject").Required().String()

	return app
}

// suspendedSession journals a run that stops part way, which is what a worker killed
// mid-job leaves behind for the delivery that follows it.
func suspendedSession(store runstate.Store, id string) {
	GinkgoHelper()

	polls := 0
	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), testApp())),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider: agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
		Checkpoint:       agent.Checkpoint{ResumeID: id, CreateIfMissing: true},
		SessionStore:     store,
		SuspendRequested: func() bool { polls++; return polls > 1 },
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

	Expect(err).ToNot(HaveOccurred())
	Expect(res.Reason).To(Equal(runstate.ReasonSuspended))
}

// slowProvider delays every model call, so a run outlives a lease that is not being
// renewed.
type slowProvider struct {
	llm.Provider

	delay time.Duration
}

func (p *slowProvider) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return p.Provider.Call(ctx, req)
}

type workerOpts struct {
	provider   llm.Provider
	store      runstate.Store
	maxPayload int
}

// worker is a channel and the server in front of it, started together and stopped in
// the one order that is safe.
type worker struct {
	ch       *Channel
	cancel   context.CancelFunc
	done     chan struct{}
	serveErr error
}

func startWorker(nc *nats.Conn, opts workerOpts) *worker {
	GinkgoHelper()

	ch, err := New(Options{
		Conn:        nc,
		Queue:       testQueue,
		TaskType:    testTaskType,
		Identity:    "worker",
		Concurrency: 1,
		MaxPayload:  opts.maxPayload,
		Logger:      quietLogger(),
	})
	Expect(err).ToNot(HaveOccurred())

	// Every run this channel serves is checkpointed, so a store is always injected:
	// left to the configuration each spec would journal into the real state directory
	// and the next spec would find the previous one's session under the same id.
	store := opts.store
	if store == nil {
		store = agenttest.NewFakeSessionStore(GinkgoTB())
	}

	srv, err := serve.New(serve.Options{
		Channels:     []serve.Channel{ch},
		Config:       agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), testApp())),
		Concurrency:  ch.Concurrency(),
		Provider:     opts.provider,
		SessionStore: store,
		Logger:       quietLogger(),
	})
	Expect(err).ToNot(HaveOccurred())

	ctx, cancel := context.WithCancel(context.Background())

	w := &worker{ch: ch, cancel: cancel, done: make(chan struct{})}

	go func() {
		w.serveErr = srv.Serve(ctx)
		close(w.done)
	}()

	DeferCleanup(w.stop)

	return w
}

// waitServed blocks until Serve has returned. It is idempotent, so a spec proving a
// shutdown path and the cleanup that follows it can both call it.
func (w *worker) waitServed() {
	GinkgoHelper()

	Eventually(w.done, 30*time.Second).Should(BeClosed())
	Expect(w.serveErr).To(Succeed())
}

// stop takes the documented shutdown order: cancel the server, wait for it, and only
// then close the channel, so no answer is lost to a processor that stopped early.
func (w *worker) stop() {
	GinkgoHelper()

	w.cancel()
	w.waitServed()
	Expect(w.ch.Close()).To(Succeed())
}

var _ = Describe("Integration: asyncjobs channel", Label("integration"), func() {
	var nc *nats.Conn

	BeforeEach(func() {
		nc = runJetStream()
	})

	Describe("Binding", func() {
		It("Should refuse a queue that does not exist", func() {
			// The queue is what must be missing here, so everything under it is
			// provisioned first; without that this passes on the storage error below
			// while claiming to be about the queue.
			newQueue(nc, 30*time.Second, 5)

			_, err := New(Options{
				Conn: nc, Queue: "NOPE", TaskType: testTaskType,
				Identity: "worker", Concurrency: 1, Logger: quietLogger(),
			})
			Expect(err).To(MatchError(ContainSubstring(`connecting to queue "NOPE"`)))
			Expect(err).To(MatchError(asyncjobs.ErrQueueNotFound))
		})

		// The worker creates none of the storage it uses, so an unprovisioned cluster
		// is the operator's first run and has to say what to do about it rather than
		// quietly deciding how every answer is stored.
		It("Should refuse to create the task store", func() {
			_, err := New(Options{
				Conn: nc, Queue: testQueue, TaskType: testTaskType,
				Identity: "worker", Concurrency: 1, Logger: quietLogger(),
			})
			Expect(err).To(MatchError(ContainSubstring("ajc tasks initialize")))

			mgr, err := jsm.New(nc)
			Expect(err).ToNot(HaveOccurred())

			known, err := mgr.IsKnownStream("CHORIA_AJ_TASKS")
			Expect(err).ToNot(HaveOccurred())
			Expect(known).To(BeFalse(), "nothing was created on the way to failing")
		})

		It("Should take its renewal interval from the queue it bound", func() {
			newQueue(nc, 10*time.Second, 5)

			ch, err := New(Options{
				Conn: nc, Queue: testQueue, TaskType: testTaskType,
				Identity: "worker", Concurrency: 3, Logger: quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(ch.Close)

			Expect(ch.Concurrency()).To(Equal(3), "the server is set from this, not the other way around")
			Expect(ch.renewEvery).To(Equal(5*time.Second), "half the queue's own run time")
			Expect(ch.Name()).To(Equal("asyncjobs/" + testQueue))
		})
	})

	Describe("Running a job", func() {
		It("Should store the answer on the task and acknowledge it", func() {
			client := newQueue(nc, 30*time.Second, 5)
			startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("all done")),
			})

			req := newRequest("go")
			enqueue(client, "job1", encode(req))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			task := loadTask(client, "job1")
			Expect(task.Tries).To(Equal(1), "one delivery, no retry")

			res, ok := answerOf(task).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("all done"))
			Expect(res.StopReason).To(Equal(a2a.StopEndTurn))
			Expect(res.Request).To(Equal(req.Request), "the answer correlates to the request that asked")
			Expect(res.Sender.Name).To(Equal("worker"))
		})

		// NewJob and ParseAnswer are the whole of a caller's path, so what is worth proving
		// is the round trip rather than either end alone: the worker accepts a payload
		// nothing here hand-assembled, and the answer it stored reads back.
		It("Should round trip a job built and read with the helpers", func() {
			client := newQueue(nc, 30*time.Second, 5)
			startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("all done")),
			})

			task, err := NewJob(Job{ID: "job1", Prompt: "go", Caller: "caller"})
			Expect(err).ToNot(HaveOccurred())
			Expect(client.EnqueueTask(context.Background(), task)).To(Succeed())

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			res, err := ParseAnswer(loadTask(client, "job1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Text).To(Equal("all done"))
			Expect(res.StopReason).To(Equal(a2a.StopEndTurn))
			Expect(res.Request).To(Equal(requestOf(task).Request), "the answer correlates to the request the helper built")
		})

		// A run that failed is a completed job whose answer is that it failed, so it is
		// acknowledged rather than retried.
		It("Should store the failure of a run that finished badly", func() {
			client := newQueue(nc, 30*time.Second, 5)
			startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			task := loadTask(client, "job1")
			msg, ok := answerOf(task).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue(), "a run that ran out of model responses answers with an error message")
			Expect(msg.Err).ToNot(BeEmpty())
			Expect(task.Tries).To(Equal(1), "a finished run is not retried")
		})
	})

	// The lease is the only thing keeping a job from being delivered to a second worker
	// while the first is still working, and a real run outlives any reasonable AckWait.
	Describe("Lease renewal", func() {
		It("Should hold a job whose run outlives the queue's run time", func() {
			client := newQueue(nc, 2*time.Second, 5)

			startWorker(nc, workerOpts{
				provider: &slowProvider{
					Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("slow but done")),
					delay:    5 * time.Second,
				},
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 40*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			task := loadTask(client, "job1")
			Expect(task.Tries).To(Equal(1), "the lease was renewed, so the job was never delivered twice")

			res, ok := answerOf(task).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("slow but done"))
		})
	})

	Describe("At-least-once delivery", func() {
		// A submitter chooses the task id, so it must not be the journal name. A journal
		// id is not a secret: it is logged and a deferred run's terminal message carries
		// it, and the prompts channel writes its conversations to the same store.
		It("Should not reach a journal the submitter names", func() {
			client := newQueue(nc, 30*time.Second, 5)

			store := agenttest.NewFakeSessionStore(GinkgoTB())

			// A conversation of the prompts channel, named the way that channel names one.
			victim := a2aendpoint.SessionFor("worker", "3Hzmp8VqrKL42NmXcPd7bTgWfR1")
			suspendedSession(store, victim)

			before, err := store.Load(victim)
			Expect(err).ToNot(HaveOccurred())

			startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ran somewhere of its own")),
				store:    store,
			})

			// The submitter spells that conversation's journal id as its task id.
			enqueue(client, victim, encode(newRequest("what did we say earlier")))

			Eventually(taskState(client, victim), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			after, err := store.Load(victim)
			Expect(err).ToNot(HaveOccurred())
			Expect(after.Messages).To(Equal(before.Messages), "the conversation took no turn")

			// The job ran, in a journal of its own.
			mine, err := store.Load(SessionFor("worker", victim))
			Expect(err).ToNot(HaveOccurred())
			Expect(mine.Messages).ToNot(BeEmpty())
		})

		// The crash case: a journal exists because a previous attempt got part way, and
		// the task's own try count says nothing because the worker that died never
		// returned to persist it. The store is the authority, so the job resumes.
		It("Should resume a session a dead attempt left behind rather than restart it", func() {
			client := newQueue(nc, 30*time.Second, 5)

			// The journal is seeded where the task id derives it, not under the task id
			// itself: a job reaches its own session and no other.
			session := SessionFor("worker", "job1")

			store := agenttest.NewFakeSessionStore(GinkgoTB())
			suspendedSession(store, session)

			before, err := store.Load(session)
			Expect(err).ToNot(HaveOccurred())
			Expect(before.Messages).ToNot(BeEmpty())

			startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
				store:    store,
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			res, ok := answerOf(loadTask(client, "job1")).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("finished"))

			// A restart would have replaced the journal; a resume continues it, and
			// announces itself with a claim before it does anything.
			after, err := store.Load(session)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(after.Messages)).To(BeNumerically(">", len(before.Messages)))

			var claims []*runstate.ClaimRecord
			for _, rec := range openRecords(store, session) {
				if rec.Claim != nil {
					claims = append(claims, rec.Claim)
				}
			}
			Expect(claims).To(HaveLen(1))
			Expect(claims[0].By).To(Equal("job1"),
				"the claim names the job, not the process, so one worker's claims are told apart")
		})

		// A worker that finished the run but died before its acknowledgement landed
		// leaves the job claimed with the answer only in the journal. The redelivery
		// must end the cycle by storing that answer, not report the finished work as an
		// error until the try limit runs out.
		It("Should answer a redelivery whose session already completed", func() {
			client := newQueue(nc, 30*time.Second, 5)

			store := agenttest.NewFakeSessionStore(GinkgoTB())

			res, err := agent.Run(context.Background(), agent.Options{
				Config:       agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), testApp())),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"go"},
				Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("paid for already")),
				Checkpoint:   agent.Checkpoint{ResumeID: SessionFor("worker", "job1"), CreateIfMissing: true},
				SessionStore: store,
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

			// An exhausted provider errors on any call, so a second run would fail the
			// job rather than let this pass.
			provider := agenttest.NewScriptedProvider(GinkgoTB())
			startWorker(nc, workerOpts{provider: provider, store: store})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			task := loadTask(client, "job1")
			Expect(task.Tries).To(Equal(1))

			answer, ok := answerOf(task).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(answer.Text).To(Equal("paid for already"), "the stored answer, not a second one")
			Expect(answer.StopReason).To(Equal(a2a.StopEndTurn))
			// What this asserts is that the replayed answer carries the first run's
			// accounting rather than an empty one. How stats become a Usage is the
			// mapping's own business and is covered where it lives.
			Expect(answer.Usage).To(Equal(a2a.UsageFrom(res.Stats)), "the usage is what the work cost the first time")
			Expect(answer.Usage.LLMCalls).To(BeNumerically(">", 0), "the run it is reporting did happen")

			Expect(provider.Requests()).To(BeEmpty(), "the answer was never paid for twice")
		})

		// Another worker holding the run is not a failure of the work, so the job goes
		// back to the queue rather than recording a non-answer as its result. One try is
		// all this queue allows, so the retry it asks for lands as an expiry rather than
		// looping for the length of the spec.
		It("Should return a job whose session another worker holds", func() {
			client := newQueue(nc, 30*time.Second, 1)

			session := SessionFor("worker", "job1")

			store := agenttest.NewFakeSessionStore(GinkgoTB())
			suspendedSession(store, session)

			held, err := store.Open(session)
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(held.Close)

			startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("never reached")),
				store:    store,
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateExpired))

			task := loadTask(client, "job1")
			Expect(task.Result).To(BeNil(), "nothing was answered, so nothing is stored")
			Expect(task.LastErr).To(ContainSubstring("locked"))
		})
	})

	// Nothing intake refuses can succeed on another delivery, so each of these
	// terminates with a readable reason and never reaches a run.
	Describe("Refusing work that can never succeed", func() {
		DescribeTable("Terminating the task",
			func(id string, payload func() []byte, reason string) {
				client := newQueue(nc, 30*time.Second, 5)
				provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("never reached"))
				startWorker(nc, workerOpts{provider: provider, maxPayload: 2048})

				enqueue(client, id, payload())

				Eventually(taskState(client, id), 30*time.Second).Should(Equal(asyncjobs.TaskStateTerminated))

				task := loadTask(client, id)
				Expect(task.LastErr).To(ContainSubstring(reason))
				Expect(task.LastErr).ToNot(ContainSubstring("\n"), "the task carries one line; the log carries the detail")
				Expect(task.Result).To(BeNil())

				Expect(provider.Requests()).To(BeEmpty(), "nothing was ever run")
			},
			Entry("a payload that is not a valid message", "job1",
				func() []byte { return []byte(`{"protocol":"io.choria.fisk-ai.v1.request"}`) },
				"not a valid v1 message"),
			Entry("a valid message that is not a request", "job2",
				func() []byte {
					cancel := a2a.NewCancel()
					stampHeader(&cancel.Header)
					return encode(cancel)
				},
				"is not a io.choria.fisk-ai.v1.request.prompt message"),
			Entry("a prompt with nothing in it", "job4",
				func() []byte { return encode(newRequest("")) },
				"not a valid v1 message"),
			Entry("a request that is not a prompt", "job6",
				func() []byte {
					resume := a2a.NewResume("2Ab3Cd4Ef5Gh")
					stampHeader(&resume.Header)
					return encode(resume)
				},
				"is not a io.choria.fisk-ai.v1.request.prompt message"),
			Entry("a payload over the size cap", "job5",
				func() []byte {
					req := newRequest("go")
					req.Context = string(make([]byte, 4096))
					return encode(req)
				},
				"byte limit"),
		)
	})

	Describe("Shutdown", func() {
		It("Should stop cleanly when the server goes first", func() {
			client := newQueue(nc, 30*time.Second, 5)
			w := startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			// The documented order, and the one the cleanup takes anyway, so calling it
			// here proves the second call is a no-op rather than a hang.
			w.stop()
		})

		// The other order is not a choice a caller makes, it is what an unreachable
		// queue does to them. A puller whose channel can produce nothing more has to be
		// told, or Serve never returns.
		It("Should end the channel when the processor goes first", func() {
			client := newQueue(nc, 30*time.Second, 5)
			w := startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			w.ch.procStop()

			w.waitServed()
		})

		// The drain ordering, which is what a signal handler wants: nothing is canceled,
		// so a run stops where it can be resumed from rather than wherever it had got
		// to, and Serve ends on its own once the channel reports it is finished.
		It("Should drain when the channel is closed while the server runs", func() {
			client := newQueue(nc, 30*time.Second, 5)
			w := startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			})

			enqueue(client, "job1", encode(newRequest("go")))

			Eventually(taskState(client, "job1"), 30*time.Second).Should(Equal(asyncjobs.TaskStateCompleted))

			Expect(w.ch.Close()).To(Succeed())

			w.waitServed()
		})

		// An idle worker has nothing to wait for, so the same call is what makes a first
		// interrupt exit rather than sit there having done nothing visible.
		It("Should drain at once when nothing is in flight", func() {
			newQueue(nc, 30*time.Second, 5)
			w := startWorker(nc, workerOpts{
				provider: agenttest.NewScriptedProvider(GinkgoTB()),
			})

			// The processor starts on the first Next, so wait for the server to have
			// asked once; closing before that is the never-pulled case below.
			Eventually(w.ch.running.Load, 10*time.Second).Should(BeTrue())

			Expect(w.ch.Close()).To(Succeed())

			w.waitServed()
		})

		It("Should be safe to close a channel the server never pulled from", func() {
			newQueue(nc, 30*time.Second, 5)

			ch, err := New(Options{
				Conn: nc, Queue: testQueue, TaskType: testTaskType,
				Identity: "worker", Concurrency: 1, Logger: quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			Expect(ch.Close()).To(Succeed())
			Expect(ch.Close()).To(Succeed())
		})
	})
})

// openRecords reads a session's records, which needs the journal rather than the
// folded state the store hands out for inspection.
func openRecords(store runstate.Store, id string) []runstate.Record {
	GinkgoHelper()

	j, err := store.Open(id)
	Expect(err).ToNot(HaveOccurred())
	defer func() { Expect(j.Close()).To(Succeed()) }()

	recs, err := j.Records()
	Expect(err).ToNot(HaveOccurred())

	return recs
}
