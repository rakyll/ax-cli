package ax

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// Table is a list of resources in the column layout of `ax get`.
type Table struct {
	Header []string
	Rows   [][]string
	// StatusColumn is the index of the column holding a phase, or -1.
	StatusColumn int
}

// Plain renders the table as aligned text.
func (t Table) Plain() string {
	if len(t.Rows) == 0 {
		return "No resources found."
	}
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 8, 3, ' ', 0)
	fmt.Fprintln(w, strings.Join(t.Header, "\t"))
	for _, r := range t.Rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush()
	return strings.TrimRight(sb.String(), "\n")
}

// TaskTable lays tasks out like `ax get tasks`.
func TaskTable(tasks []*v1alpha1.Task) Table {
	t := Table{Header: []string{"NAME", "ATESPACE", "PHASE", "ACTOR", "WORKER-IP", "AGE"}, StatusColumn: 2}
	for _, task := range tasks {
		phase := orDefault(task.GetStatus().GetPhase(), v1alpha1.PhasePending)
		t.Rows = append(t.Rows, []string{
			task.GetMetadata().GetName(),
			task.GetMetadata().GetAtespace(),
			phase,
			orDefault(task.GetStatus().GetActor(), "<none>"),
			orDefault(task.GetStatus().GetWorkerIp(), "<none>"),
			age(task.GetMetadata()),
		})
	}
	return t
}

// WorkspaceTable lays workspaces out like `ax get workspaces`.
func WorkspaceTable(workspaces []*v1alpha1.Workspace) Table {
	t := Table{Header: []string{"NAME", "ATESPACE", "GIT-REPOS", "MCP-SERVERS"}, StatusColumn: -1}
	for _, ws := range workspaces {
		t.Rows = append(t.Rows, []string{
			ws.GetMetadata().GetName(),
			ws.GetMetadata().GetAtespace(),
			fmt.Sprint(len(ws.GetSpec().GetGit())),
			fmt.Sprint(len(ws.GetSpec().GetMcp().GetServers())),
		})
	}
	return t
}

// ModelTable lays models out like `ax get models`.
func ModelTable(models []*v1alpha1.Model) Table {
	t := Table{Header: []string{"NAME", "ATESPACE", "PROVIDER", "MODEL"}, StatusColumn: -1}
	for _, m := range models {
		t.Rows = append(t.Rows, []string{
			m.GetMetadata().GetName(),
			m.GetMetadata().GetAtespace(),
			m.GetSpec().GetProvider(),
			m.GetSpec().GetModel(),
		})
	}
	return t
}

// YAML renders a resource as its manifest plus live status.
func YAML(m proto.Message) string {
	out, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Sprintf("<unable to render: %v>", err)
	}
	return strings.TrimRight(string(out), "\n")
}

// DescribeTask renders a task like `ax describe task`.
func DescribeTask(task *v1alpha1.Task) string {
	var sb strings.Builder
	field := func(k, v string) { fmt.Fprintf(&sb, "%-13s %s\n", k+":", v) }
	field("Name", task.GetMetadata().GetName())
	field("Atespace", task.GetMetadata().GetAtespace())
	field("Phase", orDefault(task.GetStatus().GetPhase(), v1alpha1.PhasePending))
	field("Actor", orDefault(task.GetStatus().GetActor(), "<none>"))
	field("Worker IP", orDefault(task.GetStatus().GetWorkerIp(), "<none>"))
	field("Age", age(task.GetMetadata()))
	field("Debug", fmt.Sprint(task.GetSpec().GetDebug()))
	if spec := task.GetSpec(); spec != nil {
		if ref := spec.GetWorkspace(); ref != nil {
			field("Workspace", fmt.Sprintf("%s  path=%s", ref.GetName(), spec.WorkspacePath()))
		}
		if spec.GetImage() != "" {
			field("Image", spec.GetImage())
		}
		if len(spec.GetCommand()) > 0 {
			field("Command", strings.Join(spec.GetCommand(), " "))
		}
		if config := spec.GetConfig().AsMap(); len(config) > 0 {
			if out, err := yaml.Marshal(config); err == nil {
				sb.WriteString("Config:\n")
				for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
					fmt.Fprintf(&sb, "  %s\n", line)
				}
			}
		}
	}
	if conds := task.GetStatus().GetConditions(); len(conds) > 0 {
		sb.WriteString("\nConditions:\n")
		w := tabwriter.NewWriter(&sb, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE")
		for _, c := range conds {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.GetType(), c.GetStatus(), c.GetReason(), c.GetMessage())
		}
		_ = w.Flush()
	}
	return strings.TrimRight(sb.String(), "\n")
}

// DescribeWorkspace renders a workspace like `ax describe workspace`.
func DescribeWorkspace(ws *v1alpha1.Workspace) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Name:         %s\n", ws.GetMetadata().GetName())
	fmt.Fprintf(&sb, "Atespace:     %s\n", ws.GetMetadata().GetAtespace())
	spec := ws.GetSpec()
	if len(spec.GetGit()) > 0 {
		sb.WriteString("Git Repositories:\n")
		for _, g := range spec.GetGit() {
			fmt.Fprintf(&sb, "  - %s (%s, branch: %s)\n", g.GetName(), g.GetRepo(), orDefault(g.GetBranch(), "main"))
		}
	}
	if len(spec.GetFiles()) > 0 {
		sb.WriteString("Files:\n")
		for _, f := range spec.GetFiles() {
			fmt.Fprintf(&sb, "  - %s (%d bytes)\n", f.GetPath(), len(f.GetContent()))
		}
	}
	if regs := spec.GetMcp().GetRegistries(); len(regs) > 0 {
		sb.WriteString("MCP Registries:\n")
		for _, r := range regs {
			fmt.Fprintf(&sb, "  - Provider: %s, Query: %q\n", r.GetProvider(), r.GetQuery())
		}
	}
	if servers := spec.GetMcp().GetServers(); len(servers) > 0 {
		sb.WriteString("MCP Servers:\n")
		for _, s := range servers {
			if s.GetEndpoint() != "" {
				fmt.Fprintf(&sb, "  - %s: %s\n", s.GetName(), s.GetEndpoint())
			} else {
				fmt.Fprintf(&sb, "  - %s: %s %s\n", s.GetName(), s.GetCommand(), strings.Join(s.GetArgs(), " "))
			}
		}
	}
	if regs := spec.GetSkills().GetRegistries(); len(regs) > 0 {
		sb.WriteString("Skill Registries:\n")
		for _, r := range regs {
			fmt.Fprintf(&sb, "  - Provider: %s, Query: %q\n", r.GetProvider(), r.GetQuery())
		}
	}
	if p := spec.GetSkills().GetPath(); p != "" {
		fmt.Fprintf(&sb, "Skills Path:  %s\n", p)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// DescribeModel renders a model like `ax describe model`.
func DescribeModel(m *v1alpha1.Model) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Name:         %s\n", m.GetMetadata().GetName())
	fmt.Fprintf(&sb, "Atespace:     %s\n", m.GetMetadata().GetAtespace())
	fmt.Fprintf(&sb, "Provider:     %s\n", m.GetSpec().GetProvider())
	fmt.Fprintf(&sb, "Model:        %s\n", m.GetSpec().GetModel())
	if params := m.GetSpec().GetParameters().AsMap(); len(params) > 0 {
		sb.WriteString("Parameters:\n")
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "  %s: %v\n", k, params[k])
		}
	}
	if ref := m.GetSpec().GetSecretKey(); ref != nil {
		fmt.Fprintf(&sb, "Secret:       %s (key: %s)\n", ref.GetName(), ref.GetKey())
	}
	return strings.TrimRight(sb.String(), "\n")
}

// FormatAge renders a duration the way `ax get` does: 42s, 5m, 3h, 2d.
func FormatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func age(meta *v1alpha1.ObjectMeta) string {
	if meta.GetCreateTime() == nil {
		return "<unknown>"
	}
	return FormatAge(time.Since(meta.GetCreateTime().AsTime()))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
