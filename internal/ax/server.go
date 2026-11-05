// Package ax wraps the AX control plane API for the interactive CLI: resolving
// which server to talk to, calling its RPCs, and rendering the results.
package ax

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Target describes how the CLI reaches the AX control plane.
type Target struct {
	// Server is the control plane address, e.g. http://127.0.0.1:50621.
	Server string
	// Context is the Kubernetes context the server was resolved from, if any.
	Context string
	// Namespace is the Kubernetes namespace AX is installed in.
	Namespace string
	// Via explains how Server was chosen ("--server", "$AX_SERVER", "tunnel", ...).
	Via string
}

// ResolveOptions mirror the global flags of the ax CLI.
type ResolveOptions struct {
	Server    string
	Context   string
	Namespace string
	// AXBinary is the ax CLI used to start a tunnel when none is running.
	AXBinary string
}

// Resolve finds the control plane the same way the ax CLI does: an explicit
// --server, then $AX_SERVER, then a background port-forward for the active
// Kubernetes context. Tunnels are owned by the ax CLI (state lives in
// ~/.ax/tunnels), so when none is healthy Resolve asks ax to start one rather
// than managing kubectl itself.
func Resolve(ctx context.Context, opts ResolveOptions) (Target, error) {
	if opts.Namespace == "" {
		opts.Namespace = os.Getenv("AX_SYSTEM_NAMESPACE")
	}
	if opts.Namespace == "" {
		opts.Namespace = "ax-system"
	}
	t := Target{Namespace: opts.Namespace}

	if opts.Server != "" {
		t.Server, t.Via = opts.Server, "--server"
		return t, nil
	}
	if env := os.Getenv("AX_SERVER"); env != "" {
		t.Server, t.Via = env, "$AX_SERVER"
		return t, nil
	}

	t.Context = CurrentContext(opts.Context)
	if t.Context == "" {
		t.Server, t.Via = "http://localhost:8080", "default (no kube context)"
		return t, nil
	}

	if port, ok := healthyTunnel(t.Context, opts.Namespace); ok {
		t.Server, t.Via = fmt.Sprintf("http://127.0.0.1:%d", port), "tunnel"
		return t, nil
	}

	// Any server-bound ax command establishes the tunnel as a side effect.
	bin := opts.AXBinary
	if bin == "" {
		bin = "ax"
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, runErr := exec.CommandContext(cmdCtx, bin, "--context", t.Context, "-n", opts.Namespace, "get", "models").CombinedOutput()
	if port, ok := healthyTunnel(t.Context, opts.Namespace); ok {
		t.Server, t.Via = fmt.Sprintf("http://127.0.0.1:%d", port), "tunnel (new)"
		return t, nil
	}
	msg := strings.TrimSpace(string(out))
	if runErr != nil && msg == "" {
		msg = runErr.Error()
	}
	return t, fmt.Errorf("could not reach AX in context %q: %s", t.Context, msg)
}

type tunnelInfo struct {
	Namespace string `json:"namespace"`
	Port      int    `json:"port"`
}

func tunnelDir() string {
	if dir := os.Getenv("AX_HOME"); dir != "" {
		return filepath.Join(dir, "tunnels")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ax", "tunnels")
}

// sanitizeContext matches the ax CLI's tunnel state file naming.
func sanitizeContext(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "default"
	}
	return sb.String()
}

func healthyTunnel(kubeContext, namespace string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(tunnelDir(), sanitizeContext(kubeContext)+".json"))
	if err != nil {
		return 0, false
	}
	var info tunnelInfo
	if json.Unmarshal(data, &info) != nil || info.Port <= 0 {
		return 0, false
	}
	if info.Namespace != "" && info.Namespace != namespace {
		return 0, false
	}
	return info.Port, Healthy(fmt.Sprintf("http://127.0.0.1:%d", info.Port))
}

// Healthy reports whether the control plane at server answers /healthz.
func Healthy(server string) bool {
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(strings.TrimRight(server, "/") + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// CurrentContext returns override, $KUBECONTEXT, or the kubeconfig's
// current-context, in that order. It returns "" when none is set.
func CurrentContext(override string) string {
	if override != "" {
		return override
	}
	if env := os.Getenv("KUBECONTEXT"); env != "" {
		return env
	}
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".kube", "config")
		}
	}
	if path != "" && !strings.Contains(path, string(os.PathListSeparator)) {
		if data, err := os.ReadFile(path); err == nil {
			var cfg struct {
				CurrentContext string `yaml:"current-context"`
			}
			if yaml.Unmarshal(data, &cfg) == nil && cfg.CurrentContext != "" {
				return cfg.CurrentContext
			}
		}
	}
	out, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
