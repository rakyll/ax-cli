// Command ax-cli is an interactive terminal for AX: ask Claude to operate your
// tasks, workspaces, and models in plain English, or drive them directly with
// slash commands.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/google/ax-cli/internal/agent"
	"github.com/google/ax-cli/internal/ax"
	"github.com/google/ax-cli/internal/tui"
)

const usage = `ax-cli: an interactive terminal for AX.

Usage:
  ax-cli [flags]              start the interactive terminal
  ax-cli [flags] -p "prompt"  answer one prompt and exit

Flags:
  -a, --atespace NAME   atespace to start in (default "default")
  -n, --namespace NS    namespace of the AX control plane (default "ax-system")
      --context NAME    kubeconfig context
      --server URL      AX server URL (default: $AX_SERVER, else a tunnel)
      --model ID        Claude model (default: $AX_CLI_MODEL, $ANTHROPIC_MODEL,
                        $ANTHROPIC_DEFAULT_OPUS_MODEL, else ` + agent.DefaultModel + `)
      --ax-binary PATH  ax CLI used for tunnels and ssh (default "ax")
      --ax-repo DIR     AX checkout whose docs Claude reads (default ~/ax)
  -p, --print PROMPT    answer one prompt non-interactively and exit
      --allow-changes   with -p, let Claude change resources without asking

Claude is reached with $ANTHROPIC_API_KEY, or through Vertex AI when
$CLAUDE_CODE_USE_VERTEX is set (with $ANTHROPIC_VERTEX_PROJECT_ID and
$CLOUD_ML_REGION).
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ax-cli:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		atespace, namespace, kubeContext, server string
		model, axBinary, axRepo, prompt          string
		allowChanges                             bool
	)
	fs := flag.NewFlagSet("ax-cli", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	for _, name := range []string{"a", "atespace"} {
		fs.StringVar(&atespace, name, "default", "")
	}
	for _, name := range []string{"n", "namespace"} {
		fs.StringVar(&namespace, name, "", "")
	}
	for _, name := range []string{"p", "print"} {
		fs.StringVar(&prompt, name, "", "")
	}
	fs.StringVar(&kubeContext, "context", "", "")
	fs.StringVar(&server, "server", "", "")
	fs.StringVar(&model, "model", defaultModel(), "")
	fs.StringVar(&axBinary, "ax-binary", envOr("AX_BINARY", "ax"), "")
	fs.StringVar(&axRepo, "ax-repo", os.Getenv("AX_REPO"), "")
	fs.BoolVar(&allowChanges, "allow-changes", false, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q; use -p to pass a prompt", fs.Args())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	interactive := prompt == ""
	if interactive {
		fmt.Fprintln(os.Stderr, "Connecting to AX…")
	}
	target, err := ax.Resolve(ctx, ax.ResolveOptions{Server: server, Context: kubeContext, Namespace: namespace, AXBinary: axBinary})
	if err != nil {
		return err
	}
	client, err := ax.Dial(target, axBinary)
	if err != nil {
		return err
	}
	defer client.Close()

	// Not ctx: the client outlives the signal handler, which stops once the
	// terminal takes over Ctrl+C, and Vertex auth refreshes tokens with it.
	claude, fallbacks, err := newClaude(context.Background())
	if err != nil {
		return err
	}
	a := agent.New(agent.Config{
		Client:          claude,
		Model:           model,
		AX:              client,
		Atespace:        atespace,
		Docs:            agent.LoadDocs(axRepo),
		ServerFallbacks: fallbacks,
		ReadOnly:        !interactive && !allowChanges,
	})

	if !interactive {
		return printMode(ctx, a, prompt, os.Stdout)
	}
	stop() // the terminal handles Ctrl+C itself

	opts := tui.Options{Agent: a, AX: client, Dark: lipgloss.HasDarkBackground()}
	fmt.Fprint(os.Stderr, "\x1b[1A\x1b[2K") // clear "Connecting to AX…"
	fmt.Println(tui.Banner(opts))
	return tui.Run(opts)
}

// newClaude returns a Claude client and whether server-side fallbacks are
// available, which they are only on the Claude API.
func newClaude(ctx context.Context) (anthropic.Client, bool, error) {
	if truthy(os.Getenv("CLAUDE_CODE_USE_VERTEX")) {
		project := os.Getenv("ANTHROPIC_VERTEX_PROJECT_ID")
		if project == "" {
			return anthropic.Client{}, false, fmt.Errorf("CLAUDE_CODE_USE_VERTEX is set but ANTHROPIC_VERTEX_PROJECT_ID is not")
		}
		region := envOr("CLOUD_ML_REGION", "global")
		return anthropic.NewClient(vertex.WithGoogleAuth(ctx, region, project)), false, nil
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		return anthropic.Client{}, false, fmt.Errorf("set ANTHROPIC_API_KEY, or CLAUDE_CODE_USE_VERTEX=1 with ANTHROPIC_VERTEX_PROJECT_ID to use Vertex AI")
	}
	return anthropic.NewClient(), true, nil
}

// printMode answers one prompt, streaming the reply to w. Tool activity goes
// to stderr so stdout holds only Claude's answer.
func printMode(ctx context.Context, a *agent.Agent, prompt string, w io.Writer) error {
	events := make(chan agent.Event, 64)
	go a.Run(ctx, prompt, events)
	verbose := term.IsTerminal(int(os.Stderr.Fd()))
	midText := false
	for ev := range events {
		switch ev := ev.(type) {
		case agent.TextDelta:
			fmt.Fprint(w, ev.Text)
			midText = true
		case agent.BlockDone:
			if midText {
				fmt.Fprintln(w)
				midText = false
			}
		case agent.ToolStart:
			if verbose {
				fmt.Fprintf(os.Stderr, "⏺ %s(%s)\n", ev.Name, ev.Summary)
			}
		case agent.PermissionRequest:
			// Only reached with --allow-changes, which pre-approves.
			ev.Reply <- agent.Allow
		case agent.TurnDone:
			if ev.Interrupted {
				return fmt.Errorf("interrupted")
			}
			return ev.Err
		}
	}
	return nil
}

// defaultModel honors ax-cli's own variable first, then the ones Claude Code
// reads, so a Vertex project pinned to particular model versions just works.
func defaultModel() string {
	for _, k := range []string{"AX_CLI_MODEL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return agent.DefaultModel
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
