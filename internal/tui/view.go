package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// suggestion is one completion: the full input it produces.
type suggestion struct {
	text   string
	label  string
	desc   string
	submit bool // run immediately when accepted with enter
}

const maxMenu = 8

// updateMenu recomputes completions for the current input.
func (m *model) updateMenu() {
	m.menu, m.menuSel = m.suggestions(m.ta.Value()), 0
}

func (m *model) suggestions(v string) []suggestion {
	if !strings.HasPrefix(v, "/") || strings.Contains(v, "\n") {
		return nil
	}
	fields := strings.Fields(v[1:])
	trailing := strings.HasSuffix(v, " ")

	// Command names.
	if len(fields) == 0 || (len(fields) == 1 && !trailing) {
		prefix := ""
		if len(fields) == 1 {
			prefix = fields[0]
		}
		var out []suggestion
		for _, c := range m.cmds {
			if !strings.HasPrefix(c.name, prefix) {
				continue
			}
			s := suggestion{text: "/" + c.name, label: "/" + c.name, desc: c.desc, submit: true}
			if c.args != "" {
				s.label += " " + c.args
				// Commands whose first argument is required wait for it.
				if strings.HasPrefix(c.args, "<") {
					s.text += " "
					s.submit = false
				}
			}
			out = append(out, s)
		}
		if len(out) == 1 && out[0].text == v {
			return nil
		}
		return out
	}

	// Arguments.
	c := findCommand(m.cmds, fields[0])
	if c == nil || c.complete == nil {
		return nil
	}
	args := fields[1:]
	token := ""
	if !trailing && len(args) > 0 {
		token = args[len(args)-1]
		args = args[:len(args)-1]
	}
	base := strings.TrimSuffix(v, token)
	var out []suggestion
	for _, cand := range c.complete(m, len(args), args, token) {
		if !strings.HasPrefix(cand, token) || cand == token {
			continue
		}
		s := suggestion{text: base + cand, label: cand, submit: true}
		if !strings.HasSuffix(cand, "/") {
			s.text += " "
		} else {
			s.submit = false
		}
		out = append(out, s)
	}
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

func (m *model) View() string {
	if m.quitting {
		return ""
	}
	w := max(m.width, 20)
	var parts []string

	for _, t := range m.tools {
		parts = append(parts, gutter(m.spin.View()+" ", toolHeader(t)))
	}
	if m.thinking != "" && m.text == "" {
		parts = append(parts, gutter(dimS.Render("✻ "), thinkS.Render(lastLines(m.thinking, 3, w-4))))
	}
	if strings.TrimSpace(m.text) != "" {
		mark := "⏺ "
		if m.textStarted {
			mark = "  "
		}
		parts = append(parts, gutter(mark, lipgloss.NewStyle().Width(w-3).Render(strings.TrimSpace(m.text))))
	}

	if m.perm != nil {
		parts = append(parts, m.permissionView(w))
		return "\n" + strings.Join(parts, "\n\n") + "\n"
	}

	if m.busy {
		elapsed := int(time.Since(m.busyStart).Seconds())
		hint := fmt.Sprintf("(%ds · esc to interrupt)", elapsed)
		if m.tokensOut > 0 && m.turn {
			hint = fmt.Sprintf("(%ds · %s tokens · esc to interrupt)", elapsed, compact(m.tokensIn+m.tokensOut))
		}
		parts = append(parts, m.spin.View()+" "+accentS.Render(m.busyLabel+"…")+" "+dimS.Render(hint))
	}
	for _, q := range m.queue {
		parts = append(parts, dimS.Render("  queued: "+firstLine(q)))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(subtle).
		Width(w - 2).
		Render(m.ta.View())

	var sb strings.Builder
	if len(parts) > 0 {
		sb.WriteString("\n" + strings.Join(parts, "\n") + "\n")
	}
	sb.WriteString("\n" + box + "\n")
	if len(m.menu) > 0 {
		sb.WriteString(m.menuView(w))
	} else {
		sb.WriteString(m.statusLine(w))
	}
	return sb.String()
}

func (m *model) menuView(w int) string {
	start := 0
	if m.menuSel >= maxMenu {
		start = m.menuSel - maxMenu + 1
	}
	end := min(start+maxMenu, len(m.menu))
	labelW := 0
	for _, s := range m.menu[start:end] {
		labelW = max(labelW, lipgloss.Width(s.label))
	}
	var lines []string
	for i := start; i < end; i++ {
		s := m.menu[i]
		label := fmt.Sprintf("%-*s", labelW, s.label)
		line := "  " + label
		if s.desc != "" {
			line += "  " + s.desc
		}
		line = truncateWidth(line, w-1)
		if i == m.menuSel {
			lines = append(lines, accentS.Render(line))
		} else {
			lines = append(lines, dimS.Render(line))
		}
	}
	if len(m.menu) > end {
		lines = append(lines, dimS.Render(fmt.Sprintf("  … %d more", len(m.menu)-end)))
	}
	return strings.Join(lines, "\n")
}

func (m *model) statusLine(w int) string {
	if m.flash != "" {
		return "  " + dimS.Render(m.flash)
	}
	t := m.ax.Target
	left := []string{accentS.Render("◉ " + m.agent.Atespace())}
	if t.Context != "" {
		left = append(left, t.Context)
	}
	left = append(left, strings.TrimPrefix(t.Server, "http://"))
	l := "  " + strings.Join(left, dimS.Render(" · "))
	r := m.agent.Model()
	if m.tokensIn+m.tokensOut > 0 {
		r += " · " + compact(m.tokensIn+m.tokensOut) + " tokens"
	}
	r = dimS.Render(r) + "  "
	gap := w - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		return truncateWidth(l, w)
	}
	return dimS.Render(l) + strings.Repeat(" ", gap) + r
}

func (m *model) permissionView(w int) string {
	p := m.perm
	label := toolLabels[p.Tool]
	if label == "" {
		label = p.Tool
	}
	inner := w - 4
	var b strings.Builder
	b.WriteString(boldS.Render(accentS.Render(p.Title)) + "\n")
	if p.Detail != "" {
		detail := p.Detail
		lines := strings.Split(detail, "\n")
		if len(lines) > 20 {
			detail = strings.Join(lines[:20], "\n") + "\n" + fmt.Sprintf("… %d more lines", len(lines)-20)
		}
		for _, l := range strings.Split(detail, "\n") {
			b.WriteString("\n  " + dimS.Render(truncateWidth(l, inner-4)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nDo you want to proceed?\n")
	options := []string{
		"Yes",
		fmt.Sprintf("Yes, and don't ask again for %s this session", label),
		"No, and tell " + m.agent.Provider() + " what to do differently (esc)",
	}
	for i, o := range options {
		line := fmt.Sprintf("  %d. %s", i+1, o)
		if i == m.permSel {
			line = accentS.Render(fmt.Sprintf("❯ %d. %s", i+1, o))
		}
		b.WriteString(line + "\n")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(pink).
		Padding(0, 1).
		Width(w - 2).
		Render(strings.TrimRight(b.String(), "\n"))
}

// Banner is printed once when the terminal starts.
func Banner(opts Options) string {
	t := opts.AX.Target
	axel := accentS.Render("✻")
	var b strings.Builder
	b.WriteString(axel + " " + boldS.Render("Welcome to AX") + dimS.Render(" · interactive mode") + "\n\n")
	b.WriteString(dimS.Render("  /help for commands, ! for shell, esc to interrupt") + "\n\n")
	row := func(k, v string) { fmt.Fprintf(&b, "  %s %s\n", dimS.Render(fmt.Sprintf("%-9s", k)), v) }
	if t.Context != "" {
		row("context", t.Context)
	}
	row("server", t.Server+dimS.Render(" ("+t.Via+")"))
	row("atespace", opts.Agent.Atespace())
	row("model", opts.Agent.Model())
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(pink).
		Padding(0, 1).
		Render(strings.TrimRight(b.String(), "\n"))
}

func lastLines(s string, n, width int) string {
	wrapped := lipgloss.NewStyle().Width(max(width, 10)).Render(strings.TrimSpace(s))
	lines := strings.Split(wrapped, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	l, _, more := strings.Cut(s, "\n")
	if more {
		l += " …"
	}
	return l
}

func truncateWidth(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > w-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
