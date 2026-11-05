package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/google/ax-cli/internal/agent"
	"github.com/google/ax-cli/internal/ax"
)

func testModel(t *testing.T) *model {
	t.Helper()
	t.Setenv("AX_HOME", t.TempDir())
	c := &ax.Client{Target: ax.Target{Server: "http://127.0.0.1:1", Namespace: "ax-system"}}
	m := newModel(Options{Agent: agent.New(agent.Config{AX: c, Atespace: "default"}), AX: c})
	m.names = map[string][]string{"task": {"api-refactor", "docs-review"}, "workspace": {"repo"}}
	return m
}

func TestSplitStable(t *testing.T) {
	for _, tc := range []struct{ in, done, rest string }{
		{"Hello", "", "Hello"},
		{"One.\n\nTwo", "One.\n\n", "Two"},
		{"Look:\n\n```yaml\na: 1\n\nb: 2\n", "Look:\n\n", "```yaml\na: 1\n\nb: 2\n"},
		{"```\nx\n```\n\nAfter", "```\nx\n```\n\n", "After"},
	} {
		done, rest := splitStable(tc.in)
		if done != tc.done || rest != tc.rest {
			t.Errorf("splitStable(%q) = %q, %q; want %q, %q", tc.in, done, rest, tc.done, tc.rest)
		}
	}
}

func TestSuggestions(t *testing.T) {
	m := testModel(t)
	labels := func(v string) []string {
		var out []string
		for _, s := range m.suggestions(v) {
			out = append(out, s.text)
		}
		return out
	}
	if got := labels("/de"); strings.Join(got, ",") != "/describe ,/delete " {
		t.Errorf("/de → %q", got)
	}
	if got := labels("/describe task d"); strings.Join(got, ",") != "/describe task docs-review " {
		t.Errorf("task names → %q", got)
	}
	if got := labels("/describe w"); strings.Join(got, ",") != "/describe workspace " {
		t.Errorf("kinds → %q", got)
	}
	if got := labels("/describe workspace "); strings.Join(got, ",") != "/describe workspace repo " {
		t.Errorf("workspace names → %q", got)
	}
	if got := labels("/tasks"); len(got) != 0 {
		t.Errorf("complete command still offers %q", got)
	}
	if got := labels("hello"); len(got) != 0 {
		t.Errorf("plain text offers %q", got)
	}
}

func TestPastedTextIsNotAKey(t *testing.T) {
	m := testModel(t)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("delete")})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" the task")})
	if got := m.ta.Value(); got != "delete the task" {
		t.Errorf("input = %q, want %q", got, "delete the task")
	}
}

func TestBackslashEnterInsertsNewline(t *testing.T) {
	m := testModel(t)
	m.setInput(`first\`)
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.ta.Value(); got != "first\n" {
		t.Errorf("input = %q", got)
	}
}

func TestPermissionKeys(t *testing.T) {
	for key, want := range map[string]agent.Decision{"1": agent.Allow, "2": agent.AllowAlways, "3": agent.Deny} {
		m := testModel(t)
		reply := make(chan agent.Decision, 1)
		m.perm = &agent.PermissionRequest{Tool: "delete_resource", Title: "Delete task x", Reply: reply}
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if got := <-reply; got != want {
			t.Errorf("key %s → %v, want %v", key, got, want)
		}
		if m.perm != nil {
			t.Errorf("key %s left the dialog open", key)
		}
	}
	m := testModel(t)
	reply := make(chan agent.Decision, 1)
	m.perm = &agent.PermissionRequest{Tool: "delete_resource", Reply: reply}
	m.key(tea.KeyMsg{Type: tea.KeyDown})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if got := <-reply; got != agent.AllowAlways {
		t.Errorf("down+enter → %v", got)
	}
}

func TestHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h")
	h := loadHistory(path)
	h.add("one")
	h.add("two\nlines")
	h.add("two\nlines")

	h = loadHistory(path)
	if len(h.entries) != 2 {
		t.Fatalf("entries = %q", h.entries)
	}
	if e, _ := h.prev("draft"); e != "two\nlines" {
		t.Errorf("prev = %q", e)
	}
	if e, _ := h.prev(""); e != "one" {
		t.Errorf("prev = %q", e)
	}
	if _, ok := h.prev(""); ok {
		t.Error("prev past the oldest entry")
	}
	h.next()
	if e, _ := h.next(); e != "draft" {
		t.Errorf("next restores %q, want the draft", e)
	}
}

func TestRenderTable(t *testing.T) {
	out := stripANSI(renderTable(ax.Table{
		Header:       []string{"NAME", "PHASE"},
		Rows:         [][]string{{"a", "Running"}, {"longer", "Suspended"}},
		StatusColumn: 1,
	}))
	want := "NAME     PHASE\na        Running\nlonger   Suspended"
	if out != want {
		t.Errorf("renderTable =\n%s\nwant\n%s", out, want)
	}
}
