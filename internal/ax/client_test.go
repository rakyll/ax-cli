package ax

import (
	"context"
	"strings"
	"testing"

	"github.com/google/ax-cli/internal/fakeax"
)

const manifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: repo
spec:
  git:
    - name: ax
      repo: https://github.com/google/ax
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: build
spec:
  image: "example.com/runner@sha256:3a0dea6ad8b55278685db58aca6e37dc4ba04056831d45bef3aaeafdca43cac6"
  command: ["sleep", "infinity"]
  workspace:
    name: repo
  debug: true
`

func newClient(t *testing.T) *Client {
	t.Helper()
	addr, stop, err := fakeax.New().Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if !Healthy("http://" + addr) {
		t.Fatal("fake server is not healthy")
	}
	c, err := Dial(Target{Server: "http://" + addr, Namespace: "ax-system"}, "ax")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestApplyLifecycle(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	results, err := c.Apply(ctx, []byte(manifest), "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range results {
		got = append(got, r.String())
	}
	if want := "workspace.ax.io/repo created,task.ax.io/build created"; strings.Join(got, ",") != want {
		t.Errorf("apply = %q, want %q", strings.Join(got, ","), want)
	}

	// Re-applying leaves the workspace unchanged and rejects the immutable task.
	results, err = c.Apply(ctx, []byte(manifest), "")
	if err == nil {
		t.Fatal("re-applying an existing task succeeded")
	}
	if len(results) != 1 || results[0].Outcome != "unchanged" {
		t.Errorf("re-apply results = %v, want workspace unchanged", results)
	}

	table, err := c.List(ctx, "tasks", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) != 1 || table.Rows[0][2] != "Suspended" {
		t.Fatalf("tasks = %v, want build Suspended", table.Rows)
	}

	if _, err := c.Resume(ctx, "default", "build"); err != nil {
		t.Fatal(err)
	}
	desc, err := c.Describe(ctx, "task", "default", "build")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Phase:        Running", "Debug:        true", "Workspace:    repo", "Ready"} {
		if !strings.Contains(desc, want) {
			t.Errorf("describe is missing %q:\n%s", want, desc)
		}
	}
	if _, err := c.Suspend(ctx, "default", "build"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "task", "default", "build"); err != nil {
		t.Fatal(err)
	}
	names, err := c.Names(ctx, "task", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("tasks after delete = %v", names)
	}
}

func TestApplyAtespace(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	if _, err := c.Apply(ctx, []byte(manifest), "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "workspace", "team", "repo"); err != nil {
		t.Errorf("workspace not created in atespace team: %v", err)
	}
	mismatch := strings.Replace(manifest, "  name: repo\n", "  name: repo\n  atespace: other\n", 1)
	if _, err := c.Apply(ctx, []byte(mismatch), "team"); err == nil {
		t.Error("applying a manifest for another atespace succeeded")
	}
	if _, err := c.Apply(ctx, []byte("# nothing\n"), ""); err == nil {
		t.Error("applying an empty manifest succeeded")
	}
}

func TestKind(t *testing.T) {
	for in, want := range map[string]string{"task": "Task", "Tasks": "Task", "workspaces": "Workspace", "model": "Model"} {
		if got, err := Kind(in); err != nil || got != want {
			t.Errorf("Kind(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Kind("pod"); err == nil {
		t.Error("Kind(pod) succeeded")
	}
}

func TestSanitizeContext(t *testing.T) {
	if got := sanitizeContext("gke_proj_us-central1_cluster"); strings.ContainsAny(got, "/:") {
		t.Errorf("sanitizeContext left separators: %q", got)
	}
}
