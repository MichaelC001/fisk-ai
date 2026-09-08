//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/serve/web/vercel"
)

func TestVercel(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/Web/Vercel")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// messageIDs matches the start part's minted id, which a spec replaces with a fixed
// one so the rest of the body can be pinned byte for byte.
var messageIDs = regexp.MustCompile(`"messageId":"[0-9A-Za-z]+"`)

// pinned is a response body with the minted message id replaced, so what a spec
// asserts on is every other byte the format decided.
func pinned(body string) string {
	return messageIDs.ReplaceAllString(body, `"messageId":"MID"`)
}

// decoded is one request through the format: the turn it asked for, the writer the
// response is written with, and the recorder that writer writes to.
type decoded struct {
	turn   web.Turn
	writer web.TurnWriter
	rec    *httptest.ResponseRecorder
}

// body is what the recorder holds, with the minted message id pinned.
func (d decoded) body() string { return pinned(d.rec.Body.String()) }

// decode reads body through the format, failing the spec if the format refuses it.
func decode(body string) decoded {
	GinkgoHelper()

	out, err := decodeErr(body)
	Expect(err).ToNot(HaveOccurred())

	return out
}

// opened is one page opening a stored conversation: the writer the format replays into
// and the recorder it writes to.
type opened struct {
	writer web.TurnWriter
	rec    *httptest.ResponseRecorder
}

// body is what the recorder holds, with the minted message id pinned.
func (o opened) body() string { return pinned(o.rec.Body.String()) }

// open is the writer the channel replays a stored conversation into, which is what the
// session open route asks the format for.
func open() opened {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fisk/v1/vercel/sessions/w-abc", nil)

	return opened{writer: vercel.New().Replayer(rec, req), rec: rec}
}

// decodeErr is decode for the specs that assert on the refusal.
func decodeErr(body string) (decoded, error) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/fisk/v1/vercel", strings.NewReader(body))

	turn, writer, err := vercel.New().Decode(rec, req)

	return decoded{turn: turn, writer: writer, rec: rec}, err
}
