package queue

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestStore(t *testing.T) (*ProgressStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewProgressStore(rdb), mr
}

func TestProgressUpdateAndGet(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	err := store.Update(ctx, "task-1", map[string]interface{}{
		"project_id":   "tenant-a",
		"state":        "active",
		"series_read":  "10",
		"samples_read": "100",
		"started_at":   "2025-06-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	p, err := store.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want %q", p.TaskID, "task-1")
	}
	if p.ProjectID != "tenant-a" {
		t.Errorf("ProjectID = %q, want %q", p.ProjectID, "tenant-a")
	}
	if p.State != "active" {
		t.Errorf("State = %q, want %q", p.State, "active")
	}
	if p.SeriesRead != 10 {
		t.Errorf("SeriesRead = %d, want 10", p.SeriesRead)
	}
	if p.SamplesRead != 100 {
		t.Errorf("SamplesRead = %d, want 100", p.SamplesRead)
	}
	if p.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
}

func TestProgressGetNonexistent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	p, err := store.Get(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.TaskID != "does-not-exist" {
		t.Errorf("TaskID = %q, want %q", p.TaskID, "does-not-exist")
	}
	if p.State != "" {
		t.Errorf("State = %q, want empty for nonexistent key", p.State)
	}
	if p.SeriesRead != 0 {
		t.Errorf("SeriesRead = %d, want 0", p.SeriesRead)
	}
}

func TestProgressList(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	_ = store.Update(ctx, "task-1", map[string]interface{}{
		"project_id": "tenant-a",
		"state":      "completed",
	})
	_ = store.Update(ctx, "task-2", map[string]interface{}{
		"project_id": "tenant-b",
		"state":      "active",
	})

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d items, want 2", len(list))
	}

	byID := map[string]TaskProgress{}
	for _, p := range list {
		byID[p.TaskID] = p
	}
	if byID["task-1"].State != "completed" {
		t.Errorf("task-1 state = %q, want completed", byID["task-1"].State)
	}
	if byID["task-2"].State != "active" {
		t.Errorf("task-2 state = %q, want active", byID["task-2"].State)
	}
}

func TestProgressDelete(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Seed 3 entries.
	for _, id := range []string{"task-1", "task-2", "task-3"} {
		_ = store.Update(ctx, id, map[string]interface{}{"project_id": "p", "state": "completed"})
	}

	// Delete 2 by ID.
	n, err := store.Delete(ctx, []string{"task-1", "task-2"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 2 {
		t.Errorf("Delete returned %d, want 2", n)
	}

	// Remaining list should be 1 entry.
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d items, want 1", len(list))
	}
	if list[0].TaskID != "task-3" {
		t.Errorf("remaining task = %q, want task-3", list[0].TaskID)
	}

	// Deleting the same keys again is idempotent (returns 0).
	n2, err := store.Delete(ctx, []string{"task-1", "task-2"})
	if err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent Delete returned %d, want 0", n2)
	}
}

func TestProgressDeleteAll(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Seed 3 entries.
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		_ = store.Update(ctx, id, map[string]interface{}{"project_id": "p", "state": "completed"})
	}

	n, err := store.DeleteAll(ctx)
	if err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if n != 3 {
		t.Errorf("DeleteAll returned %d, want 3", n)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List after DeleteAll: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List returned %d items, want 0", len(list))
	}

	// Calling DeleteAll on empty store is idempotent.
	n2, err := store.DeleteAll(ctx)
	if err != nil {
		t.Fatalf("DeleteAll (empty): %v", err)
	}
	if n2 != 0 {
		t.Errorf("DeleteAll on empty store returned %d, want 0", n2)
	}
}

func TestProgressUpdateEmpty(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Empty fields map should be a no-op.
	err := store.Update(ctx, "task-1", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Update with empty fields: %v", err)
	}
}

func TestReconcileStaleActive(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	_ = store.Update(ctx, "task-active-1", map[string]interface{}{
		"project_id": "tenant-a",
		"state":      "active",
	})
	_ = store.Update(ctx, "task-active-2", map[string]interface{}{
		"project_id": "tenant-b",
		"state":      "active",
	})
	_ = store.Update(ctx, "task-completed", map[string]interface{}{
		"project_id": "tenant-c",
		"state":      "completed",
	})
	_ = store.Update(ctx, "task-failed", map[string]interface{}{
		"project_id": "tenant-d",
		"state":      "failed",
		"error":      "some earlier error",
	})
	_ = store.Update(ctx, "task-pending", map[string]interface{}{
		"project_id": "tenant-e",
		"state":      "pending",
	})

	n, err := store.ReconcileStaleActive(ctx)
	if err != nil {
		t.Fatalf("ReconcileStaleActive: %v", err)
	}
	if n != 2 {
		t.Errorf("reconciled = %d, want 2", n)
	}

	for _, id := range []string{"task-active-1", "task-active-2"} {
		p, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if p.State != "failed" {
			t.Errorf("%s state = %q, want failed", id, p.State)
		}
		if p.Error == "" {
			t.Errorf("%s error empty, want reconciliation message", id)
		}
	}

	for _, tc := range []struct {
		id, wantState string
	}{
		{"task-completed", "completed"},
		{"task-failed", "failed"},
		{"task-pending", "pending"},
	} {
		p, err := store.Get(ctx, tc.id)
		if err != nil {
			t.Fatalf("Get %s: %v", tc.id, err)
		}
		if p.State != tc.wantState {
			t.Errorf("%s state = %q, want %q", tc.id, p.State, tc.wantState)
		}
	}

	// task-failed should still carry its original error, not the reconciliation one.
	p, _ := store.Get(ctx, "task-failed")
	if p.Error != "some earlier error" {
		t.Errorf("task-failed error = %q, want unchanged %q", p.Error, "some earlier error")
	}
}

func TestReconcileStaleActiveEmpty(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	n, err := store.ReconcileStaleActive(ctx)
	if err != nil {
		t.Fatalf("ReconcileStaleActive on empty store: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciled on empty store = %d, want 0", n)
	}
}
