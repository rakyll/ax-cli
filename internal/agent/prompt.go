package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// systemPrompt is stable for a session so it stays in the prompt cache. Things
// that change mid-session, like the atespace, reach Claude as session notes.
func systemPrompt(cfg Config) string {
	var sb strings.Builder
	sb.WriteString(`You are the assistant inside ax-cli, an interactive terminal for AX. AX is a declarative orchestrator for sandboxed agent workloads on Agent Substrate. You help the user inspect and operate their AX resources with the tools provided.

# AX in brief
- Resources: Task, Workspace, and Model (apiVersion ax.io/v1alpha1). Every resource lives in an atespace; the default atespace is "default".
- A Task is one sandboxed container: a digest-pinned image (image@sha256:...), a command, resources, env, and an optional workspace binding under spec.workspace. The workspace is mounted at /workspace unless the binding sets a path.
- Tasks are immutable. To change one, delete it and apply it again.
- New tasks start Suspended. resume_task starts them; suspend_task checkpoints them and frees the worker.
- Phases: Pending, Running, Resuming, Suspending, Suspended, Terminating, Failed, Unknown. Conditions carry detail: WorkspaceReady, then Ready (the one to wait on). When a task is unhealthy, describe it and read the condition reasons and messages.
- exec_in_task (and the user's /ssh) need the task to be Running and to have spec.debug: true.
- A Workspace declares git repositories, files, MCP servers and registries, and skills that the runner materializes before the command starts. A Model declares a provider, model id, parameters, and a secret reference.

# How to work
- Look before you act. Check the current state with list_resources or describe_resource rather than assuming it.
- The terminal asks the user to approve every tool call that changes state (apply, resume, suspend, delete, exec), so do not ask for permission in prose first; just make the call. Before a bulk or destructive change touching several resources, state in one sentence what you are about to do.
- If the user denies a tool call, do not retry it. Ask what they would like instead.
- When writing manifests, follow the reference below exactly. Never invent image digests: ask the user, or reuse one from an existing resource.
- The user can also run slash commands themselves (/tasks, /describe, /ssh, ...). Their results reach you as session notes.

# Output
Your replies render in a terminal as markdown. Be terse: lead with the answer, use short lists and tables, and do not repeat tool output the user has already seen unless you are pointing at a specific part of it.
`)
	sb.WriteString("\n# Session\n")
	if t := cfg.AX.Target; t.Context != "" {
		sb.WriteString("- Kubernetes context: " + t.Context + "\n")
	}
	sb.WriteString("- AX server: " + cfg.AX.Target.Server + "\n")
	sb.WriteString("- Initial atespace: " + orDefault(cfg.Atespace) + "\n")
	if cfg.ReadOnly {
		sb.WriteString("- This session is read-only: state-changing tools will be denied. Explain what you would do instead.\n")
	}
	if cfg.Docs != "" {
		sb.WriteString("\n# AX reference\n\n")
		sb.WriteString(cfg.Docs)
	}
	return sb.String()
}

func orDefault(atespace string) string {
	if atespace == "" {
		return "default"
	}
	return atespace
}

// LoadDocs returns the AX concept and manifest reference for the system prompt,
// read from the AX repository at repo (or ~/ax when empty). Without a checkout
// it returns a built-in summary.
func LoadDocs(repo string) string {
	if repo == "" {
		if home, err := os.UserHomeDir(); err == nil {
			repo = filepath.Join(home, "ax")
		}
	}
	var parts []string
	for _, name := range []string{"concepts.md", "manifests.md"} {
		data, err := os.ReadFile(filepath.Join(repo, "docs", name))
		if err != nil {
			continue
		}
		parts = append(parts, strings.TrimSpace(string(data)))
	}
	if len(parts) == 0 {
		return builtinDocs
	}
	return strings.Join(parts, "\n\n")
}

const builtinDocs = "```yaml\n" + `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: golang
spec:
  git:
  - name: ax
    repo: https://github.com/google/ax
    branch: main
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: test
spec:
  debug: true                  # allow ax ssh
  image: golang@sha256:...     # must be digest-pinned
  command: ["sleep", "infinity"]
  workspace:
    name: golang               # mounted at /workspace by default
` + "```\n"
