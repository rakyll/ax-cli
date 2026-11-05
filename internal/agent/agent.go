// Package agent runs the conversational loop: it streams the model's replies
// (Claude or Gemini), executes AX tools on its behalf, and asks the user before
// anything that changes the cluster.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	"github.com/google/ax-cli/internal/ax"
)

// DefaultModel is the model used unless --model says otherwise.
const DefaultModel = "claude-opus-5"

// DefaultGeminiModel is the default when only Gemini is configured.
const DefaultGeminiModel = "gemini-3.1-pro-preview"

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
	// ThinkingDelta is a chunk of the model's summarized reasoning.
	ThinkingDelta struct{ Text string }
	// BlockDone marks the end of a streamed text or thinking block.
	BlockDone struct{}
	// ToolStart is sent when the model calls a tool, before it runs.
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
	// Claude and Gemini are the API clients; either may be nil when its
	// provider is not configured.
	Claude   *anthropic.Client
	Gemini   *genai.Client
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
	conv        conversation
	atespace    string
	notes       []string
	alwaysAllow map[string]bool
}

// New returns an Agent with an empty conversation.
func New(cfg Config) *Agent {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	a := &Agent{cfg: cfg, tools: axTools(), atespace: cfg.Atespace, alwaysAllow: map[string]bool{}}
	a.conv = a.newConversation(cfg.Model)
	return a
}

// Provider names the API serving model: "Gemini" for gemini-* models, else
// "Claude".
func Provider(model string) string {
	if strings.HasPrefix(model, "gemini") || strings.HasPrefix(model, "models/gemini") {
		return "Gemini"
	}
	return "Claude"
}

// Model returns the model in use.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Model
}

// Provider returns the name of the API serving the current model.
func (a *Agent) Provider() string { return Provider(a.Model()) }

// SetModel switches the model for subsequent turns. Histories do not carry
// across providers, so switching between Claude and Gemini starts a new
// conversation; restarted reports whether it did.
func (a *Agent) SetModel(model string) (restarted bool, err error) {
	if err := a.check(model); err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if Provider(model) != Provider(a.cfg.Model) {
		restarted = !a.conv.empty()
		a.conv = a.newConversation(model)
		a.notes = nil
	}
	a.cfg.Model = model
	return restarted, nil
}

// Check reports whether the provider for the current model is configured.
func (a *Agent) Check() error { return a.check(a.Model()) }

func (a *Agent) check(model string) error {
	switch Provider(model) {
	case "Gemini":
		if a.cfg.Gemini == nil {
			return fmt.Errorf("%s needs the Gemini API: set GEMINI_API_KEY", model)
		}
	default:
		if a.cfg.Claude == nil {
			return fmt.Errorf("%s needs Claude: set ANTHROPIC_API_KEY, or CLAUDE_CODE_USE_VERTEX=1 with ANTHROPIC_VERTEX_PROJECT_ID", model)
		}
	}
	return nil
}

func (a *Agent) newConversation(model string) conversation {
	if Provider(model) == "Gemini" {
		return &geminiConv{client: a.cfg.Gemini}
	}
	return &claudeConv{client: a.cfg.Claude, fallbacks: a.cfg.ServerFallbacks}
}

// Atespace returns the atespace tools default to.
func (a *Agent) Atespace() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.atespace
}

// SetAtespace changes the default atespace and tells the model on the next turn.
func (a *Agent) SetAtespace(atespace string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if atespace == a.atespace {
		return
	}
	a.atespace = atespace
	a.notes = append(a.notes, fmt.Sprintf("The user switched the session atespace to %q. Tools now default to it.", atespace))
}

// Note queues context for the model that is sent with the next user message, for
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
	a.conv = a.newConversation(a.cfg.Model)
	a.notes = nil
	a.alwaysAllow = map[string]bool{}
}

// conversation is one provider's message history and the way to extend it.
// Implementations guard their own history; step is never called concurrently.
type conversation interface {
	empty() bool
	// addUser appends user text, joining a trailing user turn if there is
	// one (after an interrupted tool round the history ends with results).
	addUser(text string)
	// step streams one model response, forwarding deltas to events, and on
	// success appends it to the history.
	step(ctx context.Context, req request, events chan<- Event) (stepResult, error)
	// addResults appends the results of the calls from the last step.
	addResults(results []toolResult)
	// rollback drops a trailing user prompt that never got a reply. Trailing
	// tool results are kept: they must stay paired with their calls.
	rollback()
}

type request struct {
	model, system string
	tools         []*tool
}

type stepResult struct {
	calls []toolCall
	// again asks for another step with no new input, as when the server
	// paused a long turn.
	again bool
	// stop ends the turn with an error, such as a refusal.
	stop                       error
	inTokens, outTokens, cache int64
}

type toolCall struct {
	ID, Name string
	Input    json.RawMessage
}

type toolResult struct {
	call    toolCall
	output  string
	isError bool
}

// Run sends prompt and drives the tool loop until the model finishes, sending
// events as it goes. It always finishes by sending a TurnDone.
func (a *Agent) Run(ctx context.Context, prompt string, events chan<- Event) {
	done := TurnDone{}
	defer func() { events <- done }()
	if done.Err = a.Check(); done.Err != nil {
		return
	}

	a.mu.Lock()
	text := prompt
	if len(a.notes) > 0 {
		text = "<session-notes>\n" + strings.Join(a.notes, "\n") + "\n</session-notes>\n\n" + prompt
		a.notes = nil
	}
	conv := a.conv
	req := request{model: a.cfg.Model, system: systemPrompt(a.cfg), tools: a.tools}
	a.mu.Unlock()
	conv.addUser(text)

	for {
		res, err := conv.step(ctx, req, events)
		done.InputTokens += res.inTokens
		done.OutputTokens += res.outTokens
		done.CacheReadTokens += res.cache
		if err != nil {
			if ctx.Err() != nil {
				// Keep the prompt so the model knows what it was doing;
				// the next prompt joins this user turn.
				done.Interrupted = true
				a.Note("The user interrupted your previous response before it finished.")
			} else {
				conv.rollback()
				done.Err = err
			}
			return
		}
		if res.stop != nil {
			done.Err = res.stop
			return
		}
		if len(res.calls) > 0 {
			conv.addResults(a.runTools(ctx, res.calls, events))
			if ctx.Err() != nil {
				done.Interrupted = true
				a.Note("The user interrupted you after the tool calls above.")
				return
			}
			continue
		}
		if !res.again {
			return
		}
	}
}

// Event is any of the event types above.
type Event any

// runTools executes calls in order and returns one result per call. Calls left
// after a cancellation are answered with an error so the history stays valid.
func (a *Agent) runTools(ctx context.Context, calls []toolCall, events chan<- Event) []toolResult {
	var results []toolResult
	for _, c := range calls {
		out, isErr := a.runTool(ctx, c, events)
		events <- ToolDone{ID: c.ID, Name: c.Name, Output: out, IsError: isErr}
		results = append(results, toolResult{call: c, output: out, isError: isErr})
	}
	return results
}

func (a *Agent) runTool(ctx context.Context, use toolCall, events chan<- Event) (string, bool) {
	if ctx.Err() != nil {
		events <- ToolStart{ID: use.ID, Name: use.Name}
		return "Interrupted by the user before this tool ran.", true
	}
	t := findTool(a.tools, use.Name)
	if t == nil {
		events <- ToolStart{ID: use.ID, Name: use.Name}
		return fmt.Sprintf("unknown tool %q", use.Name), true
	}
	in, err := parseInput(use.Input, t.required)
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

func findTool(tools []*tool, name string) *tool {
	for _, t := range tools {
		if t.name == name {
			return t
		}
	}
	return nil
}
