package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// claudeConv is a conversation with Claude through the Messages API.
type claudeConv struct {
	client    *anthropic.Client
	fallbacks bool

	mu       sync.Mutex
	messages []anthropic.BetaMessageParam
}

func (c *claudeConv) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages) == 0
}

func (c *claudeConv) addUser(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.messages); n > 0 && c.messages[n-1].Role == anthropic.BetaMessageParamRoleUser {
		c.messages[n-1].Content = append(c.messages[n-1].Content, anthropic.NewBetaTextBlock(text))
	} else {
		c.messages = append(c.messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
	}
}

func (c *claudeConv) addResults(results []toolResult) {
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.call.ID, r.output, r.isError))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, anthropic.NewBetaUserMessage(blocks...))
}

func (c *claudeConv) rollback() {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.messages)
	if n == 0 || c.messages[n-1].Role != anthropic.BetaMessageParamRoleUser {
		return
	}
	for _, b := range c.messages[n-1].Content {
		if b.OfToolResult != nil {
			return
		}
	}
	c.messages = c.messages[:n-1]
}

func (c *claudeConv) step(ctx context.Context, req request, events chan<- Event) (stepResult, error) {
	c.mu.Lock()
	params := anthropic.BetaMessageNewParams{
		Model:     req.model,
		MaxTokens: 64000,
		System: []anthropic.BetaTextBlockParam{{
			Text:         req.system,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Messages: append([]anthropic.BetaMessageParam(nil), c.messages...),
		Tools:    claudeTools(req.tools),
		Thinking: anthropic.BetaThinkingConfigParamUnion{
			OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{Display: anthropic.BetaThinkingConfigAdaptiveDisplaySummarized},
		},
		// Caches the conversation so far, so each tool round trip only pays for
		// what is new.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if c.fallbacks {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	c.mu.Unlock()

	msg, err := c.stream(ctx, params, events)
	res := stepResult{
		inTokens:  msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens,
		outTokens: msg.Usage.OutputTokens,
		cache:     msg.Usage.CacheReadInputTokens,
	}
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.messages = append(c.messages, msg.ToParam())
	c.mu.Unlock()

	switch msg.StopReason {
	case anthropic.BetaStopReasonToolUse:
		for _, block := range msg.Content {
			use, ok := block.AsAny().(anthropic.BetaToolUseBlock)
			if !ok {
				continue
			}
			raw, ok := use.Input.(json.RawMessage)
			if !ok {
				raw, _ = json.Marshal(use.Input)
			}
			res.calls = append(res.calls, toolCall{ID: use.ID, Name: use.Name, Input: raw})
		}
	case anthropic.BetaStopReasonPauseTurn:
		// The server paused a long turn; sending the history back resumes it.
		res.again = true
	case anthropic.BetaStopReasonRefusal:
		reason := "Claude declined to continue this request"
		if msg.StopDetails.Explanation != "" {
			reason += ": " + msg.StopDetails.Explanation
		}
		res.stop = errors.New(reason)
	case anthropic.BetaStopReasonMaxTokens:
		res.stop = errors.New("the response hit the output token limit; ask Claude to continue")
	}
	return res, nil
}

// stream makes one streaming request and forwards deltas to the UI.
func (c *claudeConv) stream(ctx context.Context, params anthropic.BetaMessageNewParams, events chan<- Event) (*anthropic.BetaMessage, error) {
	s := c.client.Beta.Messages.NewStreaming(ctx, params)
	defer s.Close()
	msg := anthropic.BetaMessage{}
	inBlock := false
	for s.Next() {
		ev := s.Current()
		if err := msg.Accumulate(ev); err != nil {
			return &msg, err
		}
		switch e := ev.AsAny().(type) {
		case anthropic.BetaRawContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				inBlock = true
				events <- TextDelta{Text: d.Text}
			case anthropic.BetaThinkingDelta:
				inBlock = true
				events <- ThinkingDelta{Text: d.Thinking}
			}
		case anthropic.BetaRawContentBlockStopEvent:
			if inBlock {
				events <- BlockDone{}
				inBlock = false
			}
		}
	}
	if err := s.Err(); err != nil {
		if inBlock {
			events <- BlockDone{}
		}
		return &msg, err
	}
	return &msg, nil
}

func claudeTools(tools []*tool) []anthropic.BetaToolUnionParam {
	out := make([]anthropic.BetaToolUnionParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:                t.name,
			Description:         param.NewOpt(t.description),
			InputSchema:         anthropic.BetaToolInputSchemaParam{Properties: t.properties, Required: t.required},
			EagerInputStreaming: param.NewOpt(true),
		}})
	}
	return out
}
