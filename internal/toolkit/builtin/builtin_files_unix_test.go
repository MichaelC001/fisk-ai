//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

//go:build !windows

package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A FIFO exists only on Unix, and opening one for reading blocks until a writer
// appears, so this spec holds the handler to stat before it opens.
var _ = Describe("read_file tool on unix", func() {
	It("Should refuse a FIFO without opening it", func() {
		root := GinkgoT().TempDir()
		Expect(syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644)).To(Succeed())

		spec, err := readFileSpec(nil, json.RawMessage(`{"root": `+jsonString(root)+`}`))
		Expect(err).ToNot(HaveOccurred())

		done := make(chan error, 1)
		go func() {
			_, err := callTool(mustNew(spec), context.Background(), json.RawMessage(`{"path":"fifo"}`), nil)
			done <- err
		}()

		Eventually(done).Should(Receive(MatchError(ContainSubstring("not a regular file"))))
	})
})
