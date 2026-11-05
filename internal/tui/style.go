package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// axel pink, from the AX mascot.
var (
	pink    = lipgloss.AdaptiveColor{Light: "#D6336C", Dark: "#FF87AF"}
	gray    = lipgloss.AdaptiveColor{Light: "#8A8A8A", Dark: "#808080"}
	subtle  = lipgloss.AdaptiveColor{Light: "#B0B0B0", Dark: "#4E4E4E"}
	green   = lipgloss.AdaptiveColor{Light: "#2F9E44", Dark: "#69DB7C"}
	yellow  = lipgloss.AdaptiveColor{Light: "#E67700", Dark: "#FFD43B"}
	red     = lipgloss.AdaptiveColor{Light: "#E03131", Dark: "#FF6B6B"}
	blue    = lipgloss.AdaptiveColor{Light: "#1971C2", Dark: "#74C0FC"}
	accentS = lipgloss.NewStyle().Foreground(pink)
	boldS   = lipgloss.NewStyle().Bold(true)
	dimS    = lipgloss.NewStyle().Foreground(gray)
	thinkS  = lipgloss.NewStyle().Foreground(gray).Italic(true)
	errS    = lipgloss.NewStyle().Foreground(red)
	okS     = lipgloss.NewStyle().Foreground(green)
	userS   = lipgloss.NewStyle().Foreground(gray)
)

// phaseStyle colors a task phase.
func phaseStyle(phase string) lipgloss.Style {
	switch phase {
	case "Running":
		return okS
	case "Failed", "Unknown":
		return errS
	case "Suspended":
		return lipgloss.NewStyle().Foreground(blue)
	case "Pending", "Resuming", "Suspending", "Terminating":
		return lipgloss.NewStyle().Foreground(yellow)
	}
	return lipgloss.NewStyle()
}

func newMarkdown(width int, dark bool) *glamour.TermRenderer {
	cfg := styles.LightStyleConfig
	if dark {
		cfg = styles.DarkStyleConfig
	}
	zero := uint(0)
	cfg.Document.Margin = &zero
	cfg.Document.BlockPrefix = ""
	cfg.Document.BlockSuffix = ""
	r, err := glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(max(width-8, 20)))
	if err != nil {
		return nil
	}
	return r
}

// renderMarkdown renders md and trims the blank lines glamour adds around it.
func renderMarkdown(r *glamour.TermRenderer, md string) string {
	if r == nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	lines := strings.Split(out, "\n")
	for len(lines) > 0 && strings.TrimSpace(stripANSI(lines[0])) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(stripANSI(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// gutter prefixes the first line of s with mark and indents the rest to match,
// the way Claude Code lays out messages.
func gutter(mark, s string) string {
	lines := strings.Split(s, "\n")
	pad := strings.Repeat(" ", lipgloss.Width(mark))
	for i, l := range lines {
		if i == 0 {
			lines[i] = mark + l
		} else if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func stripANSI(s string) string {
	var sb strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
