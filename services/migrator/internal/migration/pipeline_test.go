package migration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	promtsdb "github.com/prometheus/prometheus/tsdb"

	proto "github.com/gogo/protobuf/proto"
)

// createTestTSDB creates a TSDB with known data:
//
//	tenant-a: 3 series (cpu_usage, mem_usage, disk_io) × 10 samples each
//	tenant-b: 2 series (cpu_usage, mem_usage) × 5 samples each
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
		{"cpu_usage", "tenant-b", "host-3", "vm-exporter", 5},
		{"mem_usage", "tenant-b", "host-3", "vm-exporter", 5},
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

// mockRemoteWriteServer returns an httptest.Server that decodes remote write
// requests and accumulates total series and sample counts.
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

func TestPipelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	stats, err := Run(context.Background(), PipelineConfig{
		TSDBPath:          dir,
		TenantID:          "tenant-a",
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 2,
	}, nil, testLogger())

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// tenant-a: 3 series × 10 samples = 30 samples
	if stats.SeriesRead != 3 {
		t.Errorf("SeriesRead = %d, want 3", stats.SeriesRead)
	}
	if stats.SamplesRead != 30 {
		t.Errorf("SamplesRead = %d, want 30", stats.SamplesRead)
	}
	if stats.SamplesSent != 30 {
		t.Errorf("SamplesSent = %d, want 30", stats.SamplesSent)
	}

	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.series != 3 {
		t.Errorf("mock server received %d series, want 3", counter.series)
	}
	if counter.samples != 30 {
		t.Errorf("mock server received %d samples, want 30", counter.samples)
	}
}

// TestPipelineByteBudget verifies the byte-budget accumulator:
//   - Generous budget packs all 3 series into a single request.
//   - Tiny budget forces per-sample splitting across many requests.
func TestPipelineByteBudget(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	// Sub-case 1: generous budget — all 3 series × 10 samples should pack
	// into a single remote write request (key slice success criterion).
	t.Run("generous_budget", func(t *testing.T) {
		counter := &writeCounter{}
		srv := newMockServer(t, counter)
		defer srv.Close()

		_, err := Run(context.Background(), PipelineConfig{
			TSDBPath:          dir,
			TenantID:          "tenant-a",
			CortexURL:         srv.URL,
			MaxBatchBytes:     10 * 1024 * 1024, // 10 MiB — trivially large
			WriterConcurrency: 1,
		}, nil, testLogger())
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		counter.mu.Lock()
		defer counter.mu.Unlock()
		if counter.requests != 1 {
			t.Errorf("requests = %d, want 1 (all series packed into one batch)", counter.requests)
		}
		if counter.series != 3 {
			t.Errorf("series = %d, want 3", counter.series)
		}
		if counter.samples != 30 {
			t.Errorf("samples = %d, want 30", counter.samples)
		}
	})

	// Sub-case 2: tiny budget — each 10-sample series must be split across
	// multiple requests; total samples must equal 30 (no loss).
	// Note: a 10-sample series encodes to ~259 bytes; using 128 bytes forces
	// each series to be split into multiple chunks (avgSampleBytes≈25,
	// samplesPerChunk≈5, so 2+ chunks per 10-sample series → >3 requests).
	t.Run("tiny_budget", func(t *testing.T) {
		counter := &writeCounter{}
		srv := newMockServer(t, counter)
		defer srv.Close()

		_, err := Run(context.Background(), PipelineConfig{
			TSDBPath:          dir,
			TenantID:          "tenant-a",
			CortexURL:         srv.URL,
			MaxBatchBytes:     128, // forces split path: ~259B series → ~2 chunks each
			WriterConcurrency: 1,
		}, nil, testLogger())
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		counter.mu.Lock()
		defer counter.mu.Unlock()
		if counter.requests <= 3 {
			t.Errorf("requests = %d, want > 3 (tiny budget should split each series)", counter.requests)
		}
		if counter.samples != 30 {
			t.Errorf("samples = %d, want 30 (no samples lost during splitting)", counter.samples)
		}
	})
}

// TestPipelineBatchSplitting verifies the oversize-series split path using a
// tiny MaxBatchBytes budget instead of the legacy BatchSize knob.
func TestPipelineBatchSplitting(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	stats, err := Run(context.Background(), PipelineConfig{
		TSDBPath:          dir,
		TenantID:          "tenant-a",
		CortexURL:         srv.URL,
		MaxBatchBytes:     128, // tiny budget: ~259B series → split into multiple chunks
		WriterConcurrency: 1,
	}, nil, testLogger())

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// With a 128-byte budget, each 10-sample series (~259B encoded) is split
	// into multiple chunks → more than 3 total requests.
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.requests <= 3 {
		t.Errorf("mock server received %d requests, want > 3 (split path)", counter.requests)
	}
	if stats.BatchesSent <= 3 {
		t.Errorf("BatchesSent = %d, want > 3", stats.BatchesSent)
	}
	if counter.samples != 30 {
		t.Errorf("mock server received %d samples, want 30 (no data loss)", counter.samples)
	}
}

func TestPipelineDryRun(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	stats, err := Run(context.Background(), PipelineConfig{
		TSDBPath:          dir,
		TenantID:          "tenant-a",
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 2,
		DryRun:            true,
	}, nil, testLogger())

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if stats.SamplesRead != 30 {
		t.Errorf("SamplesRead = %d, want 30", stats.SamplesRead)
	}

	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.requests != 0 {
		t.Errorf("mock server received %d requests in dry-run, want 0", counter.requests)
	}
}

func TestPipelineProgressCallback(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	counter := &writeCounter{}
	srv := newMockServer(t, counter)
	defer srv.Close()

	var mu sync.Mutex
	var calls []int // series counts recorded by callback

	_, err := Run(context.Background(), PipelineConfig{
		TSDBPath:          dir,
		TenantID:          "tenant-a",
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 2,
	}, func(seriesRead int, _ int) {
		mu.Lock()
		calls = append(calls, seriesRead)
		mu.Unlock()
	}, testLogger())

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("progress called %d times, want 3", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if calls[i] <= calls[i-1] {
			t.Errorf("progress not monotonically increasing: calls[%d]=%d, calls[%d]=%d", i-1, calls[i-1], i, calls[i])
		}
	}
}

// --- Negative / error path tests ---

func TestPipelineWriterError(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	// Server that always returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("simulated failure"))
	}))
	defer srv.Close()

	_, err := Run(context.Background(), PipelineConfig{
		TSDBPath:          dir,
		TenantID:          "tenant-a",
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 1,
	}, nil, testLogger())

	if err == nil {
		t.Fatal("expected error from failing writer, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestPipelineNoMatchingSeries(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	var reqCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stats, err := Run(context.Background(), PipelineConfig{
		TSDBPath:          dir,
		TenantID:          "nonexistent-tenant",
		CortexURL:         srv.URL,
		MaxBatchBytes:     5 * 1024 * 1024,
		WriterConcurrency: 1,
	}, nil, testLogger())

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if stats.SeriesRead != 0 {
		t.Errorf("SeriesRead = %d, want 0", stats.SeriesRead)
	}
	if stats.SamplesRead != 0 {
		t.Errorf("SamplesRead = %d, want 0", stats.SamplesRead)
	}
	if reqCount.Load() != 0 {
		t.Errorf("server received %d requests for nonexistent tenant, want 0", reqCount.Load())
	}
}
