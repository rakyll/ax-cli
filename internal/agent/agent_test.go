package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/google/ax-cli/internal/ax"
	"github.com/google/ax-cli/internal/fakeax"
)

const taskManifest = `apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: hello
spec:
  image: "example.com/runner@sha256:3a0dea6ad8b55278685db58aca6e37dc4ba04056831d45bef3aaeafdca43cac6"
  command: ["sleep", "infinity"]
`

func newAgent(t *testing.T, readOnly bool) *Agent {
	t.Helper()
	addr, stop, err := fakeax.New().Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	c, err := ax.Dial(ax.Target{Server: "http://" + addr, Namespace: "ax-system"}, "ax")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return New(Config{AX: c, Atespace: "default", ReadOnly: readOnly})
}

// runTool runs one tool call, answering permission requests with decision.
func runTool(t *testing.T, a *Agent, name string, in map[string]any, decision Decision) (string, bool, int) {
	t.Helper()
	raw, _ := json.Marshal(in)
	events := make(chan Event, 16)
	asked := 0
	type result struct {
		out   string
		isErr bool
	}
	done := make(chan result)
	go func() {
		out, isErr := a.runTool(context.Background(), anthropic.BetaToolUseBlock{ID: "t1", Name: name, Input: json.RawMessage(raw)}, events)
		done <- result{out, isErr}
	}()
	for {
		select {
		case ev := <-events:
			if p, ok := ev.(PermissionRequest); ok {
				asked++
				p.Reply <- decision
			}
		case r := <-done:
			return r.out, r.isErr, asked
		}
	}
}

func TestToolsAgainstFakeServer(t *testing.T) {
	a := newAgent(t, false)

	out, isErr, asked := runTool(t, a, "apply_manifest", map[string]any{"manifest": taskManifest}, Allow)
	if isErr || asked != 1 || !strings.Contains(out, "task.ax.io/hello created") {
		t.Fatalf("apply = %q (err %v, asked %d)", out, isErr, asked)
	}

	out, isErr, asked = runTool(t, a, "list_resources", map[string]any{"kind": "task"}, Deny)
	if isErr || asked != 0 || !strings.Contains(out, "hello") || !strings.Contains(out, "Suspended") {
		t.Fatalf("list = %q (err %v, asked %d)", out, isErr, asked)
	}

	// Denied: nothing changes.
	out, isErr, _ = runTool(t, a, "resume_task", map[string]any{"name": "hello"}, Deny)
	if !isErr || !strings.Contains(out, "denied") {
		t.Fatalf("denied resume = %q (err %v)", out, isErr)
	}

	// Allowed always: the second call does not ask.
	if _, isErr, asked = runTool(t, a, "resume_task", map[string]any{"name": "hello"}, AllowAlways); isErr || asked != 1 {
		t.Fatalf("resume: err %v, asked %d", isErr, asked)
	}
	out, isErr, _ = runTool(t, a, "wait_for_task", map[string]any{"name": "hello", "phase": "Running", "timeout_seconds": 5}, Deny)
	if isErr || !strings.Contains(out, "Running") {
		t.Fatalf("wait = %q (err %v)", out, isErr)
	}
	out, isErr, asked = runTool(t, a, "resume_task", map[string]any{"name": "hello"}, Deny)
	if asked != 0 || !isErr || !strings.Contains(out, "not Suspended") {
		t.Fatalf("second resume = %q (err %v, asked %d)", out, isErr, asked)
	}

	// Missing arguments and unknown tools come back as errors for Claude.
	if out, isErr, _ = runTool(t, a, "describe_resource", map[string]any{"kind": "task"}, Deny); !isErr || !strings.Contains(out, "name") {
		t.Errorf("describe without name = %q", out)
	}
	if _, isErr, _ = runTool(t, a, "no_such_tool", nil, Deny); !isErr {
		t.Error("unknown tool succeeded")
	}
}

func TestReadOnlyDeniesChanges(t *testing.T) {
	a := newAgent(t, true)
	out, isErr, asked := runTool(t, a, "apply_manifest", map[string]any{"manifest": taskManifest}, Allow)
	if !isErr || asked != 0 || !strings.Contains(out, "read-only") {
		t.Fatalf("read-only apply = %q (err %v, asked %d)", out, isErr, asked)
	}
}

func TestParseInput(t *testing.T) {
	if _, err := parseInput(json.RawMessage(`{"kind":"task"}`), []string{"kind", "name"}); err == nil {
		t.Error("missing required field accepted")
	}
	if _, err := parseInput(json.RawMessage(`{"kind":`), nil); err == nil {
		t.Error("truncated JSON accepted")
	}
	in, err := parseInput(json.RawMessage(`{"command":["ls","-la"],"timeout_seconds":30}`), []string{"command"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(in.strs("command"), " "); got != "ls -la" {
		t.Errorf("strs = %q", got)
	}
	if got := in.num("timeout_seconds", 120); got != 30 {
		t.Errorf("num = %v", got)
	}
	if got := in.num("missing", 120); got != 120 {
		t.Errorf("num default = %v", got)
	}
}

func TestRollbackDanglingUser(t *testing.T) {
	a := &Agent{}
	a.messages = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
	}
	a.rollbackDanglingUser()
	if len(a.messages) != 0 {
		t.Errorf("plain prompt kept after a failed request")
	}

	a.messages = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaToolUseBlock("t1", map[string]any{}, "list_resources")}},
		anthropic.NewBetaUserMessage(anthropic.NewBetaToolResultBlock("t1", "ok", false)),
	}
	a.rollbackDanglingUser()
	if len(a.messages) != 3 {
		t.Errorf("tool results dropped; they must stay paired with their calls")
	}
}

func TestManifestSummary(t *testing.T) {
	m := "apiVersion: ax.io/v1alpha1\nkind: Workspace\nmetadata:\n  name: repo\nspec:\n  git:\n    - name: ax\n---\n" + taskManifest
	if got := manifestSummary(m); got != "workspace/repo, task/hello" {
		t.Errorf("manifestSummary = %q", got)
	}
	if got := manifestSummary("garbage"); got != "manifest" {
		t.Errorf("manifestSummary(garbage) = %q", got)
	}
}

func TestSystemPromptMentionsSession(t *testing.T) {
	cfg := Config{AX: &ax.Client{Target: ax.Target{Server: "http://127.0.0.1:1", Context: "kind-dev"}}, Atespace: "team", ReadOnly: true, Docs: "DOCS"}
	p := systemPrompt(cfg)
	for _, want := range []string{"kind-dev", "http://127.0.0.1:1", "team", "read-only", "DOCS"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
}
