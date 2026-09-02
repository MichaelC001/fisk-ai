//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package jetstream is the NATS JetStream KV memory backend: each memory is a
// value in a pre-existing KV bucket, carrying its one-line description in the same
// YAML frontmatter the file backend uses so a value migrates between backends
// unchanged. Importing this package registers the backend under
// memory.BackendJetStream, so a program links it in by importing it (usually for
// its side effect). Beyond that registration it exports NewKVStore, the constructor
// a Go caller embedding the agent uses to build a store over its own NATS
// connection to hand to agent.Options.MemoryStore.
package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/choria-io/fisk-ai/internal/memory"
)

func init() {
	memory.Register(memory.BackendJetStream, newStore, memory.RequiresNats())
}

const (
	// bindTimeout bounds binding to the bucket and reading its status at
	// construction, so a wrong bucket name or an unreachable JetStream surfaces at
	// run start rather than hanging.
	bindTimeout = 10 * time.Second

	// prefixSeparator joins the key prefix and the key as "<prefix>.<key>". Dot is a
	// legal memory key character, so a prefixed key stays a legal memory key and a
	// legal NATS KV key.
	prefixSeparator = "."

	// allKeys is the KV subject wildcard matching every key. Joined with the prefix
	// it watches only this store's keyspace; alone it watches the whole bucket.
	allKeys = ">"
)

// options is the typed shape of harness.memory.options for the jetstream backend.
type options struct {
	// Bucket is the KV bucket memories are stored in. It is required and must
	// already exist: the backend binds to it and never creates it, so the operator
	// owns the bucket's durability policy.
	Bucket string `json:"bucket"`

	// Prefix namespaces this agent's keys within the bucket, stored as
	// "<prefix>.<key>". Unset (nil) defaults to the agent identity, mirroring the
	// file backend's memory/<identity> directory so two agents do not share a
	// keyspace by accident. Set it to a shared value for agents that deliberately
	// share memory, or to "" for a flat, unprefixed keyspace. It is a pointer so an
	// omitted prefix (default to identity) is distinct from an explicit empty one.
	Prefix *string `json:"prefix,omitempty"`

	// NoRequireReadBeforeUpdate turns off the read-before-update guard. By default
	// an overwrite must follow a read of the current value in this conversation and
	// fails with ErrStale otherwise, so a run cannot clobber a memory it has not seen or
	// that changed under it (real lost-update protection over a shared bucket, which
	// the KV revision makes atomic). Set this to allow a blind overwrite, matching
	// the file backend. Like no_index it is a negative switch.
	NoRequireReadBeforeUpdate bool `json:"no_require_read_before_update,omitempty"`
}

// newStore is the memory.Factory for the jetstream backend: it decodes the
// options, resolves the key prefix, binds to the existing bucket over the borrowed
// NATS connection, and validates that the bucket cannot silently lose memories. A
// construction failure surfaces here at run start rather than on the first tool
// call.
func newStore(env memory.RuntimeEnv, identity string, raw json.RawMessage) (memory.Store, error) {
	opts, err := memory.DecodeOptions[options](raw, "jetstream memory")
	if err != nil {
		return nil, err
	}
	if opts.Bucket == "" {
		return nil, fmt.Errorf("jetstream memory: options.bucket is required (the KV bucket name); the bucket must already exist")
	}

	prefix := identity
	if opts.Prefix != nil {
		prefix = *opts.Prefix
	}
	if prefix != "" {
		err := memory.ValidateKey(prefix)
		if err != nil && opts.Prefix == nil {
			return nil, fmt.Errorf("jetstream memory: the agent identity %q cannot be used as a key prefix (%w); set options.prefix to a legal value, or \"\" for a flat keyspace", identity, err)
		}
		if err != nil {
			return nil, fmt.Errorf("jetstream memory: options.prefix %q is invalid: %w", prefix, err)
		}
	}

	nc := env.Nats
	if nc == nil {
		return nil, fmt.Errorf("jetstream memory: requires a NATS connection but none is configured; set nats_context in the config to a context created with `nats context add`")
	}

	ctx, cancel := context.WithTimeout(context.Background(), bindTimeout)
	defer cancel()

	return NewKVStore(ctx, nc, opts.Bucket, KVStoreOptions{
		Prefix:              prefix,
		AllowBlindOverwrite: opts.NoRequireReadBeforeUpdate,
	})
}

// KVStoreOptions are the choices a caller building the store directly makes. The zero
// value namespaces nothing and enforces read-before-update, matching an unconfigured
// jetstream backend.
type KVStoreOptions struct {
	// Prefix namespaces this store's keys within the bucket, stored as
	// "<prefix>.<key>", so two agents sharing a bucket do not share a keyspace. An
	// empty prefix is a flat keyspace, shared with every other writer of the bucket.
	Prefix string

	// AllowBlindOverwrite lets Update replace a value the caller has not read. By
	// default an Update must follow a Read of the current value in the same
	// memory.Scope and fails with memory.ErrStale otherwise, so one writer cannot
	// clobber another's change over a shared bucket.
	AllowBlindOverwrite bool
}

// NewKVStore binds the memory store to an existing KV bucket over nc, which the
// store borrows and never closes. It is the constructor a Go caller embedding the
// agent uses to build a store to hand to agent.Options.MemoryStore, without
// importing this package for its registration side effect.
//
// The bucket must already exist, so the operator owns its durability policy, and it
// is rejected when it would silently lose memories: a TTL expires stored entries and
// a max value size below memory.MaxEntryBytes truncates a full-size one. ctx limits
// the bind and the status read, not the life of the store.
func NewKVStore(ctx context.Context, nc *nats.Conn, bucket string, opts KVStoreOptions) (*KVStore, error) {
	if bucket == "" {
		return nil, fmt.Errorf("jetstream memory: a KV bucket name is required; the bucket must already exist")
	}

	if opts.Prefix != "" {
		err := memory.ValidateKey(opts.Prefix)
		if err != nil {
			return nil, fmt.Errorf("jetstream memory: the key prefix %q is invalid: %w", opts.Prefix, err)
		}
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream memory: %w", err)
	}

	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, fmt.Errorf("jetstream memory: KV bucket %q does not exist; create it first, e.g. `nats kv add %s --history=1 --max-value-size=%d`", bucket, bucket, memory.MaxEntryBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("jetstream memory: binding KV bucket %q: %w", bucket, err)
	}

	err = checkBucketConfig(ctx, kv, bucket)
	if err != nil {
		return nil, err
	}

	return &KVStore{
		kv:          kv,
		bucket:      bucket,
		prefix:      opts.Prefix,
		requireRead: !opts.AllowBlindOverwrite,
		own:         memory.NewScope(),
	}, nil
}

// checkBucketConfig rejects a bucket that would silently lose memories. Memory is a
// durable store the model reasons over, so a TTL that expires entries or a max
// value size below the entry limit is a construction failure, not a degraded run:
// it fails here at run start just like a missing bucket.
func checkBucketConfig(ctx context.Context, kv jetstream.KeyValue, bucket string) error {
	status, err := kv.Status(ctx)
	if err != nil {
		return fmt.Errorf("jetstream memory: reading status of KV bucket %q: %w", bucket, err)
	}

	if ttl := status.TTL(); ttl != 0 {
		return fmt.Errorf("jetstream memory: KV bucket %q has a %s TTL set; stored memories would silently expire. Recreate the bucket without a TTL for durable memory", bucket, ttl)
	}

	// A non-positive max value size means no cap: NATS maps it to the backing
	// stream's MaxMsgSize, whose unset default is -1 (unlimited). Only a genuine
	// positive cap below the entry limit would truncate a write, so reject just that.
	// The bound is MaxEntryBytes, not MaxContentBytes: the stored value is the
	// serialized entry (the body plus its frontmatter header), so a bucket sized to
	// the body cap alone would reject a full-size memory at write time.
	if maxValue := status.Config().MaxValueSize; maxValue > 0 && int64(maxValue) < memory.MaxEntryBytes {
		return fmt.Errorf("jetstream memory: KV bucket %q max value size is %d bytes, below the %d byte memory entry limit; large memories would fail to write. Recreate the bucket with --max-value-size=%d", bucket, maxValue, memory.MaxEntryBytes, memory.MaxEntryBytes)
	}

	return nil
}

// KVStore is the JetStream KV-backed memory.Store. It binds a pre-existing bucket
// and never closes the borrowed NATS connection behind it: the connection belongs to
// whoever opened it. Build one with NewKVStore.
type KVStore struct {
	kv          jetstream.KeyValue
	bucket      string
	prefix      string
	requireRead bool

	// own records the KV revision each key was last seen at through Read or a
	// successful write, so an overwrite can be gated on the model having seen the
	// current value (read-before-update). It is used only for a caller that supplies no
	// memory.Scope on the context, which is a store built per run: then the store and
	// the run are the same thing and it means what it says. A host sharing one store
	// across runs supplies a scope per run, and this is never consulted, because one
	// run's read must not authorize another's overwrite of the same key.
	own *memory.Scope

	// countCache holds the entry count for the capacity check, seeded lazily from one
	// keyspace scan and then tracked through the creates and deletes this store sees,
	// so a run that writes many memories does not rescan on every create. countCached
	// guards the seed. Unlike the read-before-update record it is not run state: it
	// approximates how full the bucket is, and a shared store watching every run's
	// writes approximates it better than one watching a single run's.
	countMu     sync.Mutex
	countCache  int
	countCached bool
}

// scope is where this call's read-before-update record lives: the run's own when the
// host supplied one, and the store's otherwise. Resolving it per call rather than at
// construction is what lets one store serve runs that each have their own.
func (s *KVStore) scope(ctx context.Context) *memory.Scope {
	scope := memory.ScopeFrom(ctx)
	if scope != nil {
		return scope
	}

	return s.own
}

// remember records the revision a key is now known to be at, granting overwrite
// authority for it for the life of the scope. forget drops that authority.
func (s *KVStore) remember(ctx context.Context, key string, rev uint64) {
	s.scope(ctx).Remember(key, rev)
}

func (s *KVStore) forget(ctx context.Context, key string) {
	s.scope(ctx).Forget(key)
}

// knownRevision returns the revision key was last seen at in this scope and whether
// it was seen at all.
func (s *KVStore) knownRevision(ctx context.Context, key string) (uint64, bool) {
	return s.scope(ctx).Revision(key)
}

// storageKey maps a memory key to the bucket key, applying the namespace prefix.
// The caller validates key first, so the result is always a legal KV key.
func (s *KVStore) storageKey(key string) string {
	if s.prefix == "" {
		return key
	}

	return s.prefix + prefixSeparator + key
}

// Read implements memory.Store, and records the revision it saw so a later Update in
// the same scope can prove the model read this value before replacing it.
func (s *KVStore) Read(ctx context.Context, key string) (string, string, error) {
	if err := memory.ValidateKey(key); err != nil {
		return "", "", err
	}

	entry, err := s.kv.Get(ctx, s.storageKey(key))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", "", memory.ErrNotExist
	}
	if err != nil {
		return "", "", fmt.Errorf("reading memory %q: %w", key, err)
	}

	// Record the revision so a later overwrite, on this turn or a later one in the same
	// conversation, can prove the model saw this value.
	// Only Read does this: the start-of-run index and List read values to build the
	// key/description index, not on the model's behalf, so they must not grant
	// overwrite authority (seeing a key in the index is not the same as reading it).
	s.remember(ctx, key, entry.Revision())

	description, content := memory.Parse(entry.Value())

	return description, content, nil
}

// Create implements memory.Store. The KV create is atomic on existence, so two
// processes creating one key cannot both succeed.
func (s *KVStore) Create(ctx context.Context, key, description, content string) error {
	data, err := entryValue(key, description, content)
	if err != nil {
		return err
	}

	sk := s.storageKey(key)

	// The KV create is atomic on existence: it fails with ErrKeyExists for a live key but
	// succeeds for a previously deleted (tombstoned) one, which is exactly the
	// create-guard the contract wants. The capacity check ahead of it is, like the
	// file backend's, best-effort under concurrency.
	count, err := s.currentCount(ctx)
	if err != nil {
		return err
	}

	err = memory.CheckCapacity(count)
	if err != nil {
		return err
	}

	rev, err := s.kv.Create(ctx, sk, data)
	if errors.Is(err, jetstream.ErrKeyExists) {
		return memory.ErrExists
	}
	if err != nil {
		return fmt.Errorf("creating memory %q: %w", key, err)
	}

	// Creating a key grants overwrite authority for it: the model just wrote its
	// value, so it may keep editing it without re-reading. It also adds one to the
	// tracked entry count.
	s.remember(ctx, key, rev)
	s.adjustCount(1)

	return nil
}

// Update replaces a stored memory. With the read-before-update guard on it uses a
// revision-checked KV update, so it only succeeds when the scope knows the key's
// revision and no writer has changed it since; otherwise it is a blind put. Either
// way it records the new revision so the model can keep editing.
func (s *KVStore) Update(ctx context.Context, key, description, content string) error {
	data, err := entryValue(key, description, content)
	if err != nil {
		return err
	}

	return s.overwrite(ctx, key, s.storageKey(key), data)
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

// overwrite writes data over whatever key holds now, on Update's terms.
func (s *KVStore) overwrite(ctx context.Context, key, sk string, data []byte) error {
	if !s.requireRead {
		rev, err := s.kv.Put(ctx, sk, data)
		if err != nil {
			return fmt.Errorf("writing memory %q: %w", key, err)
		}
		s.remember(ctx, key, rev)

		return nil
	}

	prev, ok := s.knownRevision(ctx, key)
	if !ok {
		return fmt.Errorf("%w: no revision is known for memory %q; read it before overwriting", memory.ErrStale, key)
	}

	rev, err := s.kv.Update(ctx, sk, data, prev)
	if isWrongLastSequence(err) {
		// The revision moved: another writer changed the key since it was read. Drop
		// the stale authority so a retry must read the new value first.
		s.forget(ctx, key)
		return fmt.Errorf("%w: memory %q changed since it was read; read it again before overwriting", memory.ErrStale, key)
	}
	if err != nil {
		return fmt.Errorf("writing memory %q: %w", key, err)
	}

	s.remember(ctx, key, rev)

	return nil
}

// isWrongLastSequence reports whether err is JetStream's revision-mismatch error,
// which Update returns when the key's latest revision is not the expected one.
func isWrongLastSequence(err error) bool {
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

// Delete implements memory.Store. Removing an absent key is not an error, and the
// tracked revision goes with the value so a re-created key cannot be overwritten on the
// strength of a read that came before the delete.
func (s *KVStore) Delete(ctx context.Context, key string) (bool, error) {
	if err := memory.ValidateKey(key); err != nil {
		return false, err
	}

	sk := s.storageKey(key)

	// Whether the key existed is best-effort under concurrency: unlike the file
	// backend's single atomic os.Remove, this is a Get then a Delete, so a
	// concurrent deleter can race between them. Delete itself stays idempotent.
	_, err := s.kv.Get(ctx, sk)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deleting memory %q: %w", key, err)
	}

	if err := s.kv.Delete(ctx, sk); err != nil {
		return false, fmt.Errorf("deleting memory %q: %w", key, err)
	}

	// Drop any tracked revision: a later create is the way back, and it records its
	// own. A stale entry left here could authorize an overwrite of a re-created key.
	// The delete also takes one off the tracked entry count.
	s.forget(ctx, key)
	s.adjustCount(-1)

	return true, nil
}

// Info reports the backend and the KV bucket it is bound to. The bucket is an
// operator-configured name, not a path or a credential, so it is safe to export.
// The key prefix is not reported: it is derived from the agent identity, which every
// span already carries.
func (s *KVStore) Info() memory.Info {
	return memory.Info{Backend: memory.BackendJetStream, Location: s.bucket}
}

// List implements memory.Store through one server-side watcher pass filtered to this
// store's prefix. It reads values to build the index and records no revision, so
// listing grants no authority to overwrite what it saw.
func (s *KVStore) List(ctx context.Context) ([]memory.Item, error) {
	items, err := s.snapshot(ctx, false)
	if err != nil {
		return nil, err
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })

	return items, nil
}

// snapshot streams this store's current entries in a single server-side pass. It
// watches only this store's keyspace (the prefix as a subject filter), so on a
// shared bucket it never transfers another agent's keys, and it reads keys and
// values together rather than a Get per key. With metaOnly it skips the values, for
// counting. A watcher replays every current key and then sends a nil entry once the
// initial state is complete, which ends the pass before the live tail; delete
// markers are excluded and duplicates (which a busy bucket can report) are dropped.
func (s *KVStore) snapshot(ctx context.Context, metaOnly bool) ([]memory.Item, error) {
	opts := []jetstream.WatchOpt{jetstream.IgnoreDeletes()}
	if metaOnly {
		opts = append(opts, jetstream.MetaOnly())
	}

	subject := allKeys
	filter := ""
	if s.prefix != "" {
		subject = s.prefix + prefixSeparator + allKeys
		filter = s.prefix + prefixSeparator
	}

	w, err := s.kv.Watch(ctx, subject, opts...)
	if err != nil {
		return nil, fmt.Errorf("listing memory: %w", err)
	}
	defer func() { _ = w.Stop() }()

	seen := map[string]struct{}{}
	var items []memory.Item
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case entry, ok := <-w.Updates():
			if !ok {
				// The watcher closed before the initial-state marker (a dropped
				// connection or a terminated consumer), so what arrived is an
				// incomplete snapshot. Fail rather than pass a short result off as
				// complete: an undercount would bypass the entry cap and a short list
				// would hide stored memories from the model. A clean end always
				// arrives as the nil entry below.
				return nil, fmt.Errorf("listing memory: watch closed before the snapshot completed")
			}
			if entry == nil {
				// All current values delivered; the live tail is not wanted.
				return items, nil
			}

			key := strings.TrimPrefix(entry.Key(), filter)
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}

			description := ""
			if !metaOnly {
				description, _ = memory.Parse(entry.Value())
			}
			items = append(items, memory.Item{Key: key, Description: description})
		}
	}
}

// currentCount returns how many memories this store holds, for the create-time
// entry cap. It seeds a cached count from one keyspace scan the first time it is
// needed and then tracks this run's own creates and deletes (adjustCount), so a run
// that writes many memories scans once rather than on every create. The count is a
// per-run best-effort estimate: a concurrent writer sharing the keyspace is not
// seen, which matches the cap's documented best-effort-under-concurrency semantics
// and still bounds a single runaway run exactly.
func (s *KVStore) currentCount(ctx context.Context) (int, error) {
	s.countMu.Lock()
	cached, c := s.countCached, s.countCache
	s.countMu.Unlock()
	if cached {
		return c, nil
	}

	items, err := s.snapshot(ctx, true)
	if err != nil {
		return 0, err
	}

	s.countMu.Lock()
	if !s.countCached {
		s.countCache = len(items)
		s.countCached = true
	}
	c = s.countCache
	s.countMu.Unlock()

	return c, nil
}

// adjustCount keeps the cached entry count in step with this run's own creates and
// deletes. It is a no-op until currentCount has seeded the cache, so a run that only
// reads or overwrites never scans.
func (s *KVStore) adjustCount(delta int) {
	s.countMu.Lock()
	if s.countCached {
		s.countCache += delta
		if s.countCache < 0 {
			s.countCache = 0
		}
	}
	s.countMu.Unlock()
}
