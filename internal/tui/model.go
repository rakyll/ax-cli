// Package tui is the interactive ax-cli terminal: an inline Bubbletea program
// in the style of Claude Code. Finished output is printed into the terminal's
// scrollback; only the in-progress turn, the input box, and the status line
// are redrawn.
package tui

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/google/ax-cli/internal/agent"
	"github.com/google/ax-cli/internal/ax"
)

// Options configures the terminal.
type Options struct {
	Agent *agent.Agent
	AX    *ax.Client
	// Dark selects the markdown theme; detect it before the program starts.
	Dark bool
}

var errInterrupted = errors.New("interrupted")

const maxNote = 8 << 10

type model struct {
	agent *agent.Agent
	ax    *ax.Client
	dark  bool
	cmds  []*command

	ta    textarea.Model
	spin  spinner.Model
	md    *glamour.TermRenderer
	hist  *history
	names map[string][]string
	width int

	// Work in progress: an agent turn or a slash command.
	busy      bool
	turn      bool
	busyLabel string
	busyStart time.Time
	cancel    context.CancelFunc
	events    chan agent.Event
	queue     []string

	// The streaming assistant turn.
	text        string
	textStarted bool
	thinking    string
	tools       []agent.ToolStart
	perm        *agent.PermissionRequest
	permSel     int

	menu    []suggestion
	menuSel int

	tokensIn, tokensOut int
	lastCtrlC           time.Time
	flash               string
	quitting            bool

	// out collects lines to print above the live area at the end of Update.
	out []string
}

type (
	agentEventMsg struct{ ev agent.Event }
	clearFlashMsg struct{}
)

// Run starts the interactive terminal and blocks until the user exits.
func Run(opts Options) error {
	_, err := tea.NewProgram(newModel(opts)).Run()
	return err
}

func newModel(opts Options) *model {
	ta := textarea.New()
	ta.Placeholder = `Ask about your tasks, or type / for commands`
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetHeight(1)
	ta.Prompt = ""
	ta.SetPromptFunc(2, func(i int) string {
		if i == 0 {
			return "> "
		}
		return "  "
	})
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = dimS
	ta.FocusedStyle.Prompt = accentS
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Spinner{Frames: []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}, FPS: 120 * time.Millisecond}
	sp.Style = accentS

	return &model{
		agent: opts.Agent,
		ax:    opts.AX,
		dark:  opts.Dark,
		cmds:  commands(),
		ta:    ta,
		spin:  sp,
		hist:  loadHistory(historyPath()),
		names: map[string][]string{},
		width: 80,
		md:    newMarkdown(80, opts.Dark),
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.refreshNames())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if len(m.out) == 0 {
		return m, cmd
	}
	// Print before anything else runs so output keeps its order. The
	// renderer counts lines by newline, so nothing may soft-wrap.
	p := tea.Println(ansi.Wrap(strings.Join(m.out, "\n"), max(m.width-1, 20), ""))
	m.out = nil
	return m, tea.Sequence(p, cmd)
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.ta.SetWidth(max(msg.Width-4, 10))
		m.md = newMarkdown(msg.Width, m.dark)
		m.resizeInput()
		return nil

	case tea.KeyMsg:
		return m.key(msg)

	case spinner.TickMsg:
		if !m.busy {
			return nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return cmd

	case agentEventMsg:
		return m.event(msg.ev)

	case cmdDoneMsg:
		m.stopBusy()
		switch {
		case errors.Is(msg.err, errInterrupted):
			m.result(dimS.Render("Interrupted"))
		case msg.err != nil:
			m.result(errS.Render(ax.ErrorMessage(msg.err)))
		case msg.out != "":
			m.result(msg.out)
		}
		if msg.note != "" {
			m.agent.Note("The user ran this themselves; the output was:\n" + truncateNote(msg.note))
		}
		var cmds []tea.Cmd
		if msg.refresh {
			cmds = append(cmds, m.refreshNames())
		}
		cmds = append(cmds, m.dequeue())
		return tea.Batch(cmds...)

	case namesMsg:
		for k, v := range msg {
			m.names[k] = v
		}
		return nil

	case clearFlashMsg:
		m.flash = ""
		return nil
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return cmd
}

func (m *model) key(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		switch {
		case m.busy:
			m.interrupt()
		case m.ta.Value() != "":
			m.setInput("")
		case time.Since(m.lastCtrlC) < 2*time.Second:
			m.quitting = true
			return tea.Quit
		default:
			m.lastCtrlC = time.Now()
			return m.setFlash("Press Ctrl+C again to exit")
		}
		return nil
	}

	if m.perm != nil {
		return m.permissionKey(msg)
	}

	// Several runes in one message are typed-ahead or pasted text. Insert
	// them literally: their String() would otherwise match key names, so
	// pasting "delete" or "up" would act as those keys.
	if msg.Type == tea.KeyRunes && (len(msg.Runes) > 1 || msg.Paste) {
		m.ta.InsertString(string(msg.Runes))
		m.resizeInput()
		m.updateMenu()
		return nil
	}

	switch msg.String() {
	case "esc":
		switch {
		case len(m.menu) > 0:
			m.menu = nil
		case m.busy:
			m.interrupt()
		}
		return nil
	case "ctrl+d":
		if m.ta.Value() == "" {
			m.quitting = true
			return tea.Quit
		}
	case "enter":
		v := m.ta.Value()
		if strings.HasSuffix(v, `\`) {
			m.ta.SetValue(strings.TrimSuffix(v, `\`) + "\n")
			m.resizeInput()
			return nil
		}
		if len(m.menu) > 0 {
			s := m.menu[m.menuSel]
			m.setInput(s.text)
			if !s.submit {
				return nil
			}
		}
		return m.submit()
	case "tab":
		if len(m.menu) > 0 {
			m.setInput(m.menu[m.menuSel].text)
		}
		return nil
	case "up", "ctrl+p":
		if len(m.menu) > 0 {
			m.menuSel = (m.menuSel - 1 + len(m.menu)) % len(m.menu)
			return nil
		}
		if m.ta.Line() == 0 {
			if e, ok := m.hist.prev(m.ta.Value()); ok {
				m.setInput(e)
				m.menu = nil
			}
			return nil
		}
	case "down", "ctrl+n":
		if len(m.menu) > 0 {
			m.menuSel = (m.menuSel + 1) % len(m.menu)
			return nil
		}
		if m.ta.Line() == m.ta.LineCount()-1 {
			if e, ok := m.hist.next(); ok {
				m.setInput(e)
				m.menu = nil
			}
			return nil
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.resizeInput()
	m.updateMenu()
	return cmd
}

func (m *model) permissionKey(msg tea.KeyMsg) tea.Cmd {
	choice := -1
	switch msg.String() {
	case "up", "k", "shift+tab":
		m.permSel = (m.permSel + 2) % 3
	case "down", "j", "tab":
		m.permSel = (m.permSel + 1) % 3
	case "1", "y":
		choice = 0
	case "2":
		choice = 1
	case "3", "n", "esc":
		choice = 2
	case "enter":
		choice = m.permSel
	}
	if choice < 0 {
		return nil
	}
	reply := m.perm.Reply
	m.perm = nil
	switch choice {
	case 0:
		reply <- agent.Allow
	case 1:
		reply <- agent.AllowAlways
	default:
		// Like Claude Code: deny, stop the turn, and let the user say what
		// to do instead.
		reply <- agent.Deny
		m.interrupt()
	}
	return nil
}

// submit handles the input line.
func (m *model) submit() tea.Cmd {
	line := strings.TrimSpace(m.ta.Value())
	m.setInput("")
	m.menu = nil
	if line == "" {
		return nil
	}
	m.hist.add(line)
	if m.busy {
		m.queue = append(m.queue, line)
		return nil
	}
	return m.dispatch(line)
}

func (m *model) dispatch(line string) tea.Cmd {
	switch {
	case strings.HasPrefix(line, "/"):
		m.print(gutter(userS.Render("> "), userS.Render(line)))
		fields := strings.Fields(line[1:])
		if len(fields) == 0 {
			return m.help(nil)
		}
		c := findCommand(m.cmds, fields[0])
		if c == nil {
			m.result(errS.Render(fmt.Sprintf("Unknown command /%s. Type /help for commands.", fields[0])))
			return nil
		}
		return c.run(m, fields[1:])
	case strings.HasPrefix(line, "!"):
		m.print(gutter(accentS.Render("! "), userS.Render(strings.TrimSpace(line[1:]))))
		return m.shell(strings.TrimSpace(line[1:]))
	}
	m.print(gutter(userS.Render("> "), userS.Render(line)))
	return m.startTurn(line)
}

func (m *model) dequeue() tea.Cmd {
	if len(m.queue) == 0 || m.busy {
		return nil
	}
	line := m.queue[0]
	m.queue = m.queue[1:]
	return m.dispatch(line)
}

var verbs = []string{"Regenerating", "Gilling", "Paddling", "Sandboxing", "Wiggling", "Orchestrating", "Checkpointing", "Pondering", "Swishing", "Blooping"}

func (m *model) startTurn(prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.startBusy(verbs[rand.IntN(len(verbs))], cancel)
	m.turn = true
	m.events = make(chan agent.Event, 64)
	go m.agent.Run(ctx, prompt, m.events)
	return tea.Batch(m.spin.Tick, waitEvent(m.events))
}

func waitEvent(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg { return agentEventMsg{<-ch} }
}

func (m *model) startBusy(label string, cancel context.CancelFunc) {
	m.busy, m.busyLabel, m.busyStart, m.cancel = true, label, time.Now(), cancel
}

func (m *model) stopBusy() {
	if m.cancel != nil {
		m.cancel()
	}
	m.busy, m.turn, m.cancel = false, false, nil
}

func (m *model) interrupt() {
	if m.perm != nil {
		m.perm.Reply <- agent.Deny
		m.perm = nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.busyLabel = "Interrupting"
	m.queue = nil
}

// event applies one agent event.
func (m *model) event(ev agent.Event) tea.Cmd {
	next := waitEvent(m.events)
	switch ev := ev.(type) {
	case agent.ThinkingDelta:
		m.thinking += ev.Text
	case agent.TextDelta:
		m.flushThinking()
		m.text += ev.Text
		if done, rest := splitStable(m.text); done != "" {
			m.printText(done)
			m.text = rest
		}
	case agent.BlockDone:
		m.flushThinking()
		m.flushText()
	case agent.ToolStart:
		m.flushThinking()
		m.flushText()
		m.tools = append(m.tools, ev)
	case agent.ToolDone:
		start := agent.ToolStart{ID: ev.ID, Name: ev.Name}
		for i, t := range m.tools {
			if t.ID == ev.ID {
				start = t
				m.tools = append(m.tools[:i], m.tools[i+1:]...)
				break
			}
		}
		m.printTool(start, ev)
	case agent.PermissionRequest:
		m.perm, m.permSel = &ev, 0
	case agent.TurnDone:
		m.flushThinking()
		m.flushText()
		m.tools = nil
		m.perm = nil
		m.tokensIn += int(ev.InputTokens + ev.CacheReadTokens)
		m.tokensOut += int(ev.OutputTokens)
		switch {
		case ev.Interrupted:
			m.result(dimS.Render("Interrupted · What should Claude do instead?"))
		case ev.Err != nil:
			wrapped := lipgloss.NewStyle().Width(max(m.width-4, 20)).Render(ev.Err.Error())
			m.print(gutter(errS.Render("⏺ "), errS.Render(wrapped)))
		}
		m.stopBusy()
		return tea.Batch(m.refreshNames(), m.dequeue())
	}
	return next
}

func (m *model) flushThinking() {
	t := strings.TrimSpace(m.thinking)
	m.thinking = ""
	if t == "" {
		return
	}
	wrapped := lipgloss.NewStyle().Width(max(m.width-4, 20)).Render(t)
	m.print(gutter(dimS.Render("✻ "), thinkS.Render(wrapped)))
}

func (m *model) flushText() {
	if strings.TrimSpace(m.text) != "" {
		m.printText(m.text)
	}
	m.text = ""
	m.textStarted = false
}

func (m *model) printText(md string) {
	r := renderMarkdown(m.md, md)
	if m.textStarted {
		m.out = append(m.out, "", gutter("  ", r))
		return
	}
	m.textStarted = true
	m.print(gutter("⏺ ", r))
}

// toolLabels are the display names of the agent's tools.
var toolLabels = map[string]string{
	"list_resources":    "List",
	"get_resource":      "Get",
	"describe_resource": "Describe",
	"apply_manifest":    "Apply",
	"resume_task":       "Resume",
	"suspend_task":      "Suspend",
	"delete_resource":   "Delete",
	"exec_in_task":      "Exec",
	"wait_for_task":     "Wait",
	"read_local_file":   "Read",
	"list_local_files":  "List files",
}

func toolHeader(t agent.ToolStart) string {
	label := toolLabels[t.Name]
	if label == "" {
		label = t.Name
	}
	return boldS.Render(label) + "(" + t.Summary + ")"
}

const toolPreviewLines = 12

func (m *model) printTool(start agent.ToolStart, done agent.ToolDone) {
	mark := okS.Render("⏺ ")
	out := strings.TrimRight(done.Output, "\n")
	switch {
	case strings.HasPrefix(out, "The user denied"):
		mark, out = errS.Render("⏺ "), errS.Render("Denied")
	case strings.HasPrefix(out, "Interrupted"):
		mark, out = dimS.Render("⏺ "), dimS.Render("Interrupted")
	case strings.HasPrefix(out, "This session is read-only"):
		mark, out = errS.Render("⏺ "), errS.Render("Not allowed in a read-only session")
	default:
		lines := strings.Split(out, "\n")
		more := 0
		if len(lines) > toolPreviewLines {
			more = len(lines) - toolPreviewLines
			lines = lines[:toolPreviewLines]
		}
		out = strings.Join(lines, "\n")
		if start.Name == "list_resources" && !done.IsError {
			out = colorPhases(out)
		}
		if done.IsError {
			mark, out = errS.Render("⏺ "), errS.Render(out)
		} else {
			out = dimS.Render(out)
		}
		if more > 0 {
			out += "\n" + dimS.Render(fmt.Sprintf("… +%d lines", more))
		}
	}
	m.print(gutter(mark, toolHeader(start)) + "\n" + gutter(dimS.Render("  ⎿  "), m.wrap(out, 5)))
}

// colorPhases colors known phase words in a task table.
func colorPhases(s string) string {
	for _, p := range []string{"Running", "Suspended", "Failed", "Pending", "Resuming", "Suspending", "Terminating"} {
		s = strings.ReplaceAll(s, "   "+p+"   ", "   "+phaseStyle(p).Render(p)+"   ")
	}
	return s
}

// print queues a block for the scrollback, separated by a blank line.
func (m *model) print(s string) {
	m.out = append(m.out, "", s)
}

// result prints a command result under the line that ran it.
func (m *model) result(s string) {
	m.out = append(m.out, gutter(dimS.Render("  ⎿  "), m.wrap(s, 5)))
}

// wrap wraps s to fit beside a gutter of the given width.
func (m *model) wrap(s string, gutter int) string {
	return ansi.Wrap(s, max(m.width-gutter-1, 20), "")
}

// say prints a synchronous command's output.
func (m *model) say(s string) { m.result(s) }

func (m *model) setFlash(s string) tea.Cmd {
	m.flash = s
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return clearFlashMsg{} })
}

func (m *model) setInput(s string) {
	m.ta.SetValue(s)
	m.ta.CursorEnd()
	m.resizeInput()
	m.updateMenu()
}

func (m *model) resizeInput() {
	w := max(m.ta.Width()-2, 1)
	n := 0
	for _, l := range strings.Split(m.ta.Value(), "\n") {
		n += max(1, (lipgloss.Width(l)+w)/w)
	}
	m.ta.SetHeight(min(max(n, 1), 10))
}

func truncateNote(s string) string {
	if len(s) <= maxNote {
		return s
	}
	return s[:maxNote] + "\n… (truncated)"
}

// splitStable splits streamed markdown into complete paragraphs that can be
// printed now and a tail still being written. It never splits inside a code
// fence.
func splitStable(s string) (done, rest string) {
	cut := -1
	inFence := false
	for i := 0; i < len(s); {
		lineEnd := strings.IndexByte(s[i:], '\n')
		if lineEnd < 0 {
			break
		}
		line := s[i : i+lineEnd]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		i += lineEnd + 1
		if !inFence && strings.TrimSpace(line) == "" && i > 1 {
			cut = i
		}
	}
	if cut <= 0 {
		return "", s
	}
	if strings.TrimSpace(s[:cut]) == "" {
		return "", s
	}
	return s[:cut], s[cut:]
}
