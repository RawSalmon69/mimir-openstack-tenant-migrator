package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
	"testing"

	"github.com/NIPA-Mimir/services/migrator/internal/queue"
	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// --- mock enqueuer ---

type mockEnqueuer struct {
	calls   []mockEnqueueCall
	failOn  map[string]bool
	counter int
}

type mockEnqueueCall struct {
	Task *asynq.Task
}

func (m *mockEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	m.calls = append(m.calls, mockEnqueueCall{Task: task})
	m.counter++
	id := fmt.Sprintf("task-%d", m.counter)

	// Check if we should fail for this task.
	payload, _ := queue.ParseMigratePayload(task)
	if m.failOn != nil && m.failOn[payload.ProjectID] {
		return nil, fmt.Errorf("enqueue failed for %s", payload.ProjectID)
	}

	return &asynq.TaskInfo{ID: id, Queue: "migration"}, nil
}

// --- spy applier ---

type spyApplier struct {
	calls [][]string
	err   error
}

func (s *spyApplier) ApplyOverrides(_ context.Context, tenants []string) error {
	cp := make([]string, len(tenants))
	copy(cp, tenants)
	s.calls = append(s.calls, cp)
	return s.err
}

// --- helpers ---

func setupTest(t *testing.T) (*http.ServeMux, *mockEnqueuer, *queue.ProgressStore, *miniredis.Miniredis) {
	t.Helper()
	return setupTestWithApplier(t, &spyApplier{})
}

func setupTestWithApplier(t *testing.T, applier OverrideApplier) (*http.ServeMux, *mockEnqueuer, *queue.ProgressStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := queue.NewProgressStore(rdb)
	enq := &mockEnqueuer{}

	mux := NewServer(ServerConfig{
		Client:            enq,
		ProgressStore:     store,
		Applier:           applier,
		TSDBPath:          "/data/tsdb",
		CortexURL:         "http://mimir:8080",
		MaxBatchBytes:     3 * 1024 * 1024,
		WriterConcurrency: 4,
		DryRun:            false,
	})
	return mux, enq, store, mr
}

// --- POST /migrate tests ---

func TestMigrate_Valid(t *testing.T) {
	mux, enq, _, _ := setupTest(t)

	body := `{"project_ids": ["proj-a", "proj-b"]}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(enq.calls) != 2 {
		t.Fatalf("expected 2 enqueue calls, got %d", len(enq.calls))
	}

	var resp migrateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(resp.Tasks))
	}
	if resp.Tasks[0].ProjectID != "proj-a" {
		t.Errorf("expected proj-a, got %s", resp.Tasks[0].ProjectID)
	}
}

func TestMigrate_SingleProject(t *testing.T) {
	mux, enq, _, _ := setupTest(t)

	body := `{"project_ids": ["single"]}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(enq.calls))
	}
}

func TestMigrate_EmptyBody(t *testing.T) {
	mux, _, _, _ := setupTest(t)

	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(""))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMigrate_EmptyArray(t *testing.T) {
	mux, _, _, _ := setupTest(t)

	body := `{"project_ids": []}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMigrate_MissingField(t *testing.T) {
	mux, _, _, _ := setupTest(t)

	body := `{"other": "value"}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMigrate_EnqueueFailure(t *testing.T) {
	mux, enq, _, _ := setupTest(t)
	enq.failOn = map[string]bool{"proj-fail": true}

	body := `{"project_ids": ["proj-fail"]}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// --- GET /status tests ---

func TestStatus_Empty(t *testing.T) {
	mux, _, _, _ := setupTest(t)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("expected total=0, got %d", resp.Total)
	}
}

func TestStatus_WithTasks(t *testing.T) {
	mux, _, store, _ := setupTest(t)
	ctx := context.Background()

	// Create some progress entries.
	_ = store.Update(ctx, "t1", map[string]interface{}{
		"project_id": "p1", "state": "completed", "samples_read": "500",
	})
	_ = store.Update(ctx, "t2", map[string]interface{}{
		"project_id": "p2", "state": "active", "samples_read": "200",
	})
	_ = store.Update(ctx, "t3", map[string]interface{}{
		"project_id": "p3", "state": "pending",
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 3 {
		t.Errorf("expected total=3, got %d", resp.Total)
	}
	if resp.Completed != 1 {
		t.Errorf("expected completed=1, got %d", resp.Completed)
	}
	if resp.Active != 1 {
		t.Errorf("expected active=1, got %d", resp.Active)
	}
	if resp.Pending != 1 {
		t.Errorf("expected pending=1, got %d", resp.Pending)
	}
}

// --- GET /status/{task_id} tests ---

func TestTaskStatus_Exists(t *testing.T) {
	mux, _, store, _ := setupTest(t)
	ctx := context.Background()

	_ = store.Update(ctx, "abc-123", map[string]interface{}{
		"project_id": "proj-x", "state": "active", "samples_read": "42",
	})

	req := httptest.NewRequest(http.MethodGet, "/status/abc-123", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var p queue.TaskProgress
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ProjectID != "proj-x" {
		t.Errorf("expected proj-x, got %s", p.ProjectID)
	}
	if p.SamplesRead != 42 {
		t.Errorf("expected 42 samples, got %d", p.SamplesRead)
	}
}

func TestTaskStatus_NotFound(t *testing.T) {
	mux, _, _, _ := setupTest(t)

	req := httptest.NewRequest(http.MethodGet, "/status/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTaskStatus_ETA(t *testing.T) {
	mux, _, store, _ := setupTest(t)
	ctx := context.Background()

	// Simulate a task that started 10 min ago and read 1000 samples.
	_ = store.Update(ctx, "eta-task", map[string]interface{}{
		"project_id":   "proj-eta",
		"state":        "active",
		"samples_read": "1000",
		"started_at":   time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339Nano),
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Find the eta-task entry.
	found := false
	for _, item := range resp.Tasks {
		if item.TaskID == "eta-task" {
			found = true
			if item.ETASeconds == nil {
				t.Error("expected ETA to be set for active task with >100 samples")
			} else if *item.ETASeconds <= 0 {
				t.Errorf("expected positive ETA, got %f", *item.ETASeconds)
			}
		}
	}
	if !found {
		t.Error("eta-task not found in status response")
	}
}

// TestHandleMigrateCallsApplierBeforeEnqueue verifies that ApplyOverrides is
// called exactly once with the exact tenant list and that it is called before
// any Enqueue invocation.
func TestHandleMigrateCallsApplierBeforeEnqueue(t *testing.T) {
	// Use an instrumented applier that records its call position relative to
	// enqueue calls by reading the enqueuer's counter at the time of apply.
	var events []callEvent

	applier := &spyApplier{}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := queue.NewProgressStore(rdb)

	// Wrap a real mockEnqueuer so we can record event order.
	baseEnq := &mockEnqueuer{}
	orderEnq := &orderTrackingEnqueuer{inner: baseEnq, events: &events}
	orderApplier := &orderTrackingApplier{inner: applier, events: &events}

	mux := NewServer(ServerConfig{
		Client:            orderEnq,
		ProgressStore:     store,
		Applier:           orderApplier,
		TSDBPath:          "/data",
		CortexURL:         "http://mimir:8080",
		MaxBatchBytes:     1024,
		WriterConcurrency: 1,
	})

	body := `{"project_ids": ["tenant-1", "tenant-2", "tenant-3"]}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// ApplyOverrides must appear exactly once and before the first enqueue.
	applyIdx := -1
	for i, e := range events {
		if e.kind == "apply" {
			applyIdx = i
			break
		}
	}
	if applyIdx == -1 {
		t.Fatal("ApplyOverrides was never called")
	}
	for i, e := range events {
		if e.kind == "enqueue" && i < applyIdx {
			t.Fatalf("Enqueue at position %d happened before ApplyOverrides at position %d", i, applyIdx)
		}
	}

	// Verify the tenants passed to ApplyOverrides.
	if len(applier.calls) != 1 {
		t.Fatalf("expected 1 ApplyOverrides call, got %d", len(applier.calls))
	}
	got := applier.calls[0]
	want := []string{"tenant-1", "tenant-2", "tenant-3"}
	if len(got) != len(want) {
		t.Fatalf("tenant list len mismatch: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tenant[%d]: want %s, got %s", i, want[i], got[i])
		}
	}
}

// TestHandleMigrateFailsFastWhenApplierErrors verifies that a 500 is returned
// and no tasks are enqueued when ApplyOverrides returns an error.
func TestHandleMigrateFailsFastWhenApplierErrors(t *testing.T) {
	spy := &spyApplier{err: errors.New("k8s boom")}
	mux, enq, _, _ := setupTestWithApplier(t, spy)

	body := `{"project_ids": ["proj-a", "proj-b"]}`
	req := httptest.NewRequest(http.MethodPost, "/migrate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to apply mimir overrides") {
		t.Errorf("expected body to contain 'failed to apply mimir overrides', got: %s", w.Body.String())
	}
	if len(enq.calls) != 0 {
		t.Errorf("expected 0 enqueue calls when applier errors, got %d", len(enq.calls))
	}
}

// --- fake inspector ---

type fakeInspector struct {
	// pending, active, scheduled, retry, archived, completed tasks by state name.
	tasks map[string][]*asynq.TaskInfo

	// Track calls for assertions.
	cancelledIDs []string
	deletedIDs   []string
}

func newFakeInspector() *fakeInspector {
	return &fakeInspector{tasks: map[string][]*asynq.TaskInfo{
		"pending":   {},
		"active":    {},
		"scheduled": {},
		"retry":     {},
		"archived":  {},
		"completed": {},
	}}
}

// addTask adds a task info to the specified state bucket with the given project_id payload.
func (f *fakeInspector) addTask(state, id, projectID string) {
	payload, _ := json.Marshal(map[string]string{"project_id": projectID})
	f.tasks[state] = append(f.tasks[state], &asynq.TaskInfo{
		ID:      id,
		Type:    "migrate:tenant",
		Payload: payload,
		Queue:   "migration",
	})
}

func (f *fakeInspector) ListPendingTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return f.tasks["pending"], nil
}
func (f *fakeInspector) ListActiveTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return f.tasks["active"], nil
}
func (f *fakeInspector) ListScheduledTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return f.tasks["scheduled"], nil
}
func (f *fakeInspector) ListRetryTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return f.tasks["retry"], nil
}
func (f *fakeInspector) ListArchivedTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return f.tasks["archived"], nil
}
func (f *fakeInspector) ListCompletedTasks(qname string, opts ...asynq.ListOption) ([]*asynq.TaskInfo, error) {
	return f.tasks["completed"], nil
}
func (f *fakeInspector) DeleteTask(qname, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}
func (f *fakeInspector) CancelProcessing(id string) error {
	f.cancelledIDs = append(f.cancelledIDs, id)
	return nil
}

// setupTestWithInspector sets up the test server with a fake inspector.
func setupTestWithInspector(t *testing.T, insp TaskInspector) (*http.ServeMux, *queue.ProgressStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := queue.NewProgressStore(rdb)
	enq := &mockEnqueuer{}
	mux := NewServer(ServerConfig{
		Client:        enq,
		ProgressStore: store,
		Inspector:     insp,
		TSDBPath:      "/data/tsdb",
		CortexURL:     "http://mimir:8080",
		MaxBatchBytes: 3 * 1024 * 1024,
	})
	return mux, store, mr
}

// --- DELETE /tasks tests ---

func TestDeleteTasksMissingParams(t *testing.T) {
	mux, _, _ := setupTestWithInspector(t, newFakeInspector())
	req := httptest.NewRequest(http.MethodDelete, "/tasks", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "either project_id or all=true is required") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestDeleteTasksBothParams(t *testing.T) {
	mux, _, _ := setupTestWithInspector(t, newFakeInspector())
	req := httptest.NewRequest(http.MethodDelete, "/tasks?all=true&project_id=x", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mutually exclusive") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestDeleteTasksAllTrue(t *testing.T) {
	fi := newFakeInspector()
	// tenant-a: 3 pending tasks
	fi.addTask("pending", "a1", "tenant-a")
	fi.addTask("pending", "a2", "tenant-a")
	fi.addTask("pending", "a3", "tenant-a")
	// tenant-b: 1 active task
	fi.addTask("active", "b1", "tenant-b")

	mux, store, _ := setupTestWithInspector(t, fi)
	ctx := context.Background()

	// Seed 4 progress entries.
	for _, id := range []string{"a1", "a2", "a3", "b1"} {
		_ = store.Update(ctx, id, map[string]interface{}{"project_id": "t", "state": "pending"})
	}

	req := httptest.NewRequest(http.MethodDelete, "/tasks?all=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp deleteTasksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DeletedProgress != 4 {
		t.Errorf("deleted_progress = %d, want 4", resp.DeletedProgress)
	}
	if resp.DeletedTasks != 4 {
		t.Errorf("deleted_tasks = %d, want 4", resp.DeletedTasks)
	}
	if !resp.All {
		t.Error("all should be true in response")
	}

	// Progress store should be empty.
	list, _ := store.List(ctx)
	if len(list) != 0 {
		t.Errorf("expected empty store, got %d entries", len(list))
	}

	// CancelProcessing called only for active task b1.
	if len(fi.cancelledIDs) != 1 || fi.cancelledIDs[0] != "b1" {
		t.Errorf("CancelProcessing calls = %v, want [b1]", fi.cancelledIDs)
	}
	// DeleteTask called for all 4.
	if len(fi.deletedIDs) != 4 {
		t.Errorf("DeleteTask calls = %d, want 4", len(fi.deletedIDs))
	}
}

func TestDeleteTasksByProjectID(t *testing.T) {
	fi := newFakeInspector()
	fi.addTask("pending", "a1", "tenant-a")
	fi.addTask("pending", "a2", "tenant-a")
	fi.addTask("active", "b1", "tenant-b")

	mux, store, _ := setupTestWithInspector(t, fi)
	ctx := context.Background()

	for _, entry := range []struct{ id, pid string }{
		{"a1", "tenant-a"}, {"a2", "tenant-a"}, {"b1", "tenant-b"},
	} {
		_ = store.Update(ctx, entry.id, map[string]interface{}{"project_id": entry.pid, "state": "pending"})
	}

	req := httptest.NewRequest(http.MethodDelete, "/tasks?project_id=tenant-a", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp deleteTasksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DeletedTasks != 2 {
		t.Errorf("deleted_tasks = %d, want 2", resp.DeletedTasks)
	}
	if resp.DeletedProgress != 2 {
		t.Errorf("deleted_progress = %d, want 2", resp.DeletedProgress)
	}
	if resp.ProjectID != "tenant-a" {
		t.Errorf("project_id = %q, want tenant-a", resp.ProjectID)
	}

	// tenant-b's progress entry should remain.
	list, _ := store.List(ctx)
	if len(list) != 1 || list[0].TaskID != "b1" {
		t.Errorf("expected only b1 to remain, got %v", list)
	}

	// CancelProcessing should NOT have been called (a1/a2 are pending, not active).
	if len(fi.cancelledIDs) != 0 {
		t.Errorf("unexpected CancelProcessing calls: %v", fi.cancelledIDs)
	}
	// DeleteTask called only for tenant-a tasks.
	if len(fi.deletedIDs) != 2 {
		t.Errorf("DeleteTask calls = %d, want 2", len(fi.deletedIDs))
	}
}

func TestDeleteTasksIdempotent(t *testing.T) {
	fi := newFakeInspector()
	fi.addTask("pending", "x1", "tenant-x")

	mux, store, _ := setupTestWithInspector(t, fi)
	ctx := context.Background()
	_ = store.Update(ctx, "x1", map[string]interface{}{"project_id": "tenant-x", "state": "pending"})

	// First call.
	req1 := httptest.NewRequest(http.MethodDelete, "/tasks?all=true", nil)
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	// Second call — inspector returns no tasks (all already gone), progress store empty.
	fi.tasks["pending"] = nil
	req2 := httptest.NewRequest(http.MethodDelete, "/tasks?all=true", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var resp deleteTasksResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DeletedTasks != 0 {
		t.Errorf("second call deleted_tasks = %d, want 0", resp.DeletedTasks)
	}
	if resp.DeletedProgress != 0 {
		t.Errorf("second call deleted_progress = %d, want 0", resp.DeletedProgress)
	}
}

// --- order-tracking helpers ---

type callEvent struct{ kind string }

type orderTrackingApplier struct {
	inner  *spyApplier
	events *[]callEvent
}

func (o *orderTrackingApplier) ApplyOverrides(ctx context.Context, tenants []string) error {
	*o.events = append(*o.events, callEvent{kind: "apply"})
	return o.inner.ApplyOverrides(ctx, tenants)
}

type orderTrackingEnqueuer struct {
	inner  *mockEnqueuer
	events *[]callEvent
}

func (o *orderTrackingEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	*o.events = append(*o.events, callEvent{kind: "enqueue"})
	return o.inner.Enqueue(task, opts...)
}

// --- /readyz tests ---

func TestReadyz_NotReady(t *testing.T) {
	mux := NewServer(ServerConfig{
		ReadinessFunc: func() bool { return false },
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not ready") {
		t.Errorf("expected body to contain 'not ready', got %q", w.Body.String())
	}
}

func TestReadyz_Ready(t *testing.T) {
	mux := NewServer(ServerConfig{
		ReadinessFunc: func() bool { return true },
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ready") {
		t.Errorf("expected body to contain 'ready', got %q", w.Body.String())
	}
}

// TestReadyz_NoFuncDefaultsReady documents the behavior when no ReadinessFunc
// is wired (e.g. tests, local dev): /readyz returns 200. Production must wire
// ReadinessFunc: ready.Load — see cmd/server/main.go.
func TestReadyz_NoFuncDefaultsReady(t *testing.T) {
	mux := NewServer(ServerConfig{}) // no ReadinessFunc

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (nil func defaults to ready), got %d", w.Code)
	}
}

// TestHealthz_AlwaysOK_AfterReadyzAdded guards against /readyz registration
// accidentally clobbering /healthz.
func TestHealthz_AlwaysOK_AfterReadyzAdded(t *testing.T) {
	mux := NewServer(ServerConfig{
		ReadinessFunc: func() bool { return false },
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected /healthz to always return 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("expected body 'ok', got %q", w.Body.String())
	}
}
