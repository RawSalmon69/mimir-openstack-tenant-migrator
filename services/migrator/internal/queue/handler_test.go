package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang/snappy"
	"github.com/hibiken/asynq"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	promtsdb "github.com/prometheus/prometheus/tsdb"
	"github.com/redis/go-redis/v9"

	proto "github.com/gogo/protobuf/proto"

	"github.com/NIPA-Mimir/services/migrator/internal/tsdb"
)

// realOpenProvider returns the production tsdb.OpenProvider closure used in
// tests so handler tests exercise the full read pipeline against the real
// fixture TSDB created by createTestTSDB.
func realOpenProvider(logger *slog.Logger) func(string) (tsdb.QuerierProvider, io.Closer, error) {
	return func(path string) (tsdb.QuerierProvider, io.Closer, error) {
		return tsdb.OpenProvider(path, logger)
	}
}

// --- Test helpers (duplicated from migration package, unexported there) ---

func createTestTSDB(t *testing.T, dir string) {
	t.Helper()
	opts := promtsdb.DefaultOptions()
	opts.RetentionDuration = 0
	db, err := promtsdb.Open(dir, nil, nil, opts, nil)
	if err != nil {
		t.Fatalf("opening TSDB: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("closing TSDB: %v", err)
		}
	}()

	type sd struct {
		name, tenant, instance, job string
		n                           int
	}
	series := []sd{
		{"cpu_usage", "tenant-a", "host-1", "node-exporter", 10},
		{"mem_usage", "tenant-a", "host-1", "node-exporter", 10},
		{"disk_io", "tenant-a", "host-2", "node-exporter", 10},
	}

	baseTime := int64(1735689600000) // 2025-01-01T00:00:00Z
	app := db.Appender(context.Background())
	for _, s := range series {
		lbls := labels.FromStrings(
			"__name__", s.name,
			"instance", s.instance,
			"job", s.job,
			"projectId", s.tenant,
		)
		for i := 0; i < s.n; i++ {
			ts := baseTime + int64(i*30_000)
			if _, err := app.Append(0, lbls, ts, float64(i)*1.5); err != nil {
				t.Fatalf("appending sample: %v", err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("committing: %v", err)
	}
	if err := db.Compact(context.Background()); err != nil {
		t.Fatalf("compacting: %v", err)
	}
}

type writeCounter struct {
	mu       sync.Mutex
	requests int
	series   int
	samples  int
}

func newMockServer(t *testing.T, counter *writeCounter) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		decoded, err := snappy.Decode(nil, body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var wr prompb.WriteRequest
		if err := proto.Unmarshal(decoded, &wr); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		counter.mu.Lock()
		counter.requests++
		counter.series += len(wr.Timeseries)
		for _, ts := range wr.Timeseries {
			counter.samples += len(ts.Samples)
		}
		counter.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func newTestHandler(t *testing.T, mr *miniredis.Miniredis) (*MigrationHandler, *ProgressStore) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	store := NewProgressStore(rdb)
	logger := testLogger()
	handler := &MigrationHandler{
		Store:        store,
		Logger:       logger,
		OpenProvider: realOpenProvider(logger),
	}
	return handler, store
}

// makeTask builds an asynq.Task with a fake task ID injected via the standard constructor.
func makeTask(t *testing.T, payload MigratePayload) *asynq.Task {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return asynq.NewTask(TypeMigrateTenant, data)
}

// --- Tests ---

func TestHandlerSuccess(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	handler, store := newTestHandler(t, mr)
	task := makeTask(t, MigratePayload{
		ProjectID:         "tenant-a",
		TSDBPath:          dir,
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 2,
	})

	if err := handler.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	// Verify progress was recorded. The task ID from asynq.NewTask is empty
	// when not enqueued through a real client, so we check via List.
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected at least one progress entry")
	}
	p := list[0]
	if p.State != "completed" {
		t.Errorf("state = %q, want completed", p.State)
	}
	if p.SamplesRead != 30 {
		t.Errorf("SamplesRead = %d, want 30", p.SamplesRead)
	}
	if p.SeriesRead != 3 {
		t.Errorf("SeriesRead = %d, want 3", p.SeriesRead)
	}

	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.samples != 30 {
		t.Errorf("remote write received %d samples, want 30", counter.samples)
	}
}

func TestHandlerInvalidPayload(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	handler, _ := newTestHandler(t, mr)

	// Malformed JSON.
	badTask := asynq.NewTask(TypeMigrateTenant, []byte("not-json"))
	if err := handler.ProcessTask(context.Background(), badTask); err == nil {
		t.Fatal("expected error for malformed JSON payload")
	}

	// Empty project ID.
	emptyTask := makeTask(t, MigratePayload{ProjectID: ""})
	if err := handler.ProcessTask(context.Background(), emptyTask); err == nil {
		t.Fatal("expected error for empty project_id")
	}
}

func TestHandlerRemoteWriteFailure(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	// Server that always 500s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	handler, store := newTestHandler(t, mr)
	task := makeTask(t, MigratePayload{
		ProjectID:         "tenant-a",
		TSDBPath:          dir,
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 1,
	})

	err = handler.ProcessTask(context.Background(), task)
	if err == nil {
		t.Fatal("expected error from failing remote write")
	}

	// Progress should show failed state.
	list, lErr := store.List(context.Background())
	if lErr != nil {
		t.Fatalf("List: %v", lErr)
	}
	if len(list) == 0 {
		t.Fatal("expected progress entry after failure")
	}
	p := list[0]
	if p.State != "failed" {
		t.Errorf("state = %q, want failed", p.State)
	}
	if p.Error == "" {
		t.Error("expected non-empty error message in progress")
	}
}

func TestProcessTaskWritesHistory(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	// --- success case ---
	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	var buf bytes.Buffer
	histLogger := NewHistoryLogger(&buf)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	store := NewProgressStore(rdb)
	logger := testLogger()
	handler := &MigrationHandler{
		Store:        store,
		Logger:       logger,
		History:      histLogger,
		OpenProvider: realOpenProvider(logger),
	}

	task := makeTask(t, MigratePayload{
		ProjectID:         "tenant-a",
		TSDBPath:          dir,
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 2,
	})

	if err := handler.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("ProcessTask (success): %v", err)
	}

	raw := buf.String()
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 history line after success, got %d: %q", len(lines), raw)
	}
	var entry1 HistoryEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry1); err != nil {
		t.Fatalf("unmarshal history line: %v", err)
	}
	if entry1.State != "completed" {
		t.Errorf("state = %q, want completed", entry1.State)
	}
	if entry1.SamplesRead != 30 {
		t.Errorf("SamplesRead = %d, want 30", entry1.SamplesRead)
	}
	if entry1.Error != "" {
		t.Errorf("Error = %q, want empty for success", entry1.Error)
	}

	// --- failure case: point at an unreachable address (use tenant-a so TSDB has data to write) ---
	failTask := makeTask(t, MigratePayload{
		ProjectID:         "tenant-a",
		TSDBPath:          dir,
		CortexURL:         "http://127.0.0.1:1/push",
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 1,
	})

	// ProcessTask should return error (failure), but history should still be appended.
	_ = handler.ProcessTask(context.Background(), failTask)

	raw2 := buf.String()
	lines2 := strings.Split(strings.TrimRight(raw2, "\n"), "\n")
	if len(lines2) != 2 {
		t.Fatalf("expected 2 history lines after failure, got %d: %q", len(lines2), raw2)
	}
	var entry2 HistoryEntry
	if err := json.Unmarshal([]byte(lines2[1]), &entry2); err != nil {
		t.Fatalf("unmarshal failure history line: %v", err)
	}
	if entry2.State != "failed" {
		t.Errorf("state = %q, want failed", entry2.State)
	}
	if entry2.Error == "" {
		t.Error("Error should be non-empty for failure case")
	}
}

func TestHandlerProgressUpdates(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	handler, store := newTestHandler(t, mr)
	task := makeTask(t, MigratePayload{
		ProjectID:         "tenant-a",
		TSDBPath:          dir,
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 1,
	})

	if err := handler.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	// After completion, final stats should be in Redis.
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 progress entry, got %d", len(list))
	}
	p := list[0]
	if p.SamplesSent != 30 {
		t.Errorf("SamplesSent = %d, want 30", p.SamplesSent)
	}
	if p.BatchesSent == 0 {
		t.Error("BatchesSent should be > 0")
	}
}
