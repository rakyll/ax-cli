// Package agent runs the conversational loop: it streams Claude's replies,
// executes AX tools on its behalf, and asks the user before anything that
// changes the cluster.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"github.com/google/ax-cli/internal/ax"
)

// DefaultModel is the Claude model used unless --model says otherwise.
const DefaultModel = "claude-opus-5"

// Decision is the user's answer to a permission request.
type Decision int

const (
	Deny Decision = iota
	Allow
	// AllowAlways allows this call and every later call to the same tool for
	// the rest of the session.
	AllowAlways
)

// Events are sent to the UI while a turn runs, in order.
type (
	// TextDelta is a chunk of streamed assistant text.
	TextDelta struct{ Text string }
	// ThinkingDelta is a chunk of Claude's summarized reasoning.
	ThinkingDelta struct{ Text string }
	// BlockDone marks the end of a streamed text or thinking block.
	BlockDone struct{}
	// ToolStart is sent when Claude calls a tool, before it runs.
	ToolStart struct {
		ID, Name, Summary string
	}
	// ToolDone carries a tool's result.
	ToolDone struct {
		ID, Name, Output string
		IsError          bool
	}
	// PermissionRequest asks the user to approve a mutating tool call. The UI
	// must send exactly one Decision on Reply.
	PermissionRequest struct {
		Tool, Title, Detail string
		Reply               chan<- Decision
	}
	// TurnDone ends a turn. Err is nil on success; Interrupted reports that the
	// user cancelled it.
	TurnDone struct {
		Err                       error
		Interrupted               bool
		InputTokens, OutputTokens int64
		CacheReadTokens           int64
	}
)

// Config configures an Agent.
type Config struct {
	Client   anthropic.Client
	Model    string
	AX       *ax.Client
	Atespace string
	// Docs is reference material about AX appended to the system prompt.
	Docs string
	// ServerFallbacks enables server-side refusal fallbacks. Only the Claude
	// API supports them.
	ServerFallbacks bool
	// ReadOnly denies every mutating tool without asking, for non-interactive runs.
	ReadOnly bool
}

// Agent holds one conversation.
type Agent struct {
	cfg   Config
	tools []*tool

	mu          sync.Mutex
	messages    []anthropic.BetaMessageParam
	atespace    string
	notes       []string
	alwaysAllow map[string]bool
}

// New returns an Agent with an empty conversation.
func New(cfg Config) *Agent {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	return &Agent{cfg: cfg, tools: axTools(), atespace: cfg.Atespace, alwaysAllow: map[string]bool{}}
}

// Model returns the Claude model in use.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Model
}

// SetModel switches the Claude model for subsequent turns.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Model = model
}

// Atespace returns the atespace tools default to.
func (a *Agent) Atespace() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.atespace
}

// SetAtespace changes the default atespace and tells Claude on the next turn.
func (a *Agent) SetAtespace(atespace string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if atespace == a.atespace {
		return
	}
	a.atespace = atespace
	a.notes = append(a.notes, fmt.Sprintf("The user switched the session atespace to %q. Tools now default to it.", atespace))
}

// Note queues context for Claude that is sent with the next user message, for
// example the output of a slash command the user ran.
func (a *Agent) Note(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notes = append(a.notes, text)
}

// Reset clears the conversation and session permissions.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = nil
	a.notes = nil
	a.alwaysAllow = map[string]bool{}
}

// Run sends prompt and drives the tool loop until Claude finishes, sending
// events as it goes. It always finishes by sending a TurnDone.
func (a *Agent) Run(ctx context.Context, prompt string, events chan<- Event) {
	done := TurnDone{}
	defer func() { events <- done }()

	a.mu.Lock()
	text := prompt
	if len(a.notes) > 0 {
		text = "<session-notes>\n" + strings.Join(a.notes, "\n") + "\n</session-notes>\n\n" + prompt
		a.notes = nil
	}
	// After an interrupted tool round the history ends with tool results; the
	// new prompt joins that user turn rather than starting a second one.
	if n := len(a.messages); n > 0 && a.messages[n-1].Role == anthropic.BetaMessageParamRoleUser {
		a.messages[n-1].Content = append(a.messages[n-1].Content, anthropic.NewBetaTextBlock(text))
	} else {
		a.messages = append(a.messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
	}
	a.mu.Unlock()

	for {
		msg, err := a.stream(ctx, events)
		if msg != nil {
			done.InputTokens += msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens
			done.OutputTokens += msg.Usage.OutputTokens
			done.CacheReadTokens += msg.Usage.CacheReadInputTokens
		}
		if err != nil {
			if ctx.Err() != nil {
				// Keep the prompt so Claude knows what it was doing; the
				// next prompt joins this user turn.
				done.Interrupted = true
				a.Note("The user interrupted your previous response before it finished.")
			} else {
				a.rollbackDanglingUser()
				done.Err = err
			}
			return
		}

		a.mu.Lock()
		a.messages = append(a.messages, msg.ToParam())
		a.mu.Unlock()

		switch msg.StopReason {
		case anthropic.BetaStopReasonToolUse:
			results := a.runTools(ctx, msg, events)
			a.mu.Lock()
			a.messages = append(a.messages, anthropic.NewBetaUserMessage(results...))
			a.mu.Unlock()
			if ctx.Err() != nil {
				done.Interrupted = true
				a.Note("The user interrupted you after the tool calls above.")
				return
			}
		case anthropic.BetaStopReasonPauseTurn:
			// The server paused a long turn; sending the history back resumes it.
		case anthropic.BetaStopReasonRefusal:
			reason := "Claude declined to continue this request"
			if msg.StopDetails.Explanation != "" {
				reason += ": " + msg.StopDetails.Explanation
			}
			done.Err = errors.New(reason)
			return
		case anthropic.BetaStopReasonMaxTokens:
			done.Err = errors.New("the response hit the output token limit; ask Claude to continue")
			return
		default:
			return
		}
	}
}

// Event is any of the event types above.
type Event any

// stream makes one streaming request and forwards deltas to the UI.
func (a *Agent) stream(ctx context.Context, events chan<- Event) (*anthropic.BetaMessage, error) {
	a.mu.Lock()
	params := anthropic.BetaMessageNewParams{
		Model:     a.cfg.Model,
		MaxTokens: 64000,
		System: []anthropic.BetaTextBlockParam{{
			Text:         systemPrompt(a.cfg),
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Messages: append([]anthropic.BetaMessageParam(nil), a.messages...),
		Tools:    toolParams(a.tools),
		Thinking: anthropic.BetaThinkingConfigParamUnion{
			OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{Display: anthropic.BetaThinkingConfigAdaptiveDisplaySummarized},
		},
		// Caches the conversation so far, so each tool round trip only pays for
		// what is new.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if a.cfg.ServerFallbacks {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	a.mu.Unlock()

	s := a.cfg.Client.Beta.Messages.NewStreaming(ctx, params)
	defer s.Close()
	msg := anthropic.BetaMessage{}
	inBlock := false
	for s.Next() {
		ev := s.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, err
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

// rollbackDanglingUser drops a trailing user prompt that never got a reply.
// Trailing tool results are kept: they must stay paired with the tool calls
// before them, and the next prompt is appended to that turn.
func (a *Agent) rollbackDanglingUser() {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.messages)
	if n == 0 || a.messages[n-1].Role != anthropic.BetaMessageParamRoleUser {
		return
	}
	for _, b := range a.messages[n-1].Content {
		if b.OfToolResult != nil {
			return
		}
	}
	a.messages = a.messages[:n-1]
}

// runTools executes every tool call in msg, in order, and returns one
// tool_result per call. Calls left after a cancellation are answered with an
// error so the history stays valid.
func (a *Agent) runTools(ctx context.Context, msg *anthropic.BetaMessage, events chan<- Event) []anthropic.BetaContentBlockParamUnion {
	var results []anthropic.BetaContentBlockParamUnion
	for _, block := range msg.Content {
		use, ok := block.AsAny().(anthropic.BetaToolUseBlock)
		if !ok {
			continue
		}
		out, isErr := a.runTool(ctx, use, events)
		events <- ToolDone{ID: use.ID, Name: use.Name, Output: out, IsError: isErr}
		results = append(results, anthropic.NewBetaToolResultBlock(use.ID, out, isErr))
	}
	return results
}

func (a *Agent) runTool(ctx context.Context, use anthropic.BetaToolUseBlock, events chan<- Event) (string, bool) {
	if ctx.Err() != nil {
		events <- ToolStart{ID: use.ID, Name: use.Name}
		return "Interrupted by the user before this tool ran.", true
	}
	t := findTool(a.tools, use.Name)
	if t == nil {
		events <- ToolStart{ID: use.ID, Name: use.Name}
		return fmt.Sprintf("unknown tool %q", use.Name), true
	}
	raw, ok := use.Input.(json.RawMessage)
	if !ok {
		raw, _ = json.Marshal(use.Input)
	}
	in, err := parseInput(raw, t.required)
	if err != nil {
		events <- ToolStart{ID: use.ID, Name: use.Name}
		return fmt.Sprintf("invalid input: %v. Fix the arguments and call the tool again.", err), true
	}
	atespace := in.str("atespace")
	if atespace == "" {
		atespace = a.Atespace()
	}
	call := &call{in: in, atespace: atespace, ax: a.cfg.AX}
	events <- ToolStart{ID: use.ID, Name: use.Name, Summary: t.summary(call)}

	if t.mutating {
		ok, msg := a.permit(ctx, t, call, events)
		if !ok {
			return msg, true
		}
	}
	out, err := t.run(ctx, call)
	if err != nil {
		if ctx.Err() != nil {
			return "Interrupted by the user while this tool was running.", true
		}
		return ax.ErrorMessage(err), true
	}
	return out, false
}

func (a *Agent) permit(ctx context.Context, t *tool, c *call, events chan<- Event) (bool, string) {
	a.mu.Lock()
	always := a.alwaysAllow[t.name]
	a.mu.Unlock()
	if always {
		return true, ""
	}
	if a.cfg.ReadOnly {
		return false, "This session is read-only; the user has not allowed changes. Tell the user the command they could run instead."
	}
	reply := make(chan Decision, 1)
	title, detail := t.prompt(c)
	events <- PermissionRequest{Tool: t.name, Title: title, Detail: detail, Reply: reply}
	select {
	case d := <-reply:
		switch d {
		case AllowAlways:
			a.mu.Lock()
			a.alwaysAllow[t.name] = true
			a.mu.Unlock()
			return true, ""
		case Allow:
			return true, ""
		default:
			return false, "The user denied this action. Stop and ask them how they would like to proceed."
		}
	case <-ctx.Done():
		return false, "Interrupted by the user."
	}
}

// input is a decoded tool input.
type input map[string]any

func (in input) str(k string) string {
	s, _ := in[k].(string)
	return s
}

func (in input) strs(k string) []string {
	raw, _ := in[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (in input) num(k string, def float64) float64 {
	if f, ok := in[k].(float64); ok {
		return f
	}
	return def
}

// parseInput decodes a tool input and checks its required fields. Inputs are
// streamed eagerly, so they arrive unvalidated and may be truncated.
func parseInput(raw json.RawMessage, required []string) (input, error) {
	in := input{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("arguments are not valid JSON: %v", err)
		}
	}
	for _, k := range required {
		switch v := in[k].(type) {
		case nil:
			return nil, fmt.Errorf("%q is required", k)
		case string:
			if strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("%q must not be empty", k)
			}
		case []any:
			if len(v) == 0 {
				return nil, fmt.Errorf("%q must not be empty", k)
			}
		}
	}
	return in, nil
}

func toolParams(tools []*tool) []anthropic.BetaToolUnionParam {
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

func findTool(tools []*tool, name string) *tool {
	for _, t := range tools {
		if t.name == name {
			return t
		}
	}
	return nil
}
