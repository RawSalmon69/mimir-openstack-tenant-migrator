package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/NIPA-Mimir/services/migrator/internal/tsdb"
)

// countingCloser wraps the io.Closer returned by OpenProvider and increments
// a counter on Close. Used to prove the deferred Close in ProcessTask runs
// on every exit path.
type countingCloser struct {
	count *atomic.Int32
}

func (c *countingCloser) Close() error {
	c.count.Add(1)
	return nil
}

// TestSigtermDrainCleansUp checks two per-task invariants required for the
// shutdown sequence (asynq drain → dbCloser.Close → http.Shutdown → redis
// close) to avoid leaks:
//
//  1. The deferred closer from MigrationHandler.OpenProvider ALWAYS runs
//     on both the success path and the Querier-error path.
//  2. The final Redis state is `completed` or `failed` — never `active`
//     (which would orphan ReconcileStaleActive on the next boot).
func TestSigtermDrainCleansUp(t *testing.T) {
	t.Run("success_path_runs_deferred_closer", func(t *testing.T) {
		mr, err := miniredis.Run()
		if err != nil {
			t.Fatalf("miniredis: %v", err)
		}
		defer mr.Close()

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		store := NewProgressStore(rdb)

		var closerCount atomic.Int32
		var historyBuf bytes.Buffer

		// Empty FakeProvider — pipeline opens, finds no series, completes
		// cleanly. CortexURL never receives a request because there's
		// nothing to send.
		fakeProvider := &tsdb.FakeProvider{Series: []tsdb.SeriesData{}}

		handler := &MigrationHandler{
			Store:   store,
			Logger:  testLogger(),
			History: NewHistoryLogger(&historyBuf),
			OpenProvider: func(_ string) (tsdb.QuerierProvider, io.Closer, error) {
				return fakeProvider, &countingCloser{count: &closerCount}, nil
			},
		}

		task := makeTask(t, MigratePayload{
			ProjectID:         "tenant-a",
			TSDBPath:          "/dev/null",
			CortexURL:         "http://example.invalid:1/push",
			MaxBatchBytes:     5 * 1024 * 1024,
			WriterConcurrency: 1,
		})

		if err := handler.ProcessTask(context.Background(), task); err != nil {
			t.Fatalf("ProcessTask (success path): %v", err)
		}

		if got := closerCount.Load(); got != 1 {
			t.Errorf("success path: closer ran %d times, expected 1", got)
		}

		assertTerminalRedisState(t, store, "completed")
		assertHistoryStateContains(t, &historyBuf, "completed")
	})

	t.Run("failure_path_runs_deferred_closer", func(t *testing.T) {
		mr, err := miniredis.Run()
		if err != nil {
			t.Fatalf("miniredis: %v", err)
		}
		defer mr.Close()

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		store := NewProgressStore(rdb)

		var closerCount atomic.Int32
		var historyBuf bytes.Buffer

		// FakeProvider that returns an error from Querier — simulates a
		// provider that opened cleanly but fails mid-read (e.g. underlying
		// *tsdb.DB closed by a racing shutdown). The handler must STILL
		// run its deferred closer (the closer was returned successfully)
		// AND record state=failed in Redis (NOT leave it as `active`).
		fakeProvider := &tsdb.FakeProvider{
			QuerierErr: errors.New("simulated TSDB closed mid-task"),
		}

		handler := &MigrationHandler{
			Store:   store,
			Logger:  testLogger(),
			History: NewHistoryLogger(&historyBuf),
			OpenProvider: func(_ string) (tsdb.QuerierProvider, io.Closer, error) {
				return fakeProvider, &countingCloser{count: &closerCount}, nil
			},
		}

		task := makeTask(t, MigratePayload{
			ProjectID:         "tenant-b",
			TSDBPath:          "/dev/null",
			CortexURL:         "http://example.invalid:1/push",
			MaxBatchBytes:     5 * 1024 * 1024,
			WriterConcurrency: 1,
		})

		err = handler.ProcessTask(context.Background(), task)
		if err == nil {
			t.Fatal("ProcessTask (failure path): expected error from Querier, got nil")
		}

		if got := closerCount.Load(); got != 1 {
			t.Errorf("failure path: closer ran %d times after Querier error, expected 1", got)
		}

		assertTerminalRedisState(t, store, "failed")
		assertHistoryStateContains(t, &historyBuf, "failed")
	})
}

// assertTerminalRedisState verifies Redis has at least one progress entry and
// that no entry is in `active` state.
func assertTerminalRedisState(t *testing.T, store *ProgressStore, want string) {
	t.Helper()
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected at least one progress entry after ProcessTask")
	}
	sawWant := false
	for _, p := range list {
		switch p.State {
		case want:
			sawWant = true
		case "completed", "failed":
			// Acceptable terminal state even if not the one we asked for.
		case "active":
			t.Errorf("progress entry %q left in `active` after ProcessTask returned — would orphan ReconcileStaleActive on restart", p.TaskID)
		default:
			t.Errorf("unexpected state %q on progress entry %q (expected %q or other terminal)", p.State, p.TaskID, want)
		}
	}
	if !sawWant {
		t.Errorf("expected at least one entry with state=%q, found none", want)
	}
}

// assertHistoryStateContains scans the JSON-line history buffer for a
// state field matching the expected terminal state.
func assertHistoryStateContains(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	raw := buf.String()
	if raw == "" {
		t.Fatal("expected history line written; buffer empty")
	}
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		var entry HistoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("history line not JSON: %q (%v)", line, err)
			continue
		}
		if entry.State == want {
			return
		}
	}
	t.Errorf("no history entry with state=%q found in %q", want, raw)
}

// TestProgressReconcileStaleActiveScope plants an orphan `active` Redis
// entry as if a previous pod was SIGKILL'd mid-task and verifies
// ReconcileStaleActive transitions it to `failed` with a meaningful
// error. The second run verifies idempotence.
func TestProgressReconcileStaleActiveScope(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := NewProgressStore(rdb)
	ctx := context.Background()

	taskID := "orphan-test-task"
	if err := store.Update(ctx, taskID, map[string]interface{}{
		"project_id": "tenant-orphan",
		"state":      "active",
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	n, rErr := store.ReconcileStaleActive(ctx)
	if rErr != nil {
		t.Fatalf("ReconcileStaleActive: %v", rErr)
	}
	if n != 1 {
		t.Errorf("ReconcileStaleActive transitioned %d entries, expected 1", n)
	}

	list, lErr := store.List(ctx)
	if lErr != nil {
		t.Fatalf("store.List: %v", lErr)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 progress entry, got %d", len(list))
	}
	if list[0].State != "failed" {
		t.Errorf("ReconcileStaleActive left state=%q, expected `failed`", list[0].State)
	}
	if list[0].Error == "" {
		t.Errorf("ReconcileStaleActive set state=failed without populating error field — operators have no signal")
	}

	n2, _ := store.ReconcileStaleActive(ctx)
	if n2 != 0 {
		t.Errorf("ReconcileStaleActive on already-failed entries transitioned %d (expected 0 — must be idempotent)", n2)
	}
}

// Compile-time guard.
var _ io.Closer = (*countingCloser)(nil)
