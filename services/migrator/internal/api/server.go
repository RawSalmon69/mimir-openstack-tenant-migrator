// Package api provides the HTTP API for the migration service.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/NIPA-Mimir/services/migrator/internal/queue"
	"github.com/hibiken/asynq"
)

// Enqueuer abstracts the asynq.Client.Enqueue method for testing.
type Enqueuer interface {
	Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// TaskInspector abstracts the asynq.Inspector methods we use, for testing.
type TaskInspector interface {
	ListPendingTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	ListActiveTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	ListScheduledTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	ListRetryTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	ListArchivedTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	ListCompletedTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	DeleteTask(qname, id string) error
	CancelProcessing(id string) error
}

// OverrideApplier applies Mimir OOO + rate-limit overrides for a set of tenants.
type OverrideApplier interface {
	ApplyOverrides(ctx context.Context, tenants []string) error
}

// ServerConfig holds dependencies for the HTTP server.
type ServerConfig struct {
	Client            Enqueuer
	ProgressStore     *queue.ProgressStore
	Applier           OverrideApplier
	Inspector         TaskInspector
	TSDBPath          string
	CortexURL         string
	MaxBatchBytes     int
	WriterConcurrency int
	DryRun            bool
	Logger            *slog.Logger
	// ReadinessFunc reports whether the server is ready to serve traffic.
	// Wired in cmd/server/main.go to flip true after the shared TSDB
	// opens. If nil, /readyz defaults to 200.
	ReadinessFunc func() bool
}

// noopApplier is a package-local fallback so the server can always call
// Applier without a nil check in the handler.
type noopApplier struct{}

func (noopApplier) ApplyOverrides(_ context.Context, _ []string) error { return nil }

// noopInspector is a no-op TaskInspector used when no real inspector is wired
// (e.g. in tests that don't need DELETE /tasks).
type noopInspector struct{}

func (noopInspector) ListPendingTasks(_ string, _ ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return nil, nil
}
func (noopInspector) ListActiveTasks(_ string, _ ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return nil, nil
}
func (noopInspector) ListScheduledTasks(_ string, _ ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return nil, nil
}
func (noopInspector) ListRetryTasks(_ string, _ ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return nil, nil
}
func (noopInspector) ListArchivedTasks(_ string, _ ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return nil, nil
}
func (noopInspector) ListCompletedTasks(_ string, _ ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return nil, nil
}
func (noopInspector) DeleteTask(_, _ string) error     { return nil }
func (noopInspector) CancelProcessing(_ string) error  { return nil }

// NewServer creates an http.ServeMux with all migration API routes registered.
func NewServer(cfg ServerConfig) *http.ServeMux {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Applier == nil {
		cfg.Applier = noopApplier{}
	}
	if cfg.Inspector == nil {
		cfg.Inspector = noopInspector{}
	}
	if cfg.ReadinessFunc == nil {
		cfg.ReadinessFunc = func() bool { return true }
	}

	mux := http.NewServeMux()

	h := &handlers{cfg: cfg}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !cfg.ReadinessFunc() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("POST /migrate", h.handleMigrate)
	mux.HandleFunc("GET /status", h.handleStatus)
	mux.HandleFunc("GET /status/{task_id}", h.handleTaskStatus)
	mux.HandleFunc("DELETE /tasks", h.handleDeleteTasks)

	return mux
}
