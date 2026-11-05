package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"

	"github.com/google/ax-cli/internal/ax"
)

// tool is one capability offered to Claude.
type tool struct {
	name        string
	description string
	properties  map[string]any
	required    []string
	// mutating tools change cluster state and need the user's approval.
	mutating bool
	// summary is the short argument description shown next to the tool name.
	summary func(*call) string
	// prompt describes a mutating call for the approval dialog.
	prompt func(*call) (title, detail string)
	run    func(context.Context, *call) (string, error)
}

// call is one invocation of a tool.
type call struct {
	in       input
	atespace string
	ax       *ax.Client
}

const maxToolOutput = 30_000

var (
	kindProp = map[string]any{
		"type": "string", "enum": []string{"task", "workspace", "model"},
		"description": "Resource kind.",
	}
	nameProp     = map[string]any{"type": "string", "description": "Resource name."}
	taskNameProp = map[string]any{"type": "string", "description": "Task name."}
	atespaceProp = map[string]any{"type": "string", "description": "Atespace to use. Omit to use the session atespace."}
)

func axTools() []*tool {
	return []*tool{
		{
			name:        "list_resources",
			description: "List every task, workspace, or model in an atespace, in the table format of `ax get`. Tasks include their phase, actor, worker IP, and age.",
			properties:  map[string]any{"kind": kindProp, "atespace": atespaceProp},
			required:    []string{"kind"},
			summary:     func(c *call) string { return plural(c.in.str("kind")) + inAtespace(c) },
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				t, err := c.ax.List(ctx, c.in.str("kind"), c.atespace)
				if err != nil {
					return "", err
				}
				return t.Plain(), nil
			},
		},
		{
			name:        "get_resource",
			description: "Get one resource's full manifest and live status as YAML, like `ax get <kind> <name>`.",
			properties:  map[string]any{"kind": kindProp, "name": nameProp, "atespace": atespaceProp},
			required:    []string{"kind", "name"},
			summary:     func(c *call) string { return c.in.str("kind") + "/" + c.in.str("name") + inAtespace(c) },
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				m, err := c.ax.Get(ctx, c.in.str("kind"), c.atespace, c.in.str("name"))
				if err != nil {
					return "", err
				}
				return ax.YAML(m), nil
			},
		},
		{
			name:        "describe_resource",
			description: "Show a human-readable summary of one resource, like `ax describe`. For tasks this includes phase, conditions with reasons and messages, workspace binding, image, and config. Start here when diagnosing a task.",
			properties:  map[string]any{"kind": kindProp, "name": nameProp, "atespace": atespaceProp},
			required:    []string{"kind", "name"},
			summary:     func(c *call) string { return c.in.str("kind") + "/" + c.in.str("name") + inAtespace(c) },
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				return c.ax.Describe(ctx, c.in.str("kind"), c.atespace, c.in.str("name"))
			},
		},
		{
			name: "apply_manifest",
			description: "Create or update resources from a YAML manifest (one or more documents separated by ---), like `ax apply -f`. " +
				"Workspaces and models are created or updated in place. Tasks are immutable: applying a task that already exists fails, so delete it first to change it. " +
				"New tasks start Suspended; call resume_task to start one.",
			properties: map[string]any{
				"manifest": map[string]any{"type": "string", "description": "The full YAML manifest."},
				"atespace": atespaceProp,
			},
			required: []string{"manifest"},
			mutating: true,
			summary:  func(c *call) string { return manifestSummary(c.in.str("manifest")) },
			prompt: func(c *call) (string, string) {
				return "Apply " + manifestSummary(c.in.str("manifest")) + inAtespace(c), strings.TrimSpace(c.in.str("manifest"))
			},
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				results, err := c.ax.Apply(ctx, []byte(c.in.str("manifest")), c.atespace)
				var lines []string
				for _, r := range results {
					lines = append(lines, r.String())
				}
				if err != nil {
					lines = append(lines, "error: "+ax.ErrorMessage(err))
					return strings.Join(lines, "\n"), fmt.Errorf("%s", strings.Join(lines, "\n"))
				}
				return strings.Join(lines, "\n"), nil
			},
		},
		{
			name:        "resume_task",
			description: "Resume a Suspended task: schedule its actor on a worker and start it, restoring any checkpoint. The first resume also waits for the workspace to initialize, which can take minutes. Returns the task's status afterwards.",
			properties:  map[string]any{"name": taskNameProp, "atespace": atespaceProp},
			required:    []string{"name"},
			mutating:    true,
			summary:     func(c *call) string { return c.in.str("name") + inAtespace(c) },
			prompt: func(c *call) (string, string) {
				return "Resume task " + c.in.str("name") + inAtespace(c), "Schedules the task's actor on a worker and starts it."
			},
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				t, err := c.ax.Resume(ctx, c.atespace, c.in.str("name"))
				if err != nil {
					return "", err
				}
				return "task.ax.io/" + c.in.str("name") + " resumed\n\n" + ax.DescribeTask(t), nil
			},
		},
		{
			name:        "suspend_task",
			description: "Suspend a Running task: checkpoint its sandbox state and free its worker. It can be resumed later exactly where it left off.",
			properties:  map[string]any{"name": taskNameProp, "atespace": atespaceProp},
			required:    []string{"name"},
			mutating:    true,
			summary:     func(c *call) string { return c.in.str("name") + inAtespace(c) },
			prompt: func(c *call) (string, string) {
				return "Suspend task " + c.in.str("name") + inAtespace(c), "Checkpoints the sandbox and frees its worker."
			},
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				t, err := c.ax.Suspend(ctx, c.atespace, c.in.str("name"))
				if err != nil {
					return "", err
				}
				return "task.ax.io/" + c.in.str("name") + " suspended\n\n" + ax.DescribeTask(t), nil
			},
		},
		{
			name:        "delete_resource",
			description: "Delete a task, workspace, or model. Deleting a task tears down its sandbox and blocks until that completes. This cannot be undone.",
			properties:  map[string]any{"kind": kindProp, "name": nameProp, "atespace": atespaceProp},
			required:    []string{"kind", "name"},
			mutating:    true,
			summary:     func(c *call) string { return c.in.str("kind") + "/" + c.in.str("name") + inAtespace(c) },
			prompt: func(c *call) (string, string) {
				return "Delete " + c.in.str("kind") + " " + c.in.str("name") + inAtespace(c), "This cannot be undone."
			},
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				if err := c.ax.Delete(ctx, c.in.str("kind"), c.atespace, c.in.str("name")); err != nil {
					return "", err
				}
				return fmt.Sprintf("%s.ax.io/%s deleted", strings.ToLower(c.in.str("kind")), c.in.str("name")), nil
			},
		},
		{
			name: "exec_in_task",
			description: "Run a non-interactive command inside a Running task's sandbox via `ax ssh` and return its combined output and exit code. " +
				"The task must have spec.debug: true. The command runs in the workspace directory. Use it to inspect files, logs, and processes.",
			properties: map[string]any{
				"name": taskNameProp,
				"command": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"description": `Command and arguments, e.g. ["ls", "-la"]. Use ["sh", "-c", "..."] for pipes or globs.`,
				},
				"atespace": atespaceProp,
			},
			required: []string{"name", "command"},
			mutating: true,
			summary:  func(c *call) string { return c.in.str("name") + ": " + strings.Join(c.in.strs("command"), " ") },
			prompt: func(c *call) (string, string) {
				return "Run a command in task " + c.in.str("name") + inAtespace(c), "$ " + strings.Join(c.in.strs("command"), " ")
			},
			run: func(ctx context.Context, c *call) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				out, code, err := c.ax.Exec(ctx, c.atespace, c.in.str("name"), c.in.strs("command"))
				if err != nil {
					return "", err
				}
				out = truncate(out)
				if code != 0 {
					return "", fmt.Errorf("exit code %d\n%s", code, out)
				}
				if strings.TrimSpace(out) == "" {
					return "(no output, exit code 0)", nil
				}
				return out, nil
			},
		},
		{
			name:        "wait_for_task",
			description: "Poll a task until it reaches the given phase, fails, or the timeout passes. Use it after resume_task or suspend_task when the user wants to know once the task is ready.",
			properties: map[string]any{
				"name": taskNameProp,
				"phase": map[string]any{
					"type": "string", "enum": []string{"Running", "Suspended"},
					"description": "The phase to wait for.",
				},
				"timeout_seconds": map[string]any{"type": "integer", "description": "How long to wait, up to 600. Default 120."},
				"atespace":        atespaceProp,
			},
			required: []string{"name", "phase"},
			summary:  func(c *call) string { return c.in.str("name") + " → " + c.in.str("phase") },
			run:      waitForTask,
		},
		{
			name:        "read_local_file",
			description: "Read a file on the user's machine, relative to the directory the CLI was started in, e.g. a manifest the user mentions. Up to 256 KiB.",
			properties:  map[string]any{"path": map[string]any{"type": "string", "description": "File path."}},
			required:    []string{"path"},
			summary:     func(c *call) string { return c.in.str("path") },
			run: func(_ context.Context, c *call) (string, error) {
				data, err := os.ReadFile(c.in.str("path"))
				if err != nil {
					return "", err
				}
				if len(data) > 256<<10 {
					return "", fmt.Errorf("%s is %d bytes, larger than the 256 KiB limit", c.in.str("path"), len(data))
				}
				return string(data), nil
			},
		},
		{
			name:        "list_local_files",
			description: "List files in a directory on the user's machine, relative to the directory the CLI was started in. Use it to find manifests.",
			properties:  map[string]any{"dir": map[string]any{"type": "string", "description": "Directory. Defaults to the current directory."}},
			summary:     func(c *call) string { return orDot(c.in.str("dir")) },
			run: func(_ context.Context, c *call) (string, error) {
				entries, err := os.ReadDir(orDot(c.in.str("dir")))
				if err != nil {
					return "", err
				}
				var names []string
				for _, e := range entries {
					n := e.Name()
					if e.IsDir() {
						n += string(filepath.Separator)
					}
					names = append(names, n)
				}
				sort.Strings(names)
				return truncate(strings.Join(names, "\n")), nil
			},
		},
	}
}

func waitForTask(ctx context.Context, c *call) (string, error) {
	timeout := time.Duration(c.in.num("timeout_seconds", 120)) * time.Second
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	want := c.in.str("phase")
	for {
		t, err := c.ax.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: c.atespace, Name: c.in.str("name")})
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("task did not reach %s within %s", want, timeout)
			}
			return "", err
		}
		switch t.GetStatus().GetPhase() {
		case want:
			return ax.DescribeTask(t), nil
		case v1alpha1.PhaseFailed:
			return "", fmt.Errorf("task failed\n\n%s", ax.DescribeTask(t))
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("task did not reach %s within %s; it is %s\n\n%s", want, timeout, t.GetStatus().GetPhase(), ax.DescribeTask(t))
			}
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// manifestSummary names the resources in a manifest, e.g. "workspace/golang, task/test".
func manifestSummary(manifest string) string {
	var parts []string
	kind := ""
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "kind:"):
			kind = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "kind:")))
		case kind != "" && strings.HasPrefix(trimmed, "name:") && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			parts = append(parts, kind+"/"+strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), `"'`))
			kind = ""
		}
	}
	if len(parts) == 0 {
		return "manifest"
	}
	return strings.Join(parts, ", ")
}

func inAtespace(c *call) string {
	if c.atespace == "" || c.atespace == "default" {
		return ""
	}
	return " in " + c.atespace
}

func plural(kind string) string {
	if kind == "" || strings.HasSuffix(kind, "s") {
		return kind
	}
	return kind + "s"
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + fmt.Sprintf("\n… (%d more bytes truncated)", len(s)-maxToolOutput)
}
