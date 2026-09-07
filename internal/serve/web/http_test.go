//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These drive the handler directly, so a refusal is asserted on without a server behind
// the channel to run what would have been admitted.
var _ = Describe("The handler", func() {
	var (
		ch     *Channel
		format *fakeFormat
	)

	BeforeEach(func() {
		format = &fakeFormat{}

		opts := testOptions()
		opts.Formats = []Mount{{Path: "fake", Format: format}}
		ch = newTestChannel(opts)
	})

	// request builds one against the bound address, so the Host header names the
	// listener the way a browser's would.
	request := func(method, path string, body turnBody, headers map[string]string) *httptest.ResponseRecorder {
		GinkgoHelper()

		encoded, err := json.Marshal(body)
		Expect(err).ToNot(HaveOccurred())

		req := httptest.NewRequest(method, "http://"+ch.Addr()+path, bytes.NewReader(encoded))
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		ch.server.Handler.ServeHTTP(rec, req)

		return rec
	}

	Describe("CORS", func() {
		// A cross-origin POST still reaches the handler and would still run a turn, so
		// the list is enforced here rather than left to the browser.
		It("Should refuse an unlisted origin before the body is read", func() {
			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{Thread: "t1", Prompt: "hi"}, map[string]string{"Origin": "http://evil.example"})

			Expect(rec.Code).To(Equal(http.StatusForbidden))
			Expect(rec.Body.String()).To(ContainSubstring("http://evil.example"))
			Expect(format.decoded()).To(BeZero(), "the format never saw the request")
		})

		It("Should answer a preflight for a listed origin", func() {
			rec := request(http.MethodOptions, "/fisk/v1/fake", turnBody{}, map[string]string{
				"Origin":                         testOrigin,
				"Access-Control-Request-Method":  "POST",
				"Access-Control-Request-Headers": "content-type, x-thread",
			})

			Expect(rec.Code).To(Equal(http.StatusNoContent))
			Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(Equal(testOrigin))
			Expect(rec.Header().Get("Access-Control-Allow-Methods")).To(Equal("POST, OPTIONS"))
			Expect(rec.Header().Get("Access-Control-Allow-Headers")).To(Equal("content-type, x-thread"))
			Expect(rec.Header().Get("Access-Control-Max-Age")).To(Equal(preflightMaxAge))
			Expect(rec.Header().Values("Vary")).To(ContainElement("Origin"))
		})

		It("Should refuse a preflight from an unlisted origin", func() {
			rec := request(http.MethodOptions, "/fisk/v1/fake", turnBody{}, map[string]string{
				"Origin":                        "http://evil.example",
				"Access-Control-Request-Method": "POST",
			})

			Expect(rec.Code).To(Equal(http.StatusForbidden))
			Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(BeEmpty())
		})

		// A request carrying no Origin is not a browser's cross-origin request: curl, a
		// same-origin page and a proxy all send none.
		It("Should pass a request carrying no origin", func() {
			format.decodeErr = errBadBody

			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{}, nil)

			Expect(rec.Code).To(Equal(http.StatusBadRequest), "it reached the format")
			Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(BeEmpty())
		})

		It("Should let a listed origin read the answer and every header", func() {
			format.decodeErr = errBadBody

			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{}, map[string]string{"Origin": testOrigin})

			Expect(rec.Code).To(Equal(http.StatusBadRequest), "it reached the format")
			Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(Equal(testOrigin))
			Expect(rec.Header().Get("Access-Control-Expose-Headers")).To(Equal("*"))
		})
	})

	Describe("The Host check", func() {
		// The listener is on loopback, so a Host naming anything else is a name that
		// resolved to loopback in somebody's DNS.
		It("Should refuse a Host that is not the loopback listener", func() {
			encoded, err := json.Marshal(turnBody{Thread: "t1", Prompt: "hi"})
			Expect(err).ToNot(HaveOccurred())

			req := httptest.NewRequest(http.MethodPost, "http://"+ch.Addr()+"/fisk/v1/fake", bytes.NewReader(encoded))
			req.Host = "agent.evil.example:" + ch.port

			rec := httptest.NewRecorder()
			ch.server.Handler.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusForbidden))
			Expect(rec.Body.String()).To(ContainSubstring("agent.evil.example"))
			Expect(format.decoded()).To(BeZero())
		})

		It("Should accept every loopback name on the bound port and nothing else", func() {
			Expect(ch.refusesHost("127.0.0.1:" + ch.port)).To(BeFalse())
			Expect(ch.refusesHost("localhost:" + ch.port)).To(BeFalse())
			Expect(ch.refusesHost("LocalHost:" + ch.port)).To(BeFalse())
			Expect(ch.refusesHost("[::1]:" + ch.port)).To(BeFalse())

			Expect(ch.refusesHost("127.0.0.1:1")).To(BeTrue(), "another port")
			Expect(ch.refusesHost("127.0.0.1")).To(BeTrue(), "port 80 is not the bound port")
			Expect(ch.refusesHost("agent.example:" + ch.port)).To(BeTrue())
			Expect(ch.refusesHost("10.0.0.1:" + ch.port)).To(BeTrue())
		})

		// A deployment on any other address sits behind a proxy that owns the public
		// name and passes the browser's Host through unchanged, so the check would refuse
		// every request it forwarded.
		It("Should skip the check on a listener that is not loopback", func() {
			c := &Channel{loopback: false, port: "8080"}

			Expect(c.refusesHost("agent.example")).To(BeFalse())
		})
	})

	Describe("Routing", func() {
		It("Should refuse every path when nothing is mounted", func() {
			bare := newTestChannel(testOptions())

			req := httptest.NewRequest(http.MethodPost, "http://"+bare.Addr()+"/fisk/v1/fake", strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			bare.server.Handler.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("Should refuse a path outside the mounts and a method other than POST", func() {
			Expect(request(http.MethodPost, "/fisk/v1/other", turnBody{}, nil).Code).To(Equal(http.StatusNotFound))
			Expect(request(http.MethodPost, "/fake", turnBody{}, nil).Code).To(Equal(http.StatusNotFound))
			Expect(request(http.MethodGet, "/fisk/v1/fake", turnBody{}, nil).Code).To(Equal(http.StatusMethodNotAllowed))
			Expect(format.decoded()).To(BeZero())
		})

		It("Should refuse a request once the channel is draining", func() {
			Expect(ch.Close()).To(Succeed())

			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{Thread: "t1", Prompt: "hi"}, nil)

			Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
			Expect(rec.Header().Get("Retry-After")).To(Equal(retryAfter))
			Expect(format.decoded()).To(BeZero())
		})
	})

	Describe("Decoding", func() {
		It("Should answer a format's refusal as a 400 naming it", func() {
			format.decodeErr = errBadBody

			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{}, nil)

			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(rec.Body.String()).To(ContainSubstring(errBadBody.Error()))
		})

		It("Should refuse a turn naming no thread, asking nothing, or answering no call", func() {
			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{Prompt: "hi"}, nil)
			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(rec.Body.String()).To(ContainSubstring("names no thread"))

			rec = request(http.MethodPost, "/fisk/v1/fake", turnBody{Thread: "t1"}, nil)
			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(rec.Body.String()).To(ContainSubstring("neither a prompt nor an answer"))

			rec = request(http.MethodPost, "/fisk/v1/fake", turnBody{Thread: "t1", Answer: &answerBody{Kind: "approve"}}, nil)
			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(rec.Body.String()).To(ContainSubstring("names no tool call"))
		})

		// A thread the store does not hold has no question to answer, and a prompt is
		// the only thing that opens one.
		It("Should refuse an answer for a thread the store does not hold", func() {
			rec := request(http.MethodPost, "/fisk/v1/fake", turnBody{Thread: "t1", Answer: &answerBody{ToolUse: "c1", Kind: "approve"}}, nil)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
			Expect(rec.Body.String()).To(ContainSubstring("holds no conversation"))

			rec = request(http.MethodPost, "/fisk/v1/fake", turnBody{Thread: "t1", Prompt: "hi", Answer: &answerBody{ToolUse: "c1", Kind: "approve"}}, nil)
			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})
	})
})
