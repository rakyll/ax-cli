# ax-cli

An interactive terminal for [AX](../ax), in the style of Claude Code. Ask in
plain English and Claude or Gemini inspects and operates your tasks,
workspaces, and models, asking before it changes anything. Slash commands
drive AX directly, without the model.

```
╭─────────────────────────────────────────────────────╮
│ ✻ Welcome to AX · interactive mode                  │
│   /help for commands, ! for shell, esc to interrupt │
╰─────────────────────────────────────────────────────╯

> why is flaky-tests not ready?

⏺ Describe(task/flaky-tests)
  ⎿  Name:         flaky-tests
     Phase:        Resuming
     …
╭──────────────────────────────────────────────────────╮
│ > Ask about your tasks, or type / for commands       │
╰──────────────────────────────────────────────────────╯
  ◉ default · gke_proj_us-central1_ax · 127.0.0.1:43211    claude-opus-5 · 12.4k tokens
```

## Install

```sh
go build -o ax-cli .
```

ax-cli depends on the public `pkg/apis/v1alpha1` package of AX through a
`replace ../ax` directive, so build it next to an AX checkout.

It also needs the `ax` CLI on `$PATH` (or `--ax-binary`) for two things that
rely on AX's internal packages: starting a port-forward tunnel when none is
running, and `ax ssh`.

## Connecting

To AX, the same way `ax` does:

1. `--server`, or `$AX_SERVER`.
2. Otherwise a healthy tunnel for the kube context, read from
   `~/.ax/tunnels/` (or `$AX_HOME/tunnels/`).
3. Otherwise ax-cli runs `ax get models` once, which starts a tunnel, and
   uses it.

`--context`, `-n/--namespace`, and `-a/--atespace` work as they do in `ax`.

To Claude, with `$ANTHROPIC_API_KEY`, or through Vertex AI when
`$CLAUDE_CODE_USE_VERTEX` is set (with `$ANTHROPIC_VERTEX_PROJECT_ID` and
`$CLOUD_ML_REGION`, default `global`).

To Gemini, through the Gemini API with `$GEMINI_API_KEY` (or
`$GOOGLE_API_KEY`). Any model whose name starts with `gemini` goes to Gemini,
for example `--model gemini-3.1-pro-preview` or `/model gemini-3.5-flash`.

Either provider is enough. The model is `--model`, else `$AX_CLI_MODEL`, else
(with Claude set up) `$ANTHROPIC_MODEL`, `$ANTHROPIC_DEFAULT_OPUS_MODEL`, or
`claude-opus-5`, so an existing Claude Code setup carries over; with only
Gemini set up it is `gemini-3.1-pro-preview`.

`/model` switches models mid-session. Within a provider the conversation
continues; switching between Claude and Gemini starts a new one, since their
histories (thinking, tool calls) do not translate.

## Using it

| Input | Does |
|---|---|
| plain English | The model answers, calling AX tools as needed |
| `/tasks`, `/workspaces`, `/models` | List resources |
| `/get <kind> [name]` | List, or show one resource as YAML |
| `/describe <kind> <name>` | Details and conditions |
| `/apply <file>` | Create or update from a manifest |
| `/resume <task>`, `/suspend <task>` | Lifecycle |
| `/delete <kind> <name>` | Delete |
| `/ssh <task> [-- cmd]` | Hand the terminal to `ax ssh` |
| `/atespace [name]`, `/ctx`, `/model [id]` | Session settings |
| `/clear`, `/help`, `/exit` | |
| `!<command>` | Run a local shell command |

Tab completes commands, kinds, resource names, and manifest paths. ↑/↓ browse
history (kept in `~/.ax/cli_history`). `\` then Enter, or Ctrl+J, inserts a
newline. Esc interrupts the model; Ctrl+C twice exits. Resizing the window
redraws the session at the new width.

What you do with slash commands and `!` is passed to the model as context, so
you can run `/describe task x` and then ask "why is that failing?".

### What the model can do

Read-only tools run without asking: list, get, describe, wait for a phase, and
read or list local files (to find manifests). Tools that change state ask
first, in a dialog:

```
╭───────────────────────────────────────────────────────╮
│ Delete task docs-review                               │
│                                                       │
│   This cannot be undone.                              │
│                                                       │
│ Do you want to proceed?                               │
│ ❯ 1. Yes                                              │
│   2. Yes, and don't ask again for Delete this session │
│   3. No, and tell Claude what to do differently (esc) │
╰───────────────────────────────────────────────────────╯
```

These are apply, resume, suspend, delete, and running a command in a debug
task (`ax ssh <task> -- <cmd>`).

The model reads AX's `docs/concepts.md` and `docs/manifests.md` from `--ax-repo`
(default `~/ax`) so it writes valid manifests.

### One-shot mode

```sh
ax-cli -p "which tasks are failing and why?"
```

prints the model's answer to stdout. It is read-only unless `--allow-changes` is
given, in which case changes are made without asking.

## Trying it without a cluster

`internal/cmd/fakeax` is an in-memory AX control plane with sample resources:

```sh
go run ./internal/cmd/fakeax &              # serves on 127.0.0.1:8080
AX_SERVER=http://127.0.0.1:8080 go run .
```

`/ssh` and command execution need a real cluster.

## Layout

| Path | |
|---|---|
| `main.go` | Flags, connections, one-shot mode |
| `internal/ax` | AX client: server resolution, CRUD, apply, describe, tables, `ax ssh` |
| `internal/agent` | The tool loop, the Claude and Gemini providers, tool definitions, system prompt |
| `internal/tui` | The Bubbletea terminal |
| `internal/fakeax`, `internal/cmd/fakeax` | In-memory AX server for tests and demos |
