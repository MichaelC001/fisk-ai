//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// init registers this provider under its neutral name so a build that imports this
// package can resolve it from llm.NewProvider without naming the package at the
// call site. The factory adapts neutral Config to Options; construction cannot fail
// here (the base URL is validated by the caller before resolution), so it never
// returns an error.
//
// The registered credential env vars are the secret-bearing variables the
// anthropic-sdk-go default credential chain (anthropic.NewClient ->
// DefaultClientOptions) reads to authenticate the agent's own API requests; they
// are stripped from a tool subprocess's environment so a tool, whose command line
// the model chooses, can never read them. Selector variables that merely point at
// on-disk credentials (ANTHROPIC_PROFILE, ANTHROPIC_CONFIG_DIR, XDG_CONFIG_HOME) are
// deliberately not listed: they hold no secret, and the files they locate are
// guarded by filesystem permissions, not by stripping an env var a tool could
// rediscover anyway.
func init() {
	llm.Register(ProviderName, func(cfg llm.Config) (llm.Provider, error) {
		return NewProvider(Options{
			APIKey:      cfg.APIKey,
			BaseURL:     cfg.BaseURL,
			Timeout:     cfg.Timeout,
			Middlewares: cfg.Middlewares,
		}), nil
	}, []string{
		"ANTHROPIC_API_KEY",             // API key
		"ANTHROPIC_AUTH_TOKEN",          // OAuth / bearer token
		"ANTHROPIC_IDENTITY_TOKEN",      // workload-identity-federation token (literal value)
		"ANTHROPIC_WEBHOOK_SIGNING_KEY", // webhook signing secret
		"ANTHROPIC_CUSTOM_HEADERS",      // may carry Authorization / x-api-key headers
	})
}

// Options configure a Provider. APIKey and BaseURL address the backend, Timeout
// bounds a single call, and Middlewares carry the cross-cutting request hooks
// (request trace, HTTP debug dump) the caller assembles. Middlewares is neutral
// (llm.Middleware is http-shaped, not SDK-typed); this package converts it to the
// SDK's request option when it builds the client, keeping the caller SDK-free.
type Options struct {
	APIKey      string
	BaseURL     string
	Timeout     time.Duration
	Middlewares []llm.Middleware
}

// Provider is the Anthropic implementation of llm.Provider. It wraps the SDK
// client and is the only place the SDK is spoken on the call path: it renders a
// neutral Request to MessageNewParams, issues the call under a per-call timeout,
// and converts the reply back to the neutral model.
type Provider struct {
	client  sdk.Client
	timeout time.Duration
}

// NewProvider builds a Provider from Options. The base URL is validated by the
// caller before construction so its error can name the flag the operator set.
func NewProvider(opts Options) *Provider {
	clientOpts := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	for _, m := range opts.Middlewares {
		clientOpts = append(clientOpts, option.WithMiddleware(m))
	}

	return &Provider{
		client:  sdk.NewClient(clientOpts...),
		timeout: opts.Timeout,
	}
}

// Capabilities reports what this provider supports. Anthropic offers server-side
// tool search; the output-token ceiling is left unset because the SDK enforces it
// per model and the request already carries the caller's chosen cap.
func (p *Provider) Capabilities() llm.Caps {
	return llm.Caps{
		Provider:           ProviderName,
		SemconvProvider:    SemconvProviderName,
		SupportsToolSearch: true,
	}
}

// Call issues one Anthropic request under the provider's per-call timeout and
// returns the reply in the neutral model. When thinking was requested and the API
// rejects the request with a 400, it adds a hint that the model may not support
// thinking, since that is the common cause and disabling it is not an obvious
// remedy; the caller wraps the result with its own "llm call" context.
func (p *Provider) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	msg, err := p.client.Messages.New(callCtx, params)
	if err != nil {
		return nil, badRequestHint(err, req)
	}

	resp, err := ResponseToNeutral(msg)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// badRequestHint returns err with a hint about the reasoning parameters the
// backend may have refused, when the request set either of them and the API
// answered 400. Any other error is returned unchanged.
//
// Either explicit thinking mode sends a parameter, and so does an effort level, so
// either can be what a model or a proxy rejected. The thinking remedy is to remove
// the block rather than to set it false, since false is still a parameter and would
// be rejected the same way. An effort level is refused here rather than at start-up
// because the levels a model takes are its own.
//
// Both call paths use it: the SDK builds the API error from the response headers
// before a stream is handed back, so a streamed call is refused the same way a
// batched one is.
func badRequestHint(err error, req llm.Request) error {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return err
	}

	thinking := req.Thinking != llm.ThinkingUnset

	switch {
	case thinking && req.ReasoningEffort != "":
		return fmt.Errorf("%w; model %q may not accept a thinking parameter or the effort level %q; remove the llm.thinking block or llm.reasoning_effort", err, req.Model, req.ReasoningEffort)
	case thinking:
		return fmt.Errorf("%w; model %q may not accept a thinking parameter, remove the llm.thinking block to send none", err, req.Model)
	case req.ReasoningEffort != "":
		return fmt.Errorf("%w; model %q may not accept the effort level %q, set llm.reasoning_effort to one it takes or remove it", err, req.Model, req.ReasoningEffort)
	}

	return err
}

// buildParams renders a neutral Request to Anthropic MessageNewParams. It is
// separated from Call so the request assembly, including the load-bearing
// prompt-cache breakpoint placement, is exercised by tests without a wire call.
func (p *Provider) buildParams(req llm.Request) (sdk.MessageNewParams, error) {
	system := make([]sdk.TextBlockParam, len(req.SystemBlocks))
	for i, s := range req.SystemBlocks {
		system[i] = sdk.TextBlockParam{Text: s}
	}

	// Prompt caching places two cache_control breakpoints. The tools+system
	// breakpoint marks the last system block; the conversation-tail breakpoint is the
	// request-level CacheControl below. The system slice is built fresh here each call
	// from req.SystemBlocks, so marking its last element cannot write through to any
	// value the caller hashes into the run fingerprint: the fingerprint stays
	// marker-free and toggling the cache never refuses a resume.
	if req.PromptCache && len(system) > 0 {
		system[len(system)-1].CacheControl = cacheControl(req.Interactive)
	}

	tools := make([]sdk.ToolUnionParam, 0, len(req.Tools)+1)
	for _, td := range req.Tools {
		tools = append(tools, ToolDefToAnthropic(td))
	}
	if req.ToolSearch {
		tools = append(tools, toolSearchTool())
	}

	messages := make([]sdk.MessageParam, len(req.Messages))
	for i, m := range req.Messages {
		// A request that asks for thinking off does not replay the thinking a previous
		// run produced. The signature on a stored block is only meaningful to a call
		// that is thinking, so sending one to a call that is not risks being refused for
		// blocks the model was told not to produce, which is what would otherwise make
		// forcing a resume from thinking to not-thinking fail at the API instead of
		// working.
		//
		// Only the explicit off does this. An unset mode leaves the model to its own
		// behavior, which may well be to think, and stripping there would break the
		// signature chain within a single run: the model produces thinking alongside a
		// tool_use, and the next iteration has to send it back.
		if req.Thinking == llm.ThinkingOff {
			m = withoutThinking(m)
		}

		mp, err := MessageToAnthropic(m)
		if err != nil {
			return sdk.MessageNewParams{}, fmt.Errorf("message %d: %w", i, err)
		}
		messages[i] = mp
	}

	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: req.MaxOutputTokens,
		System:    system,
		Tools:     tools,
		Messages:  messages,
	}

	// An unset mode leaves the union zero, which omits the field entirely so the model
	// and backend use their default behavior. The two explicit modes each send a
	// parameter, which is the whole difference between saying nothing and asking for
	// nothing: a model that reasons unaided only stops when it is told to.
	//
	// Summarized display returns readable reasoning even on models that omit it by
	// default.
	switch req.Thinking {
	case llm.ThinkingOn:
		params.Thinking = sdk.ThinkingConfigParamUnion{
			OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{
				Display: sdk.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
	case llm.ThinkingOff:
		params.Thinking = sdk.ThinkingConfigParamUnion{
			OfDisabled: &sdk.ThinkingConfigDisabledParam{},
		}
	}

	// Sent as written rather than checked against the SDK's five constants. Which
	// levels a model takes is the model's to say, and one released after this build may
	// take a level neither this code nor the SDK names, so an unrecognized value goes to
	// the API and is refused there rather than here.
	if req.ReasoningEffort != "" {
		params.OutputConfig.Effort = sdk.OutputConfigEffort(req.ReasoningEffort)
	}

	if req.PromptCache {
		params.CacheControl = cacheControl(req.Interactive)
	}

	return params, nil
}

// cacheControl is the cache_control marker for a run's breakpoints. A chat run
// uses a 1h TTL because an operator's think-time between turns commonly exceeds the
// default 5m (a 5m TTL there would pay a cache write with no read, a net cost
// increase); an autonomous loop uses the 5m default, which its tight turn-to-turn
// cadence stays within.
func cacheControl(interactive bool) sdk.CacheControlEphemeralParam {
	if interactive {
		return sdk.CacheControlEphemeralParam{TTL: sdk.CacheControlEphemeralTTLTTL1h}
	}
	return sdk.NewCacheControlEphemeralParam()
}

// toolSearchTool returns the BM25 tool search server tool. It is never deferred,
// so it is always present when requested and lets the model search the deferred
// custom tools by name and description and pull in the ones it needs.
func toolSearchTool() sdk.ToolUnionParam {
	return sdk.ToolUnionParam{OfToolSearchToolBm25_20251119: &sdk.ToolSearchToolBm25_20251119Param{
		Type: sdk.ToolSearchToolBm25_20251119TypeToolSearchToolBm25,
	}}
}
