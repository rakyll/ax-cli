package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/google/ax-cli/internal/ax"
)

// command is a slash command. Slash commands act directly, without the model.
type command struct {
	name    string
	aliases []string
	args    string
	desc    string
	// complete returns candidates for argument i given the earlier arguments.
	complete func(m *model, i int, prev []string, token string) []string
	// run returns the work to do; nil means the command finished synchronously.
	run func(m *model, args []string) tea.Cmd
}

// cmdDoneMsg reports a finished slash or shell command.
type cmdDoneMsg struct {
	out string
	// md is markdown output, rendered on the UI goroutine at the current width.
	md  string
	err error
	// note, when set, tells the model what the user saw.
	note string
	// refresh reloads resource names for completion.
	refresh bool
}

var kinds = []string{"task", "workspace", "model"}

var models = []string{
	"claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-fable-5-1", "claude-haiku-4-5",
	"gemini-3.1-pro-preview", "gemini-pro-latest", "gemini-3.5-flash", "gemini-3-flash-preview",
}

func commands() []*command {
	return []*command{
		{name: "help", aliases: []string{"?"}, desc: "Show commands and shortcuts", run: (*model).help},
		{name: "tasks", desc: "List tasks", run: func(m *model, _ []string) tea.Cmd { return m.list("task") }},
		{name: "workspaces", desc: "List workspaces", run: func(m *model, _ []string) tea.Cmd { return m.list("workspace") }},
		{name: "models", desc: "List models", run: func(m *model, _ []string) tea.Cmd { return m.list("model") }},
		{
			name: "get", args: "<kind> [name]", desc: "List resources, or show one as YAML",
			complete: kindThenName,
			run: func(m *model, args []string) tea.Cmd {
				switch len(args) {
				case 1:
					return m.list(args[0])
				case 2:
					return m.get(args[0], args[1])
				}
				return usage("get <kind> [name]")
			},
		},
		{
			name: "describe", args: "<kind> <name>", desc: "Show a resource's details and conditions",
			complete: kindThenName,
			run: func(m *model, args []string) tea.Cmd {
				if len(args) == 1 {
					args = []string{"task", args[0]}
				}
				if len(args) != 2 {
					return usage("describe <kind> <name>")
				}
				return m.describe(args[0], args[1])
			},
		},
		{
			name: "apply", args: "<file>", desc: "Create or update resources from a manifest",
			complete: completeFile,
			run: func(m *model, args []string) tea.Cmd {
				if len(args) != 1 {
					return usage("apply <file>")
				}
				return m.apply(args[0])
			},
		},
		{
			name: "resume", args: "<task>", desc: "Resume a suspended task",
			complete: taskName,
			run: func(m *model, args []string) tea.Cmd {
				if len(args) != 1 {
					return usage("resume <task>")
				}
				return m.transition("resume", args[0])
			},
		},
		{
			name: "suspend", args: "<task>", desc: "Suspend a running task",
			complete: taskName,
			run: func(m *model, args []string) tea.Cmd {
				if len(args) != 1 {
					return usage("suspend <task>")
				}
				return m.transition("suspend", args[0])
			},
		},
		{
			name: "delete", args: "<kind> <name>", desc: "Delete a resource",
			complete: kindThenName,
			run: func(m *model, args []string) tea.Cmd {
				if len(args) != 2 {
					return usage("delete <kind> <name>")
				}
				return m.delete(args[0], args[1])
			},
		},
		{
			name: "ssh", args: "<task> [command...]", desc: "Open a shell in a running debug task",
			complete: taskName,
			run: func(m *model, args []string) tea.Cmd {
				if len(args) < 1 {
					return usage("ssh <task> [command...]")
				}
				cmd := args[1:]
				if len(cmd) > 0 && cmd[0] == "--" {
					cmd = cmd[1:]
				}
				return m.ssh(args[0], cmd)
			},
		},
		{
			name: "atespace", aliases: []string{"a"}, args: "[name]", desc: "Show or switch the atespace",
			run: func(m *model, args []string) tea.Cmd {
				if len(args) == 0 {
					m.say("atespace: " + m.agent.Atespace())
					return nil
				}
				m.agent.SetAtespace(args[0])
				m.say("Switched to atespace " + boldS.Render(args[0]))
				return m.refreshNames()
			},
		},
		{name: "ctx", aliases: []string{"context"}, desc: "Show the cluster connection", run: (*model).showContext},
		{
			name: "model", args: "[id]", desc: "Show or switch the model (Claude or Gemini)",
			complete: func(_ *model, i int, _ []string, _ string) []string {
				if i == 0 {
					return models
				}
				return nil
			},
			run: func(m *model, args []string) tea.Cmd {
				if len(args) == 0 {
					m.say("model: " + m.agent.Model())
					return nil
				}
				restarted, err := m.agent.SetModel(args[0])
				if err != nil {
					m.say(errS.Render(err.Error()))
					return nil
				}
				msg := "Switched to " + boldS.Render(args[0])
				if restarted {
					m.tokensIn, m.tokensOut = 0, 0
					msg += dimS.Render(" · new conversation; history does not carry between Claude and Gemini")
				}
				m.say(msg)
				return nil
			},
		},
		{
			name: "clear", desc: "Start a new conversation",
			run: func(m *model, _ []string) tea.Cmd {
				m.agent.Reset()
				m.tokensIn, m.tokensOut = 0, 0
				m.say("Started a new conversation.")
				return nil
			},
		},
		{name: "exit", aliases: []string{"quit", "q"}, desc: "Exit", run: func(m *model, _ []string) tea.Cmd { m.quitting = true; return tea.Quit }},
	}
}

func findCommand(cmds []*command, name string) *command {
	for _, c := range cmds {
		if c.name == name {
			return c
		}
		for _, a := range c.aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

func usage(u string) tea.Cmd {
	return func() tea.Msg { return cmdDoneMsg{err: fmt.Errorf("usage: /%s", u)} }
}

// Completers

func kindThenName(m *model, i int, prev []string, _ string) []string {
	switch i {
	case 0:
		return kinds
	case 1:
		k, err := ax.Kind(prev[0])
		if err != nil {
			return nil
		}
		return m.names[strings.ToLower(k)]
	}
	return nil
}

func taskName(m *model, i int, _ []string, _ string) []string {
	if i == 0 {
		return m.names["task"]
	}
	return nil
}

// completeFile offers directories and YAML files under token's directory.
func completeFile(_ *model, i int, _ []string, token string) []string {
	if i != 0 {
		return nil
	}
	dir, base := filepath.Split(token)
	entries, err := os.ReadDir(orDot(dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		switch {
		case e.IsDir():
			out = append(out, dir+n+"/")
		case strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml"):
			out = append(out, dir+n)
		}
	}
	return out
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// Implementations. Each runs off the UI goroutine and reports a cmdDoneMsg.

func (m *model) background(label string, timeout time.Duration, fn func(ctx context.Context) cmdDoneMsg) tea.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	m.startBusy(label, cancel)
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		defer cancel()
		msg := fn(ctx)
		if msg.err != nil && ctx.Err() == context.Canceled {
			msg.err = errInterrupted
		}
		return msg
	})
}

func (m *model) list(kind string) tea.Cmd {
	atespace := m.agent.Atespace()
	return m.background("Listing", 20*time.Second, func(ctx context.Context) cmdDoneMsg {
		t, err := m.ax.List(ctx, kind, atespace)
		if err != nil {
			return cmdDoneMsg{err: err}
		}
		return cmdDoneMsg{out: renderTable(t), note: t.Plain(), refresh: true}
	})
}

func (m *model) get(kind, name string) tea.Cmd {
	atespace := m.agent.Atespace()
	return m.background("Fetching", 20*time.Second, func(ctx context.Context) cmdDoneMsg {
		res, err := m.ax.Get(ctx, kind, atespace, name)
		if err != nil {
			return cmdDoneMsg{err: err}
		}
		y := ax.YAML(res)
		return cmdDoneMsg{md: "```yaml\n" + y + "\n```", note: y}
	})
}

func (m *model) describe(kind, name string) tea.Cmd {
	atespace := m.agent.Atespace()
	return m.background("Describing", 20*time.Second, func(ctx context.Context) cmdDoneMsg {
		out, err := m.ax.Describe(ctx, kind, atespace, name)
		if err != nil {
			return cmdDoneMsg{err: err}
		}
		return cmdDoneMsg{out: colorPhaseField(out), note: out}
	})
}

func (m *model) apply(path string) tea.Cmd {
	atespace := m.agent.Atespace()
	return m.background("Applying", time.Minute, func(ctx context.Context) cmdDoneMsg {
		data, err := os.ReadFile(path)
		if err != nil {
			return cmdDoneMsg{err: err}
		}
		results, err := m.ax.Apply(ctx, data, atespace)
		var lines []string
		for _, r := range results {
			lines = append(lines, r.String())
		}
		out := strings.Join(lines, "\n")
		if err != nil {
			if out != "" {
				err = fmt.Errorf("%s\n%s", out, ax.ErrorMessage(err))
			}
			return cmdDoneMsg{err: err, refresh: true}
		}
		return cmdDoneMsg{out: out, note: out, refresh: true}
	})
}

func (m *model) transition(verb, task string) tea.Cmd {
	atespace := m.agent.Atespace()
	label, past := "Resuming", "resumed"
	if verb == "suspend" {
		label, past = "Suspending", "suspended"
	}
	return m.background(label+" "+task, 5*time.Minute, func(ctx context.Context) cmdDoneMsg {
		var err error
		if verb == "suspend" {
			_, err = m.ax.Suspend(ctx, atespace, task)
		} else {
			_, err = m.ax.Resume(ctx, atespace, task)
		}
		if err != nil {
			return cmdDoneMsg{err: err}
		}
		out := fmt.Sprintf("task.ax.io/%s %s", task, past)
		return cmdDoneMsg{out: out, note: out, refresh: true}
	})
}

func (m *model) delete(kind, name string) tea.Cmd {
	atespace := m.agent.Atespace()
	return m.background("Deleting "+name, 2*time.Minute, func(ctx context.Context) cmdDoneMsg {
		k, err := ax.Kind(kind)
		if err != nil {
			return cmdDoneMsg{err: err}
		}
		if err := m.ax.Delete(ctx, k, atespace, name); err != nil {
			return cmdDoneMsg{err: err}
		}
		out := fmt.Sprintf("%s.ax.io/%s deleted", strings.ToLower(k), name)
		return cmdDoneMsg{out: out, note: out, refresh: true}
	})
}

// ssh hands the terminal to `ax ssh` until it exits.
func (m *model) ssh(task string, command []string) tea.Cmd {
	c := m.ax.SSHCommand(context.Background(), m.agent.Atespace(), task, command)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return cmdDoneMsg{err: fmt.Errorf("ax ssh %s: %w", task, err)}
		}
		return cmdDoneMsg{out: dimS.Render("ssh session to " + task + " closed")}
	})
}

// shell runs a local command for `!`, capturing its output.
func (m *model) shell(line string) tea.Cmd {
	return m.background("Running", 2*time.Minute, func(ctx context.Context) cmdDoneMsg {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		out, err := exec.CommandContext(ctx, sh, "-c", line).CombinedOutput()
		text := strings.TrimRight(string(out), "\n")
		note := fmt.Sprintf("$ %s\n%s", line, text)
		if err != nil {
			if text != "" {
				err = fmt.Errorf("%s\n%w", text, err)
			}
			return cmdDoneMsg{err: err, note: note + "\n" + err.Error()}
		}
		if text == "" {
			text = dimS.Render("(no output)")
		}
		return cmdDoneMsg{out: text, note: note}
	})
}

func (m *model) help(_ []string) tea.Cmd {
	var sb strings.Builder
	sb.WriteString(boldS.Render("Ask in plain English") + " and the model runs AX operations for you, asking before it changes anything.\n\n")
	sb.WriteString(boldS.Render("Commands") + "\n")
	for _, c := range m.cmds {
		name := "/" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		fmt.Fprintf(&sb, "  %s %s\n", accentS.Render(fmt.Sprintf("%-30s", name)), dimS.Render(c.desc))
	}
	sb.WriteString("\n" + boldS.Render("Shortcuts") + "\n")
	for _, kv := range [][2]string{
		{"!<command>", "Run a local shell command"},
		{"tab", "Complete commands and resource names"},
		{"↑ / ↓", "Browse history"},
		{"\\ + enter, ctrl+j", "Insert a newline"},
		{"esc", "Interrupt the model"},
		{"ctrl+c twice", "Exit"},
	} {
		fmt.Fprintf(&sb, "  %s %s\n", accentS.Render(fmt.Sprintf("%-30s", kv[0])), dimS.Render(kv[1]))
	}
	m.say(strings.TrimRight(sb.String(), "\n"))
	return nil
}

func (m *model) showContext(_ []string) tea.Cmd {
	t := m.ax.Target
	rows := [][2]string{
		{"Context", orNone(t.Context)},
		{"Namespace", t.Namespace},
		{"Server", t.Server + dimS.Render(" ("+t.Via+")")},
		{"Atespace", m.agent.Atespace()},
		{"Model", m.agent.Model()},
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%-10s %s", r[0]+":", r[1]))
	}
	m.say(strings.Join(lines, "\n"))
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// refreshNames reloads resource names for completion in the background.
func (m *model) refreshNames() tea.Cmd {
	atespace := m.agent.Atespace()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		names := namesMsg{}
		for _, k := range kinds {
			if n, err := m.ax.Names(ctx, k, atespace); err == nil {
				sort.Strings(n)
				names[k] = n
			}
		}
		return names
	}
}

type namesMsg map[string][]string

// renderTable renders t with a dim header and colored phases.
func renderTable(t ax.Table) string {
	if len(t.Rows) == 0 {
		return dimS.Render("No resources found.")
	}
	widths := make([]int, len(t.Header))
	for i, h := range t.Header {
		widths[i] = len(h)
	}
	for _, r := range t.Rows {
		for i, c := range r {
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	line := func(cells []string, style func(i int, c string) string) string {
		var sb strings.Builder
		for i, c := range cells {
			padded := c
			if i < len(cells)-1 {
				padded += strings.Repeat(" ", widths[i]-lipgloss.Width(c)+3)
			}
			sb.WriteString(style(i, padded))
		}
		return sb.String()
	}
	out := []string{line(t.Header, func(_ int, c string) string { return dimS.Render(c) })}
	for _, r := range t.Rows {
		out = append(out, line(r, func(i int, c string) string {
			if i == t.StatusColumn {
				return phaseStyle(strings.TrimSpace(c)).Render(c)
			}
			return c
		}))
	}
	return strings.Join(out, "\n")
}

// colorPhaseField colors the value of a "Phase:" line in describe output.
func colorPhaseField(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if rest, ok := strings.CutPrefix(l, "Phase:"); ok {
			lines[i] = "Phase:" + strings.Repeat(" ", len(rest)-len(strings.TrimLeft(rest, " "))) + phaseStyle(strings.TrimSpace(rest)).Render(strings.TrimSpace(rest))
		}
	}
	return strings.Join(lines, "\n")
}
