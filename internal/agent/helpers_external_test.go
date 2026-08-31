//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"

	"github.com/choria-io/fisk"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// exampleApp is a small fisk application with one runnable command carrying a flag
// and a required argument, so the tool it becomes has a genuine input schema.
func exampleApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing")
	do.Flag("level", "log level").Enum("debug", "info", "warn")
	do.Arg("subject", "the subject").Required().String()
	return app
}

// panicProvider panics on every model call, to exercise Run's panic barrier.
type panicProvider struct{}

func (panicProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	panic("boom in the model call")
}
func (panicProvider) Capabilities() llm.Caps { return llm.Caps{Provider: "anthropic"} }
