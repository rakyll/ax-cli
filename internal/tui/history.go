package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

const maxHistory = 500

// history is the prompt history, persisted one JSON string per line so
// multi-line prompts survive.
type history struct {
	path    string
	entries []string
	// pos indexes entries while browsing; len(entries) means the draft.
	pos   int
	draft string
}

func historyPath() string {
	dir := os.Getenv("AX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".ax")
	}
	return filepath.Join(dir, "cli_history")
}

func loadHistory(path string) *history {
	h := &history{path: path}
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e string
			if json.Unmarshal(sc.Bytes(), &e) == nil && e != "" {
				h.entries = append(h.entries, e)
			}
		}
	}
	if len(h.entries) > maxHistory {
		h.entries = h.entries[len(h.entries)-maxHistory:]
	}
	h.pos = len(h.entries)
	return h
}

// add records an entry and appends it to the history file.
func (h *history) add(e string) {
	h.pos = len(h.entries)
	if e == "" || (len(h.entries) > 0 && h.entries[len(h.entries)-1] == e) {
		h.pos = len(h.entries)
		return
	}
	h.entries = append(h.entries, e)
	h.pos = len(h.entries)
	if h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(e)
	_, _ = f.Write(append(line, '\n'))
}

// prev moves back in history, saving the current input as the draft when
// browsing starts. It reports false at the oldest entry.
func (h *history) prev(current string) (string, bool) {
	if h.pos == 0 {
		return "", false
	}
	if h.pos == len(h.entries) {
		h.draft = current
	}
	h.pos--
	return h.entries[h.pos], true
}

// next moves forward in history, ending at the saved draft.
func (h *history) next() (string, bool) {
	if h.pos >= len(h.entries) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.pos], true
}
