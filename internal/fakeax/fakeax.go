// Package fakeax is an in-memory AX control plane for tests and local demos.
// It implements the AX gRPC API and /healthz without Postgres or Substrate:
// resume moves a task straight to Running and suspend to Suspended.
package fakeax

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server is an in-memory AX control plane.
type Server struct {
	v1alpha1.UnimplementedAXServer

	mu         sync.Mutex
	tasks      map[string]*v1alpha1.Task
	workspaces map[string]*v1alpha1.Workspace
	models     map[string]*v1alpha1.Model
	nextActor  int
}

// New returns an empty server.
func New() *Server {
	return &Server{
		tasks:      map[string]*v1alpha1.Task{},
		workspaces: map[string]*v1alpha1.Workspace{},
		models:     map[string]*v1alpha1.Model{},
	}
}

// Serve listens on addr (e.g. "127.0.0.1:0") and serves gRPC and /healthz on
// the same port, like ax-server. It returns the bound address and a stop func.
func (s *Server) Serve(addr string) (string, func(), error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	g := grpc.NewServer()
	v1alpha1.RegisterAXServer(g, s)
	h := http.NewServeMux()
	h.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				g.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		}),
		Protocols: unencryptedHTTP2(),
	}
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), func() { _ = srv.Close(); g.Stop() }, nil
}

func unencryptedHTTP2() *http.Protocols {
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return &p
}

func key(atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return atespace + "/" + name
}

func notFound(kind, atespace, name string) error {
	return status.Errorf(codes.NotFound, "%s %q not found in atespace %q", kind, name, atespace)
}

func defaultMeta(meta *v1alpha1.ObjectMeta) (*v1alpha1.ObjectMeta, error) {
	if meta == nil || meta.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	if meta.Atespace == "" {
		meta.Atespace = "default"
	}
	if meta.CreateTime == nil {
		meta.CreateTime = timestamppb.Now()
	}
	return meta, nil
}

// Tasks

func (s *Server) GetTask(_ context.Context, r *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[key(r.GetAtespace(), r.GetName())]
	if !ok {
		return nil, notFound("task", r.GetAtespace(), r.GetName())
	}
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (s *Server) ListTasks(_ context.Context, r *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &v1alpha1.ListTasksResponse{}
	for _, k := range sortedKeys(s.tasks) {
		if t := s.tasks[k]; t.GetMetadata().GetAtespace() == orDefault(r.GetAtespace()) {
			resp.Tasks = append(resp.Tasks, proto.Clone(t).(*v1alpha1.Task))
		}
	}
	return resp, nil
}

func (s *Server) CreateTask(_ context.Context, r *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	t := proto.Clone(r.GetTask()).(*v1alpha1.Task)
	meta, err := defaultMeta(t.GetMetadata())
	if err != nil {
		return nil, err
	}
	if err := v1alpha1.ValidateTask(t); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(meta.Atespace, meta.Name)
	if _, exists := s.tasks[k]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "task %q already exists (tasks are immutable; delete it first)", meta.Name)
	}
	if ref := t.GetSpec().GetWorkspace(); ref != nil {
		if _, ok := s.workspaces[key(meta.Atespace, ref.GetName())]; !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "workspace %q not found in atespace %q", ref.GetName(), meta.Atespace)
		}
	}
	t.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.PhaseSuspended, Id: fmt.Sprintf("t-%d", time.Now().UnixNano())}
	s.tasks[k] = t
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (s *Server) DeleteTask(_ context.Context, r *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(r.GetAtespace(), r.GetName())
	if _, ok := s.tasks[k]; !ok {
		return nil, notFound("task", r.GetAtespace(), r.GetName())
	}
	delete(s.tasks, k)
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (s *Server) SuspendTask(_ context.Context, r *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	return s.transition(r.GetAtespace(), r.GetName(), v1alpha1.PhaseRunning, v1alpha1.PhaseSuspended)
}

func (s *Server) ResumeTask(_ context.Context, r *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	return s.transition(r.GetAtespace(), r.GetName(), v1alpha1.PhaseSuspended, v1alpha1.PhaseRunning)
}

func (s *Server) transition(atespace, name, from, to string) (*v1alpha1.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[key(atespace, name)]
	if !ok {
		return nil, notFound("task", atespace, name)
	}
	if t.Status.Phase != from {
		return nil, status.Errorf(codes.FailedPrecondition, "task %q is %s, not %s", name, t.Status.Phase, from)
	}
	t.Status.Phase = to
	cond := &v1alpha1.Condition{Type: "Ready", UpdateTime: timestamppb.Now()}
	if to == v1alpha1.PhaseRunning {
		s.nextActor++
		t.Status.Actor = name
		t.Status.WorkerIp = fmt.Sprintf("10.20.3.%d", 10+s.nextActor)
		cond.Status, cond.Reason = "True", "ActorRunning"
	} else {
		t.Status.WorkerIp = ""
		cond.Status, cond.Reason = "False", "Suspended"
	}
	t.Status.Conditions = []*v1alpha1.Condition{cond}
	return proto.Clone(t).(*v1alpha1.Task), nil
}

// Workspaces

func (s *Server) GetWorkspace(_ context.Context, r *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[key(r.GetAtespace(), r.GetName())]
	if !ok {
		return nil, notFound("workspace", r.GetAtespace(), r.GetName())
	}
	return proto.Clone(w).(*v1alpha1.Workspace), nil
}

func (s *Server) ListWorkspaces(_ context.Context, r *v1alpha1.ListWorkspacesRequest) (*v1alpha1.ListWorkspacesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &v1alpha1.ListWorkspacesResponse{}
	for _, k := range sortedKeys(s.workspaces) {
		if w := s.workspaces[k]; w.GetMetadata().GetAtespace() == orDefault(r.GetAtespace()) {
			resp.Workspaces = append(resp.Workspaces, proto.Clone(w).(*v1alpha1.Workspace))
		}
	}
	return resp, nil
}

func (s *Server) UpdateWorkspace(_ context.Context, r *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	w := proto.Clone(r.GetWorkspace()).(*v1alpha1.Workspace)
	meta, err := defaultMeta(w.GetMetadata())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaces[key(meta.Atespace, meta.Name)] = w
	return proto.Clone(w).(*v1alpha1.Workspace), nil
}

func (s *Server) DeleteWorkspace(_ context.Context, r *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(r.GetAtespace(), r.GetName())
	if _, ok := s.workspaces[k]; !ok {
		return nil, notFound("workspace", r.GetAtespace(), r.GetName())
	}
	delete(s.workspaces, k)
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

// Models

func (s *Server) GetModel(_ context.Context, r *v1alpha1.GetModelRequest) (*v1alpha1.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[key(r.GetAtespace(), r.GetName())]
	if !ok {
		return nil, notFound("model", r.GetAtespace(), r.GetName())
	}
	return proto.Clone(m).(*v1alpha1.Model), nil
}

func (s *Server) ListModels(_ context.Context, r *v1alpha1.ListModelsRequest) (*v1alpha1.ListModelsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &v1alpha1.ListModelsResponse{}
	for _, k := range sortedKeys(s.models) {
		if m := s.models[k]; m.GetMetadata().GetAtespace() == orDefault(r.GetAtespace()) {
			resp.Models = append(resp.Models, proto.Clone(m).(*v1alpha1.Model))
		}
	}
	return resp, nil
}

func (s *Server) UpdateModel(_ context.Context, r *v1alpha1.UpdateModelRequest) (*v1alpha1.Model, error) {
	m := proto.Clone(r.GetModel()).(*v1alpha1.Model)
	meta, err := defaultMeta(m.GetMetadata())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models[key(meta.Atespace, meta.Name)] = m
	return proto.Clone(m).(*v1alpha1.Model), nil
}

func (s *Server) DeleteModel(_ context.Context, r *v1alpha1.DeleteModelRequest) (*v1alpha1.DeleteModelResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(r.GetAtespace(), r.GetName())
	if _, ok := s.models[k]; !ok {
		return nil, notFound("model", r.GetAtespace(), r.GetName())
	}
	delete(s.models, k)
	return &v1alpha1.DeleteModelResponse{}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orDefault(atespace string) string {
	if atespace == "" {
		return "default"
	}
	return atespace
}
