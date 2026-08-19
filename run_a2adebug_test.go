//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("--a2a-debug", func() {
	// The file is written in the working directory under a fixed name, so a spec has to
	// stand somewhere of its own.
	BeforeEach(func() {
		GinkgoT().Chdir(GinkgoT().TempDir())

		a2aDebug = false
		DeferCleanup(func() { a2aDebug = false })
	})

	It("Should write nothing without the flag", func() {
		out, err := resolveA2ADebugOut()
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(BeNil())

		_, err = os.Stat(a2aDebugFilename)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	// The dump holds the conversation token, the prompt and every tool result, so it is
	// created readable by its owner alone.
	It("Should create the file 0600", func() {
		a2aDebug = true

		out, err := resolveA2ADebugOut()
		Expect(err).ToNot(HaveOccurred())
		Expect(out).ToNot(BeNil())
		DeferCleanup(func() { Expect(out.Close()).To(Succeed()) })

		info, err := os.Stat(a2aDebugFilename)
		Expect(err).ToNot(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("Should replace a dump an earlier run left behind", func() {
		Expect(os.WriteFile(a2aDebugFilename, []byte("previous run"), 0o600)).To(Succeed())

		a2aDebug = true

		out, err := resolveA2ADebugOut()
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(out.Close()).To(Succeed()) })

		body, err := os.ReadFile(a2aDebugFilename)
		Expect(err).ToNot(HaveOccurred())
		Expect(body).To(BeEmpty(), "the earlier dump is gone rather than appended to")
	})

	// The name is fixed and predictable, so somebody could plant a symlink at it. It is
	// removed and then created exclusively: removing drops the link, and O_EXCL refuses
	// one re-created in the race window rather than writing through it.
	It("Should not write through a symlink planted at the name", func() {
		target := filepath.Join(GinkgoT().TempDir(), "victim")
		Expect(os.WriteFile(target, []byte("do not touch"), 0o600)).To(Succeed())
		Expect(os.Symlink(target, a2aDebugFilename)).To(Succeed())

		a2aDebug = true

		out, err := resolveA2ADebugOut()
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(out.Close()).To(Succeed()) })

		_, err = out.Write([]byte("dump\n"))
		Expect(err).ToNot(HaveOccurred())

		// The planted file is untouched and the dump is a regular file of its own.
		body, err := os.ReadFile(target)
		Expect(err).ToNot(HaveOccurred())
		Expect(string(body)).To(Equal("do not touch"))

		info, err := os.Lstat(a2aDebugFilename)
		Expect(err).ToNot(HaveOccurred())
		Expect(info.Mode() & os.ModeSymlink).To(BeZero())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})
})
