package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/genai"
)

// geminiConv is a conversation with Gemini through the Gemini API. The history
// keeps the model's parts as returned, thought signatures included, which
// Gemini needs back to continue its reasoning across tool calls.
type geminiConv struct {
	client *genai.Client

	mu       sync.Mutex
	contents []*genai.Content
	calls    int
	// madeUp holds the IDs given to calls that arrived without one; their
	// responses are sent back without an ID, matched by name and order.
	madeUp map[string]bool
}

func (c *geminiConv) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.contents) == 0
}

func (c *geminiConv) addUser(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appendUser(&genai.Part{Text: text})
}

func (c *geminiConv) addResults(results []toolResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]*genai.Part, 0, len(results))
	for _, r := range results {
		key := "output"
		if r.isError {
			key = "error"
		}
		id := r.call.ID
		if c.madeUp[id] {
			id = ""
		}
		parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID: id, Name: r.call.Name, Response: map[string]any{key: r.output},
		}})
	}
	c.appendUser(parts...)
}

func (c *geminiConv) appendUser(parts ...*genai.Part) {
	if n := len(c.contents); n > 0 && c.contents[n-1].Role == genai.RoleUser {
		c.contents[n-1].Parts = append(c.contents[n-1].Parts, parts...)
		return
	}
	c.contents = append(c.contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
}

func (c *geminiConv) rollback() {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.contents)
	if n == 0 || c.contents[n-1].Role != genai.RoleUser {
		return
	}
	for _, p := range c.contents[n-1].Parts {
		if p.FunctionResponse != nil {
			return
		}
	}
	c.contents = c.contents[:n-1]
}

func (c *geminiConv) step(ctx context.Context, req request, events chan<- Event) (stepResult, error) {
	c.mu.Lock()
	contents := append([]*genai.Content(nil), c.contents...)
	c.mu.Unlock()
	config := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: req.system}}},
		Tools:             []*genai.Tool{{FunctionDeclarations: geminiTools(req.tools)}},
		ThinkingConfig:    &genai.ThinkingConfig{IncludeThoughts: true},
	}

	var (
		res     stepResult
		parts   []*genai.Part
		usage   *genai.GenerateContentResponseUsageMetadata
		finish  genai.FinishReason
		message string
		blocked string
		inBlock bool
		thought bool
	)
	endBlock := func() {
		if inBlock {
			events <- BlockDone{}
			inBlock = false
		}
	}
	for resp, err := range c.client.Models.GenerateContentStream(ctx, req.model, contents, config) {
		if err != nil {
			endBlock()
			setUsage(&res, usage)
			return res, err
		}
		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}
		if f := resp.PromptFeedback; f != nil && f.BlockReason != "" {
			blocked = strings.TrimSpace(string(f.BlockReason) + " " + f.BlockReasonMessage)
		}
		if len(resp.Candidates) == 0 {
			continue
		}
		cand := resp.Candidates[0]
		if cand.FinishReason != "" {
			finish, message = cand.FinishReason, cand.FinishMessage
		}
		if cand.Content == nil {
			continue
		}
		for _, p := range cand.Content.Parts {
			cp := *p
			switch {
			case p.FunctionCall != nil:
				endBlock()
				parts = append(parts, &cp)
			case p.Text != "" || len(p.ThoughtSignature) > 0:
				if inBlock && thought != p.Thought {
					endBlock()
				}
				if p.Text != "" {
					inBlock, thought = true, p.Thought
					if p.Thought {
						events <- ThinkingDelta{Text: p.Text}
					} else {
						events <- TextDelta{Text: p.Text}
					}
				}
				// Join streamed chunks into one part, keeping a signature
				// on the part it arrived with. A part may carry only a
				// signature, which the API rejects on its own.
				n := len(parts)
				switch {
				case n > 0 && mergeable(parts[n-1], p):
					parts[n-1].Text += p.Text
					parts[n-1].ThoughtSignature = p.ThoughtSignature
				case p.Text != "":
					parts = append(parts, &cp)
				case n > 0 && len(parts[n-1].ThoughtSignature) == 0:
					parts[n-1].ThoughtSignature = p.ThoughtSignature
				}
			case p.InlineData != nil || p.FileData != nil || p.ExecutableCode != nil || p.CodeExecutionResult != nil:
				parts = append(parts, &cp)
			}
		}
	}
	endBlock()
	setUsage(&res, usage)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(parts) > 0 {
		c.contents = append(c.contents, &genai.Content{Role: genai.RoleModel, Parts: parts})
	}
	for _, p := range parts {
		fc := p.FunctionCall
		if fc == nil {
			continue
		}
		id := fc.ID
		if id == "" {
			c.calls++
			id = fmt.Sprintf("gemini-call-%d", c.calls)
			if c.madeUp == nil {
				c.madeUp = map[string]bool{}
			}
			c.madeUp[id] = true
		}
		args, _ := json.Marshal(fc.Args)
		if fc.Args == nil {
			args = []byte("{}")
		}
		res.calls = append(res.calls, toolCall{ID: id, Name: fc.Name, Input: args})
	}

	switch {
	case blocked != "":
		res.stop = fmt.Errorf("Gemini blocked the prompt: %s", blocked)
	case finish == genai.FinishReasonMaxTokens:
		res.stop = fmt.Errorf("the response hit the output token limit; ask Gemini to continue")
	case finish == genai.FinishReasonMalformedFunctionCall && len(res.calls) == 0:
		res.stop = fmt.Errorf("Gemini produced a malformed tool call; try rephrasing the request")
	case finish != "" && finish != genai.FinishReasonStop && finish != genai.FinishReasonUnspecified && finish != genai.FinishReasonContinuation && len(res.calls) == 0:
		res.stop = fmt.Errorf("Gemini stopped (%s)%s", finish, prefixed(": ", message))
	}
	return res, nil
}

// mergeable reports whether streamed text part p continues last.
func mergeable(last, p *genai.Part) bool {
	return last.FunctionCall == nil && last.Text != "" && last.Thought == p.Thought && len(last.ThoughtSignature) == 0
}

func setUsage(res *stepResult, u *genai.GenerateContentResponseUsageMetadata) {
	if u == nil {
		return
	}
	res.inTokens = int64(u.PromptTokenCount + u.ToolUsePromptTokenCount)
	res.outTokens = int64(u.CandidatesTokenCount + u.ThoughtsTokenCount)
	res.cache = int64(u.CachedContentTokenCount)
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func geminiTools(tools []*tool) []*genai.FunctionDeclaration {
	out := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		schema := map[string]any{"type": "object", "properties": t.properties}
		if len(t.required) > 0 {
			schema["required"] = t.required
		}
		out = append(out, &genai.FunctionDeclaration{Name: t.name, Description: t.description, ParametersJsonSchema: schema})
	}
	return out
}
