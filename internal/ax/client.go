package ax

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// Client is a connection to one AX control plane.
type Client struct {
	v1alpha1.AXClient
	Target Target
	// AXBinary is the ax CLI used for operations that need its internal
	// packages, such as ax ssh.
	AXBinary string
	conn     *grpc.ClientConn
}

// Dial connects to the control plane at t.Server. The gRPC connection is lazy,
// so Dial succeeds even when the server is down; the first RPC reports that.
func Dial(t Target, axBinary string) (*Client, error) {
	target := strings.TrimPrefix(strings.TrimPrefix(t.Server, "http://"), "https://")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	if axBinary == "" {
		axBinary = "ax"
	}
	return &Client{AXClient: v1alpha1.NewAXClient(conn), Target: t, AXBinary: axBinary, conn: conn}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Kind normalizes user-typed kinds ("task", "tasks", "Task") to the canonical
// manifest kind.
func Kind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSuffix(kind, "s")) {
	case "task":
		return v1alpha1.KindTask, nil
	case "workspace", "w":
		return v1alpha1.KindWorkspace, nil
	case "model":
		return v1alpha1.KindModel, nil
	case "":
		return "", errors.New("missing kind")
	default:
		return "", fmt.Errorf("unsupported kind %q (expected task, workspace, or model)", kind)
	}
}

// Get fetches one resource of the given kind.
func (c *Client) Get(ctx context.Context, kind, atespace, name string) (proto.Message, error) {
	k, err := Kind(kind)
	if err != nil {
		return nil, err
	}
	switch k {
	case v1alpha1.KindTask:
		return c.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
	case v1alpha1.KindWorkspace:
		return c.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: name})
	default:
		return c.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: atespace, Name: name})
	}
}

// List returns a table of every resource of the given kind in atespace.
func (c *Client) List(ctx context.Context, kind, atespace string) (Table, error) {
	k, err := Kind(kind)
	if err != nil {
		return Table{}, err
	}
	switch k {
	case v1alpha1.KindTask:
		resp, err := c.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: atespace})
		if err != nil {
			return Table{}, err
		}
		return TaskTable(resp.GetTasks()), nil
	case v1alpha1.KindWorkspace:
		resp, err := c.ListWorkspaces(ctx, &v1alpha1.ListWorkspacesRequest{Atespace: atespace})
		if err != nil {
			return Table{}, err
		}
		return WorkspaceTable(resp.GetWorkspaces()), nil
	default:
		resp, err := c.ListModels(ctx, &v1alpha1.ListModelsRequest{Atespace: atespace})
		if err != nil {
			return Table{}, err
		}
		return ModelTable(resp.GetModels()), nil
	}
}

// Names lists the names of every resource of the given kind, for completion.
func (c *Client) Names(ctx context.Context, kind, atespace string) ([]string, error) {
	t, err := c.List(ctx, kind, atespace)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(t.Rows))
	for _, r := range t.Rows {
		names = append(names, r[0])
	}
	return names, nil
}

// Describe returns the human-readable description of one resource.
func (c *Client) Describe(ctx context.Context, kind, atespace, name string) (string, error) {
	m, err := c.Get(ctx, kind, atespace, name)
	if err != nil {
		return "", err
	}
	switch r := m.(type) {
	case *v1alpha1.Task:
		return DescribeTask(r), nil
	case *v1alpha1.Workspace:
		return DescribeWorkspace(r), nil
	default:
		return DescribeModel(r.(*v1alpha1.Model)), nil
	}
}

// Delete removes one resource.
func (c *Client) Delete(ctx context.Context, kind, atespace, name string) error {
	k, err := Kind(kind)
	if err != nil {
		return err
	}
	switch k {
	case v1alpha1.KindTask:
		_, err = c.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: name})
	case v1alpha1.KindWorkspace:
		_, err = c.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: atespace, Name: name})
	default:
		_, err = c.DeleteModel(ctx, &v1alpha1.DeleteModelRequest{Atespace: atespace, Name: name})
	}
	return err
}

// Suspend checkpoints a task and pauses it.
func (c *Client) Suspend(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	return c.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: atespace, Name: name})
}

// Resume schedules a suspended task's actor and starts it.
func (c *Client) Resume(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	return c.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: atespace, Name: name})
}

// ApplyResult is the outcome of applying one manifest document.
type ApplyResult struct {
	Kind, Name, Outcome string
}

func (r ApplyResult) String() string {
	return fmt.Sprintf("%s.ax.io/%s %s", strings.ToLower(r.Kind), r.Name, r.Outcome)
}

// Apply submits every document in a multi-document manifest, in order, the
// same way `ax apply` does. A non-empty atespace fills in documents that leave
// it unset and rejects documents that name a different one. Results for the
// documents applied before a failure are returned along with the error.
func (c *Client) Apply(ctx context.Context, data []byte, atespace string) ([]ApplyResult, error) {
	var results []ApplyResult
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for i := 1; ; i++ {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				if len(results) == 0 {
					return nil, errors.New("manifest contains no resources")
				}
				return results, nil
			}
			return results, fmt.Errorf("decoding document %d: %w", i, err)
		}
		if emptyDocument(&doc) {
			continue
		}
		r, err := c.applyDocument(ctx, &doc, atespace)
		if err != nil {
			return results, fmt.Errorf("applying document %d: %w", i, err)
		}
		results = append(results, r)
	}
}

func emptyDocument(doc *yaml.Node) bool {
	if doc.Kind == 0 || (doc.Kind == yaml.DocumentNode && len(doc.Content) == 0) {
		return true
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root := doc.Content[0]
		return root.Kind == yaml.ScalarNode && root.Tag == "!!null" && root.Value == "" && root.Style&yaml.TaggedStyle == 0
	}
	return false
}

func (c *Client) applyDocument(ctx context.Context, doc *yaml.Node, atespace string) (ApplyResult, error) {
	var head struct {
		Kind string `yaml:"kind"`
	}
	if err := doc.Decode(&head); err != nil {
		return ApplyResult{}, fmt.Errorf("reading kind: %w", err)
	}
	r := ApplyResult{Kind: head.Kind}

	switch head.Kind {
	case v1alpha1.KindTask:
		var task v1alpha1.Task
		if err := doc.Decode(&task); err != nil {
			return r, err
		}
		if err := setAtespace(task.Metadata, atespace); err != nil {
			return r, err
		}
		res, err := c.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &task})
		if err != nil {
			return r, err
		}
		r.Name, r.Outcome = res.GetMetadata().GetName(), "created"

	case v1alpha1.KindWorkspace:
		var ws v1alpha1.Workspace
		if err := doc.Decode(&ws); err != nil {
			return r, err
		}
		if err := setAtespace(ws.Metadata, atespace); err != nil {
			return r, err
		}
		existing, err := c.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: ws.GetMetadata().GetAtespace(), Name: ws.GetMetadata().GetName()})
		if r.Outcome, err = applyOutcome(err, existing.GetSpec(), ws.GetSpec()); err != nil {
			return r, err
		}
		res, err := c.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &ws})
		if err != nil {
			return r, err
		}
		r.Name = res.GetMetadata().GetName()

	case v1alpha1.KindModel:
		var m v1alpha1.Model
		if err := doc.Decode(&m); err != nil {
			return r, err
		}
		if err := setAtespace(m.Metadata, atespace); err != nil {
			return r, err
		}
		existing, err := c.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: m.GetMetadata().GetAtespace(), Name: m.GetMetadata().GetName()})
		if r.Outcome, err = applyOutcome(err, existing.GetSpec(), m.GetSpec()); err != nil {
			return r, err
		}
		res, err := c.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &m})
		if err != nil {
			return r, err
		}
		r.Name = res.GetMetadata().GetName()

	case "":
		return r, errors.New("missing kind")
	default:
		return r, fmt.Errorf("unsupported kind %q (expected Task, Workspace, or Model)", head.Kind)
	}
	return r, nil
}

func setAtespace(meta *v1alpha1.ObjectMeta, atespace string) error {
	if meta == nil || atespace == "" {
		return nil
	}
	if meta.Atespace != "" && meta.Atespace != atespace {
		return fmt.Errorf("metadata.atespace %q does not match the session atespace %q", meta.Atespace, atespace)
	}
	meta.Atespace = atespace
	return nil
}

func applyOutcome(lookupErr error, existingSpec, newSpec proto.Message) (string, error) {
	if lookupErr != nil {
		if status.Code(lookupErr) == codes.NotFound {
			return "created", nil
		}
		return "", fmt.Errorf("looking up existing resource: %w", lookupErr)
	}
	if proto.Equal(existingSpec, newSpec) {
		return "unchanged", nil
	}
	return "configured", nil
}

// SSHCommand builds the `ax ssh` invocation for a task. ax ssh needs the
// router mTLS and guest client in ax's internal packages, so the CLI drives
// the ax binary rather than reimplementing them.
func (c *Client) SSHCommand(ctx context.Context, atespace, task string, command []string) *exec.Cmd {
	args := []string{"--server", c.Target.Server, "-a", atespace, "-n", c.Target.Namespace}
	if c.Target.Context != "" {
		args = append(args, "--context", c.Target.Context)
	}
	args = append(args, "ssh", task)
	if len(command) > 0 {
		args = append(args, "--")
		args = append(args, command...)
	}
	return exec.CommandContext(ctx, c.AXBinary, args...)
}

// Exec runs a non-interactive command inside a task's sandbox and returns its
// combined output and exit code.
func (c *Client) Exec(ctx context.Context, atespace, task string, command []string) (string, int, error) {
	if len(command) == 0 {
		return "", 0, errors.New("command is required")
	}
	cmd := c.SSHCommand(ctx, atespace, task, command)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), nil
	}
	return string(out), 0, err
}

// ErrorMessage turns a gRPC error into a short human-readable message.
func ErrorMessage(err error) string {
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable:
			return "AX control plane unavailable: " + s.Message()
		case codes.DeadlineExceeded:
			return "request timed out: " + s.Message()
		default:
			return fmt.Sprintf("%s: %s", s.Code(), s.Message())
		}
	}
	return err.Error()
}
