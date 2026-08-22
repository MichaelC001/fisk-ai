//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	slackgo "github.com/slack-go/slack"
)

var _ = Describe("namesOf", func() {
	// Plenty of workspaces have people setting the display-name field to their handle, and
	// the model answering "Roland Pienaar" should not be calling him rip.
	It("Should prefer the real name over the display-name field and over the username", func() {
		u := &slackgo.User{Name: "rip", RealName: "Roland Pienaar"}
		u.Profile.DisplayName = "rip"

		Expect(namesOf(u, "U024BE7LH")).To(Equal(person{Full: "Roland Pienaar", Username: "rip"}))
	})

	It("Should take the display-name field where the profile carries no real name", func() {
		u := &slackgo.User{Name: "rip"}
		u.Profile.DisplayName = "R.I. Pienaar"

		Expect(namesOf(u, "U024BE7LH").Full).To(Equal("R.I. Pienaar"))
	})

	It("Should take the username where neither name is set", func() {
		Expect(namesOf(&slackgo.User{Name: "rip"}, "U024BE7LH").Full).To(Equal("rip"))
	})

	// A line reading the id is worse than one reading a name and better than a line lost.
	It("Should take the id where Slack reports no name at all", func() {
		Expect(namesOf(&slackgo.User{}, "U024BE7LH")).To(Equal(person{Full: "U024BE7LH", Username: "U024BE7LH"}))
	})
})
