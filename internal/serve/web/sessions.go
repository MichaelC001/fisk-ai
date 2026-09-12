//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// SessionsPath is the segment the sessions API answers on under the base path, so a
// channel on "/fisk/v1" lists at GET /fisk/v1/sessions and deletes at DELETE
// /fisk/v1/sessions/{id}. A conversation is opened under the format that renders it, at
// GET /fisk/v1/{format}/sessions/{id}. A Format may not be mounted here.
const SessionsPath = "sessions"

// The page limits the list endpoint answers. A console asks for what its rail shows and
// pages for the rest.
const (
	// DefaultPageLimit is how many conversations a request naming no limit lists.
	DefaultPageLimit = 20
	// MaxPageLimit is the largest page a request may ask for. A larger one is refused
	// rather than shortened, so a console asking for more than it can get is told.
	MaxPageLimit = 100
)

// MaxSessionIDs is how many conversations one request may name to hydrate, and a request
// naming more is refused. A session id is 66 characters and the request carries them in
// the URL, so fifty of them keep the request line under the 4KB a server accepts.
const MaxSessionIDs = 50

// MaxTitleRunes is how much of a first prompt a title carries. A longer prompt is cut
// there and marked, since a rail shows one line per conversation.
const MaxTitleRunes = 80

// The lines a sessions request is refused with. Each says what happened and none names
// the store, the worker or an error.
const (
	unknownSession = "this conversation is not one this agent holds for a browser"
	listRefusal    = "the stored conversations could not be listed; ask again in a moment"
	openRefusal    = "the stored conversation could not be read; ask again in a moment"
	deleteRefusal  = "the stored conversation could not be deleted; ask again in a moment"
	staleCursor    = "this cursor is not one this agent minted; list from the start again"
)

// The query parameters the list endpoint reads, and what marks a title that was cut.
const (
	limitField    = "limit"
	cursorField   = "cursor"
	idField       = "id"
	titleEllipsis = "..."
)

// Conversations are the stored conversations this channel lists, opens and deletes.
//
// The channel holds this rather than runstate.Store, because where the agent runs
// decides what answers it. Backed by the store, as NewStoreConversations backs
// it, the process serving the page is the process that journaled the conversation.
// Backed by messages to a worker, the conversations are the worker's and this process
// holds none of them, and nothing above this interface changes.
//
// Every implementation answers for one channel's conversations alone: an id outside them
// is runstate.ErrNotFound from Load and Delete, and List never returns a row for one.
// The channel maps that error to a 404, so an id this list does not show can be neither
// opened nor deleted through it.
type Conversations interface {
	// List returns at most limit conversations from cursor, oldest first, with the
	// cursor the next page resumes from. An empty cursor starts at the oldest.
	//
	// The order is creation order and the caller sorts nothing: a caller that ordered
	// the whole listing would first have to read the whole listing.
	List(ctx context.Context, limit int, cursor string) (runstate.RunPage, error)

	// Describe returns the listing rows for ids, in the order the caller named them,
	// for a caller that holds the ids already and has nothing to enumerate. A UI
	// server keeping its own map of who owns which conversation asks this to draw a
	// rail.
	//
	// An id outside these conversations is left out, exactly as List leaves it out,
	// so a caller reads nothing about a conversation it may not see. An id named twice
	// is described once, where it was first named.
	Describe(ctx context.Context, ids []string) ([]runstate.RunInfo, error)

	// Load reads a conversation for a Format to render. It returns the folded state
	// rather than a rendering, since a browser reads AI SDK parts or AG-UI messages and
	// which one is the Format's business.
	Load(ctx context.Context, id string) (*runstate.RunState, error)

	// Delete removes a conversation and everything journaled under it.
	Delete(ctx context.Context, id string) error
}

// StoreConversations answers Conversations from the run store this process writes.
//
// It scopes every call two ways, and the store applies both rather than this type
// dropping rows it already paid to read. The prefix keeps a Slack thread and a peer's
// prompt out of a browser's rail, since one store is handed to every channel. The agent
// identity keeps two agents sharing one JetStream stream apart, and a
// conversation journaled before that field existed is shown, which is
// runstate.ListFilter.MatchesAgent's rule.
type StoreConversations struct {
	store  runstate.Store
	filter runstate.ListFilter
}

// StoreConversations is the implementation the channel builds for itself, and the one a
// second implementation is measured against.
var _ Conversations = (*StoreConversations)(nil)

// NewStoreConversations returns the conversations identity minted through this channel,
// read from store.
func NewStoreConversations(store runstate.Store, identity string) *StoreConversations {
	return &StoreConversations{
		store:  store,
		filter: runstate.ListFilter{Agent: identity, Prefix: SessionPrefix},
	}
}

// List implements Conversations.
func (c *StoreConversations) List(ctx context.Context, limit int, cursor string) (runstate.RunPage, error) {
	return c.store.ListPage(ctx, c.filter, limit, cursor)
}

// Describe implements Conversations. The store applies the prefix and the identity, the
// pair the listing applies, so an id under another channel's prefix and a conversation
// another agent journaled are left out of the rows without this type reading either.
func (c *StoreConversations) Describe(ctx context.Context, ids []string) ([]runstate.RunInfo, error) {
	return c.store.Describe(ctx, c.filter, ids)
}

// Load implements Conversations. An id outside this channel's prefix is refused before
// the store is asked, and a conversation another agent journaled is refused after it is
// read, which is the same pair of rules the listing applies.
func (c *StoreConversations) Load(ctx context.Context, id string) (*runstate.RunState, error) {
	if !c.filter.MatchesID(id) {
		return nil, fmt.Errorf("%w: %q", runstate.ErrNotFound, id)
	}

	state, err := c.store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !c.filter.MatchesAgent(state.Agent) {
		return nil, fmt.Errorf("%w: %q", runstate.ErrNotFound, id)
	}

	return state, nil
}

// Delete implements Conversations. It reads the conversation first, so a delete reaches
// exactly the conversations the listing shows: an id under another channel's prefix, or a
// journal another agent wrote, is ErrNotFound and the store is never asked to remove it.
func (c *StoreConversations) Delete(ctx context.Context, id string) error {
	_, err := c.Load(ctx, id)
	if err != nil {
		return err
	}

	return c.store.Delete(ctx, id)
}

// Conversation is one row of the session list, as a page reads it.
type Conversation struct {
	// ID is the session id, which opens and deletes the conversation.
	ID string `json:"id"`
	// Title is the first prompt, cut to one line. A model wrote none: nothing stored
	// names a conversation, so this is what the person asked for first.
	Title string `json:"title"`
	// Created is when the conversation was opened and Updated when its last record was
	// written.
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Model is what answered the last turn.
	Model string `json:"model,omitempty"`
	// Terminal is why the last turn ended, empty for a conversation with a turn in
	// flight.
	Terminal runstate.TerminalReason `json:"terminal,omitempty"`
	// Summary is the conversation's cost when its last turn ended. It is absent
	// for a conversation whose last turn ended before a turn recorded one, and a page
	// shows an empty slot for that rather than counts of zero.
	Summary *ConversationCounts `json:"summary,omitempty"`
}

// ConversationCounts is the size of a conversation as its last turn left it.
type ConversationCounts struct {
	// Turns is how many turns the conversation has taken.
	Turns int64 `json:"turns"`
	// ContextTokens is the input the last model call carried, which the next turn sends
	// again before adding its prompt.
	ContextTokens int64 `json:"context_tokens"`
	// ToolCalls is how many tool calls the conversation has made.
	ToolCalls int64 `json:"tool_calls"`
}

// ConversationList is what the list endpoint answers.
type ConversationList struct {
	// Sessions are the page's conversations, oldest first.
	Sessions []Conversation `json:"sessions"`
	// Cursor asks for the page after this one and is absent once the listing has
	// reached the end. It is the store's own value: a page stores it and hands it back
	// unchanged, and it stays good across a reload and a restart.
	//
	// An answer to a request naming ids carries none: it holds the conversations the
	// caller named and there is nothing after them to ask for.
	Cursor string `json:"cursor,omitempty"`
}

// conversationFor turns a stored row into the row a page reads.
func conversationFor(info runstate.RunInfo) Conversation {
	out := Conversation{
		ID:       info.RunID,
		Title:    TitleFor(info.Prompt),
		Created:  info.Created,
		Updated:  info.Updated,
		Model:    info.Model,
		Terminal: info.Terminal,
	}

	if info.Summary != nil {
		out.Summary = &ConversationCounts{
			Turns:         info.Summary.Turns,
			ContextTokens: info.Summary.ContextTokens,
			ToolCalls:     info.Summary.Counters.ToolCalls,
		}
	}

	return out
}

// TitleFor names a conversation by its first prompt, on one line and cut to
// MaxTitleRunes with the cut marked. A prompt is what a person typed, so its line breaks
// and its runs of spaces are collapsed to single spaces before it is cut.
//
// It is exported because a caller listing conversations for itself names them the same
// way a page does.
func TitleFor(prompt string) string {
	title := strings.Join(strings.Fields(prompt), " ")

	runes := []rune(title)
	if len(runes) <= MaxTitleRunes {
		return title
	}

	return strings.TrimRight(string(runes[:MaxTitleRunes]), " ") + titleEllipsis
}

// serveSessionList answers this channel's conversations: the ones a request names by id,
// or a page of them from a cursor when it names none.
//
// A UI server in front of this channel holds its own map of who owns which conversation,
// so it enumerates nothing and asks for the rows of the ids it already has. A named row
// and a listed row are the same Conversation, so a console holds one renderer.
func (c *Channel) serveSessionList(w http.ResponseWriter, r *http.Request) {
	ids, err := sessionIDs(r)
	if err != nil {
		c.log.Warn("Refusing a listing request", "error", err, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}
	if len(ids) > 0 {
		c.describeSessions(w, r, ids)

		return
	}

	limit, err := pageLimit(r)
	if err != nil {
		c.log.Warn("Refusing a listing request", "error", err, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	page, err := c.conversations.List(r.Context(), limit, r.URL.Query().Get(cursorField))
	switch {
	case errors.Is(err, runstate.ErrInvalidCursor):
		c.log.Warn("Refusing a listing request carrying a cursor this store did not mint", "remote", r.RemoteAddr)
		http.Error(w, staleCursor, http.StatusBadRequest)

		return
	case err != nil:
		c.log.Error("Listing the stored conversations failed", "error", err, "remote", r.RemoteAddr)
		http.Error(w, listRefusal, http.StatusInternalServerError)

		return
	}

	out := ConversationList{Sessions: make([]Conversation, len(page.Runs)), Cursor: page.Cursor}
	for i, info := range page.Runs {
		out.Sessions[i] = conversationFor(info)
	}

	c.writeJSON(w, r, out, listRefusal)
}

// describeSessions answers the rows for the conversations a request named.
//
// An id this channel does not hold is left out of the answer rather than reported per id,
// which is what Load answers for an id outside the prefix or under another agent: a
// caller reads the rows it may see and learns nothing about the rest.
func (c *Channel) describeSessions(w http.ResponseWriter, r *http.Request, ids []string) {
	rows, err := c.conversations.Describe(r.Context(), ids)
	if err != nil {
		c.log.Error("Reading the named conversations failed", "error", err, "ids", len(ids), "remote", r.RemoteAddr)
		http.Error(w, listRefusal, http.StatusInternalServerError)

		return
	}

	out := ConversationList{Sessions: make([]Conversation, len(rows))}
	for i, info := range rows {
		out.Sessions[i] = conversationFor(info)
	}

	c.writeJSON(w, r, out, listRefusal)
}

// sessionIDs reads the conversations a request names, returning none for a request that
// names no id and lists a page instead.
//
// Naming ids and asking for a page are two different requests, so one carrying both is
// refused rather than resolved: twelve ids and a limit of twenty ask for two different
// answers and this endpoint would have to pick one.
func sessionIDs(r *http.Request) ([]string, error) {
	query := r.URL.Query()

	ids := query[idField]
	if len(ids) == 0 {
		return nil, nil
	}

	if query.Has(cursorField) || query.Has(limitField) {
		return nil, errors.New("a request naming conversations lists those and pages nothing; send id, or send cursor and limit")
	}
	if len(ids) > MaxSessionIDs {
		return nil, fmt.Errorf("a request names at most %d conversations and this one names %d; ask for the rest in a second request", MaxSessionIDs, len(ids))
	}

	return ids, nil
}

// pageLimit reads how many conversations a request asks for, taking DefaultPageLimit
// when it names none.
func pageLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get(limitField)
	if raw == "" {
		return DefaultPageLimit, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("the limit %q is not a number", raw)
	}
	if limit < 1 || limit > MaxPageLimit {
		return 0, fmt.Errorf("the limit %d is outside 1 to %d", limit, MaxPageLimit)
	}

	return limit, nil
}

// serveSessionOpen answers a stored conversation in the format it is mounted under.
//
// The conversation is read before the response head is written, so an id this channel
// does not hold is a 404 rather than a body that ends in an error. The writer then puts
// the whole conversation and closes on what its last turn ended with, and a page shows
// that when a person picks a row in the rail.
//
// A conversation the run left waiting on a question is asked it again after the replay,
// so a page opening a suspended conversation sees the card it stopped on rather than a
// thread that ends mid-air. The question is rebuilt from the pending turn, since nothing
// journals one.
func (c *Channel) serveSessionOpen(f Format) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		log := c.log.With("session", id)

		state, err := c.conversations.Load(r.Context(), id)
		if err != nil {
			c.refuseSession(w, r, log, err, openRefusal)

			return
		}

		// The ending is the one the conversation's last turn reached, so a page opening a
		// finished conversation reads the same close it would have read live. The
		// terminal record's message is the text of the error the run ended with, which
		// the live turn sent as the outcome's own error.
		ending := Ending{Outcome: serve.Outcome{ID: id, SessionID: id}}
		if state.Terminal != nil {
			ending.Outcome.Reason = state.Terminal.Reason
			if state.Terminal.Message != "" {
				ending.Outcome.Err = errors.New(state.Terminal.Message)
			}
		}

		writer := f.Replayer(w, r)
		writer.Open()
		writer.Replay(state)

		question, waiting := PendingQuestion(state, c.tools, c.confirmTags)
		if waiting {
			log.Info("Opening a conversation on the question it stopped at", "kind", question.Kind, "tool_use", question.ToolUseID, "remote", r.RemoteAddr)
			writer.Ask(question)
		}

		writer.Close(ending)
	}
}

// serveSessionDelete removes a stored conversation.
//
// It is the one destructive verb here and serve.Caller.Verified is false on this
// transport, so anyone who reaches this endpoint deletes any conversation this channel
// holds. The prefix and the identity stop a delete reaching another channel's
// conversation or another agent's; a deployment that has to stop it reaching another
// person's puts an authenticating proxy in front of this listener.
func (c *Channel) serveSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	log := c.log.With("session", id)

	err := c.conversations.Delete(r.Context(), id)
	if err != nil {
		c.refuseSession(w, r, log, err, deleteRefusal)

		return
	}

	log.Info("Deleted a stored conversation", "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// refuseSession answers a conversation this channel could not reach: a 404 for one it
// does not hold, and refusal for one it could not read, which names neither the store nor
// the error.
func (c *Channel) refuseSession(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error, refusal string) {
	if errors.Is(err, runstate.ErrNotFound) || errors.Is(err, runstate.ErrInvalidID) {
		log.Info("Refusing a request for a conversation this channel does not hold", "remote", r.RemoteAddr)
		http.Error(w, unknownSession, http.StatusNotFound)

		return
	}

	log.Error("Reaching a stored conversation failed", "error", err, "remote", r.RemoteAddr)
	http.Error(w, refusal, http.StatusInternalServerError)
}

// writeJSON answers with body as JSON, refusing with refusal when it cannot be encoded.
func (c *Channel) writeJSON(w http.ResponseWriter, r *http.Request, body any, refusal string) {
	encoded, err := json.Marshal(body)
	if err != nil {
		c.log.Error("Encoding an answer failed", "error", err, "remote", r.RemoteAddr)
		http.Error(w, refusal, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(encoded)
	if err != nil {
		c.log.Warn("Writing an answer failed", "error", err, "remote", r.RemoteAddr)
	}
}
