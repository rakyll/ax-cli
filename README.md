# ax-cli

An interactive terminal for [AX](../ax), in the style of Claude Code. Ask in
plain English and Claude inspects and operates your tasks, workspaces, and
models, asking before it changes anything. Slash commands drive AX directly,
without Claude.

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
`$CLOUD_ML_REGION`, default `global`). The model is `--model`, else
`$AX_CLI_MODEL`, `$ANTHROPIC_MODEL`, `$ANTHROPIC_DEFAULT_OPUS_MODEL`, else
`claude-opus-5`, so an existing Claude Code setup carries over.

## Using it

| Input | Does |
|---|---|
| plain English | Claude answers, calling AX tools as needed |
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
newline. Esc interrupts Claude; Ctrl+C twice exits.

What you do with slash commands and `!` is passed to Claude as context, so you
can run `/describe task x` and then ask "why is that failing?".

### What Claude can do

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

Claude reads AX's `docs/concepts.md` and `docs/manifests.md` from `--ax-repo`
(default `~/ax`) so it writes valid manifests.

### One-shot mode

```sh
ax-cli -p "which tasks are failing and why?"
```

prints Claude's answer to stdout. It is read-only unless `--allow-changes` is
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
| `internal/agent` | The Claude tool loop, tool definitions, system prompt |
| `internal/tui` | The Bubbletea terminal |
| `internal/fakeax`, `internal/cmd/fakeax` | In-memory AX server for tests and demos |
