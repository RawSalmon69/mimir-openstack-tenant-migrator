package tsdb

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	promtsdb "github.com/prometheus/prometheus/tsdb"
)

// TestQuerierProviderSatisfiedByTSDBDB is a compile-time assertion that
// *promtsdb.DB satisfies QuerierProvider without an adapter. If the interface
// drifts from *tsdb.DB.Querier's upstream signature, this file will fail to
// build and surface the mismatch during `go test`.
func TestQuerierProviderSatisfiedByTSDBDB(t *testing.T) {
	var _ QuerierProvider = (*promtsdb.DB)(nil)
}

// createTestTSDB creates a TSDB in dir with known data:
//
//	tenant-a: 3 series (cpu_usage, mem_usage, disk_io) × 10 samples each
//	tenant-b: 2 series (cpu_usage, mem_usage) × 10 samples each
func createTestTSDB(t *testing.T, dir string) {
	t.Helper()

	opts := promtsdb.DefaultOptions()
	opts.RetentionDuration = 0 // no auto-deletion
	db, err := promtsdb.Open(dir, nil, nil, opts, nil)
	if err != nil {
		t.Fatalf("opening TSDB for write: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("closing TSDB: %v", err)
		}
	}()

	type seriesDef struct {
		name      string
		tenant    string
		instance  string
		job       string
		numSamples int
	}

	series := []seriesDef{
		// tenant-a: 3 series
		{"cpu_usage", "tenant-a", "host-1", "node-exporter", 10},
		{"mem_usage", "tenant-a", "host-1", "node-exporter", 10},
		{"disk_io", "tenant-a", "host-2", "node-exporter", 10},
		// tenant-b: 2 series
		{"cpu_usage", "tenant-b", "host-3", "vm-exporter", 10},
		{"mem_usage", "tenant-b", "host-3", "vm-exporter", 10},
	}

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	app := db.Appender(context.Background())
	for _, s := range series {
		lbls := labels.FromStrings(
			"__name__", s.name,
			"instance", s.instance,
			"job", s.job,
			"projectId", s.tenant,
		)
		for i := 0; i < s.numSamples; i++ {
			ts := baseTime + int64(i*30_000) // 30s intervals
			val := float64(i) * 1.5
			if _, err := app.Append(0, lbls, ts, val); err != nil {
				t.Fatalf("appending sample: %v", err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("committing samples: %v", err)
	}

	// Force a head compaction so the data is in a block (the block iterator reads sealed blocks).
	if err := db.Compact(context.Background()); err != nil {
		t.Fatalf("compacting TSDB: %v", err)
	}
}

func TestReaderFiltersByTenant(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	tests := []struct {
		name           string
		tenant         string
		wantSeries     int
		wantSamplesEach int
	}{
		{"tenant-a has 3 series", "tenant-a", 3, 10},
		{"tenant-b has 2 series", "tenant-b", 2, 10},
		{"nonexistent has 0 series", "nonexistent", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := NewReader(ReaderConfig{
				TSDBPath: dir,
				TenantID: tt.tenant,
			}, logger)

			ch := make(chan SeriesData, 100)
			errCh := make(chan error, 1)
			go func() {
				errCh <- reader.Read(context.Background(), ch)
				close(ch)
			}()

			var results []SeriesData
			for sd := range ch {
				results = append(results, sd)
			}
			if err := <-errCh; err != nil {
				t.Fatalf("Read() error: %v", err)
			}

			if got := len(results); got != tt.wantSeries {
				t.Errorf("series count = %d, want %d", got, tt.wantSeries)
			}
			for i, sd := range results {
				if tt.wantSamplesEach > 0 && len(sd.Samples) != tt.wantSamplesEach {
					t.Errorf("series[%d] samples = %d, want %d", i, len(sd.Samples), tt.wantSamplesEach)
				}
			}
		})
	}
}

func TestReaderLabelsAreSorted(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reader := NewReader(ReaderConfig{
		TSDBPath: dir,
		TenantID: "tenant-a",
	}, logger)

	ch := make(chan SeriesData, 100)
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Read(context.Background(), ch)
		close(ch)
	}()

	for sd := range ch {
		sorted := sort.SliceIsSorted(sd.Labels, func(i, j int) bool {
			return sd.Labels[i].Name < sd.Labels[j].Name
		})
		if !sorted {
			names := make([]string, len(sd.Labels))
			for i, l := range sd.Labels {
				names[i] = l.Name
			}
			t.Errorf("labels not sorted: %v", names)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Read() error: %v", err)
	}
}

func TestReaderRespectsContext(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reader := NewReader(ReaderConfig{
		TSDBPath: dir,
		TenantID: "tenant-a",
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	// Use unbuffered channel so the reader blocks after sending 1 series.
	ch := make(chan SeriesData)

	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Read(ctx, ch)
	}()

	// Receive one series, then cancel.
	select {
	case <-ch:
		// got one
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first series")
	}
	cancel()

	// Read should return with context.Canceled (or nil if it finished first).
	err := <-errCh
	if err != nil && err != context.Canceled {
		t.Fatalf("expected nil or context.Canceled, got: %v", err)
	}
}

// --- Negative tests ---

func TestReaderNonexistentPath(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reader := NewReader(ReaderConfig{
		TSDBPath: "/tmp/nonexistent-tsdb-path-12345",
		TenantID: "anything",
	}, logger)

	ch := make(chan SeriesData, 10)
	err := reader.Read(context.Background(), ch)
	if err == nil {
		t.Fatal("expected error for nonexistent path, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestReaderEmptyTenantReturnsAll(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reader := NewReader(ReaderConfig{
		TSDBPath: dir,
		TenantID: "", // empty → all series
	}, logger)

	ch := make(chan SeriesData, 100)
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Read(context.Background(), ch)
		close(ch)
	}()

	var count int
	for range ch {
		count++
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	// Total: 3 (tenant-a) + 2 (tenant-b) = 5 series.
	if count != 5 {
		t.Errorf("series count = %d, want 5 (all tenants)", count)
	}
}

func TestReaderEmptyTSDB(t *testing.T) {
	// Create a TSDB with no data — just open and close.
	dir := t.TempDir()
	opts := promtsdb.DefaultOptions()
	db, err := promtsdb.Open(dir, nil, nil, opts, nil)
	if err != nil {
		t.Fatalf("opening TSDB: %v", err)
	}
	db.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reader := NewReader(ReaderConfig{
		TSDBPath: dir,
		TenantID: "anything",
	}, logger)

	ch := make(chan SeriesData, 10)
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Read(context.Background(), ch)
		close(ch)
	}()

	var count int
	for range ch {
		count++
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	if count != 0 {
		t.Errorf("expected 0 series from empty TSDB, got %d", count)
	}
}

// --- Provider-injection tests (Task 1-01-04) ---

// TestReaderWithFakeProvider_ReturnsInjectedSeries verifies the happy path of
// the new NewReaderWithProvider constructor: a Reader backed by a FakeProvider
// emits exactly the injected SeriesData values on its output channel.
func TestReaderWithFakeProvider_ReturnsInjectedSeries(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Pre-built SeriesData — the fake provider does not filter by matcher, so
	// tests are responsible for pre-filtering. Here we inject 3 series.
	injected := []SeriesData{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "cpu_usage"},
				{Name: "projectId", Value: "tenant-a"},
			},
			Samples: []prompb.Sample{{Timestamp: 1000, Value: 0.5}},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "mem_usage"},
				{Name: "projectId", Value: "tenant-a"},
			},
			Samples: []prompb.Sample{{Timestamp: 2000, Value: 1.5}},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "disk_io"},
				{Name: "projectId", Value: "tenant-a"},
			},
			Samples: []prompb.Sample{{Timestamp: 3000, Value: 2.5}},
		},
	}

	provider := &FakeProvider{Series: injected}
	reader := NewReaderWithProvider(provider, ReaderConfig{TenantID: "tenant-a"}, logger)

	ch := make(chan SeriesData, 10)
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Read(context.Background(), ch)
		close(ch)
	}()

	var got []SeriesData
	for sd := range ch {
		got = append(got, sd)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	if len(got) != len(injected) {
		t.Fatalf("got %d series, want %d", len(got), len(injected))
	}
	for i, sd := range got {
		if len(sd.Samples) != len(injected[i].Samples) {
			t.Errorf("series[%d] samples = %d, want %d", i, len(sd.Samples), len(injected[i].Samples))
		}
		if len(sd.Labels) != len(injected[i].Labels) {
			t.Errorf("series[%d] labels = %d, want %d", i, len(sd.Labels), len(injected[i].Labels))
		}
	}
}

// TestReaderWithFakeProvider_QuerierError verifies that when provider.Querier
// returns an error, Reader.Read wraps it via "creating querier [%d, %d]: %w"
// and the wrap chain preserves errors.Is identity.
func TestReaderWithFakeProvider_QuerierError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	sentinel := errors.New("provider-boom")
	provider := &FakeProvider{QuerierErr: sentinel}
	reader := NewReaderWithProvider(provider, ReaderConfig{TenantID: "tenant-a"}, logger)

	ch := make(chan SeriesData, 1)
	err := reader.Read(context.Background(), ch)
	if err == nil {
		t.Fatal("expected error from provider.Querier, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected errors.Is(err, sentinel), got: %v", err)
	}
}

// TestReaderFromPathSmokeRoundTrip verifies the ReaderFromPath helper end to
// end: it opens a real TSDB fixture, returns a functional Reader + io.Closer,
// and produces the same observable series count as the pre-refactor inline
// open-and-close path.
func TestReaderFromPathSmokeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	createTestTSDB(t, dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reader, closer, err := ReaderFromPath(ReaderConfig{
		TSDBPath: dir,
		TenantID: "", // empty = match all series
	}, logger)
	if err != nil {
		t.Fatalf("ReaderFromPath error: %v", err)
	}
	if closer == nil {
		t.Fatal("ReaderFromPath returned nil closer")
	}
	defer func() {
		if cerr := closer.Close(); cerr != nil {
			t.Errorf("closer.Close error: %v", cerr)
		}
	}()

	ch := make(chan SeriesData, 100)
	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Read(context.Background(), ch)
		close(ch)
	}()

	var count int
	for range ch {
		count++
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Read error: %v", err)
	}

	// createTestTSDB writes 3 tenant-a + 2 tenant-b = 5 total series.
	if count != 5 {
		t.Errorf("got %d series, want 5", count)
	}
}
