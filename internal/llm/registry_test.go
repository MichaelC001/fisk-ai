//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// timeoutProbe is the Config the timeoutProbeProvider factory last received, so a
// spec asserts what NewProvider handed the provider rather than what it was given.
var timeoutProbe Config

type timeoutProbeProvider struct{}

func (timeoutProbeProvider) Call(context.Context, Request) (*Response, error) { return nil, nil }
func (timeoutProbeProvider) Capabilities() Caps                               { return Caps{Provider: "timeout-probe"} }

func init() {
	Register("timeout-probe", func(cfg Config) (Provider, error) {
		timeoutProbe = cfg
		return timeoutProbeProvider{}, nil
	}, nil)
}

var _ = Describe("NewProvider", func() {
	It("Should hand the factory DefaultTimeout when the caller set none", func() {
		_, err := NewProvider("timeout-probe", Config{APIKey: "key"})
		Expect(err).NotTo(HaveOccurred())
		Expect(timeoutProbe.Timeout).To(Equal(DefaultTimeout))
		Expect(DefaultTimeout).To(Equal(120 * time.Second))
	})

	It("Should pass a timeout the caller set through unchanged", func() {
		_, err := NewProvider("timeout-probe", Config{Timeout: 5 * time.Second})
		Expect(err).NotTo(HaveOccurred())
		Expect(timeoutProbe.Timeout).To(Equal(5 * time.Second))
	})

	It("Should pass a negative timeout through, which asks the provider to add none", func() {
		_, err := NewProvider("timeout-probe", Config{Timeout: -1})
		Expect(err).NotTo(HaveOccurred())
		Expect(timeoutProbe.Timeout).To(Equal(time.Duration(-1)))
	})

	It("Should report an unregistered name as ErrUnknownProvider and list the linked providers", func() {
		_, err := NewProvider("nowhere", Config{})
		Expect(errors.Is(err, ErrUnknownProvider)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring(`"nowhere"`))
		Expect(err.Error()).To(ContainSubstring("timeout-probe"))
	})
})

var _ = Describe("mergeEnvNames", func() {
	It("Should return the sorted, deduplicated union across lists", func() {
		got := mergeEnvNames([][]string{
			{"B_KEY", "A_KEY"},
			{"A_KEY", "C_KEY"},
		})
		Expect(got).To(Equal([]string{"A_KEY", "B_KEY", "C_KEY"}))
	})

	It("Should trim whitespace and drop empty names", func() {
		got := mergeEnvNames([][]string{
			{"  A_KEY  ", "", "   "},
			{"A_KEY"},
		})
		Expect(got).To(Equal([]string{"A_KEY"}))
	})

	It("Should be case sensitive so distinct casings both survive", func() {
		got := mergeEnvNames([][]string{{"a_key", "A_KEY"}})
		Expect(got).To(Equal([]string{"A_KEY", "a_key"}))
	})

	It("Should return nil for no names", func() {
		Expect(mergeEnvNames(nil)).To(BeNil())
		Expect(mergeEnvNames([][]string{{}, {""}})).To(BeNil())
	})
})
