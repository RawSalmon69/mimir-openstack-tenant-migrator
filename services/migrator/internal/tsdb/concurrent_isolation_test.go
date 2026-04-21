package tsdb

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	promtsdb "github.com/prometheus/prometheus/tsdb"
)

// createMultiTenantTestTSDB writes a TSDB with 4 tenants × 6 series × 10
// samples so the concurrent-isolation test has enough fan-out to exercise
// the shared-DB goroutine-safety contract.
func createMultiTenantTestTSDB(t *testing.T, dir string) {
	t.Helper()

	opts := promtsdb.DefaultOptions()
	opts.RetentionDuration = 0
	db, err := promtsdb.Open(dir, nil, nil, opts, nil)
	if err != nil {
		t.Fatalf("opening TSDB for write: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("closing TSDB: %v", err)
		}
	}()

	tenants := []string{"tenant-a", "tenant-b", "tenant-c", "tenant-d"}
	metrics := []string{"cpu_usage", "mem_usage", "disk_io", "net_rx", "net_tx", "fd_open"}
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	app := db.Appender(context.Background())
	for _, tenant := range tenants {
		for _, metric := range metrics {
			lbls := labels.FromStrings(
				"__name__", metric,
				"projectId", tenant,
				"instance", tenant+"-host-1",
				"job", "node-exporter",
			)
			for i := 0; i < 10; i++ {
				ts := baseTime + int64(i*30_000)
				val := float64(i) * 1.5
				if _, err := app.Append(0, lbls, ts, val); err != nil {
					t.Fatalf("appending sample for %s/%s: %v", tenant, metric, err)
				}
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

// TestConcurrentSelectIsolation validates zero tenancy leaks under the
// shared-DB concurrency model. Multiple goroutines call
// db.Querier(...).Select(projectId=X) concurrently against one shared
// *tsdb.DB; each must see only its tenant's series. Run under -race.
func TestConcurrentSelectIsolation(t *testing.T) {
	dir := t.TempDir()
	createMultiTenantTestTSDB(t, dir)

	provider, closer, err := OpenProvider(dir, nil)
	if err != nil {
		t.Fatalf("OpenProvider: %v", err)
	}
	defer func() {
		if cErr := closer.Close(); cErr != nil {
			t.Errorf("closer.Close: %v", cErr)
		}
	}()

	tenants := []string{"tenant-a", "tenant-b", "tenant-c", "tenant-d"}
	expectedSeriesPerTenant := 6 // cpu_usage, mem_usage, disk_io, net_rx, net_tx, fd_open

	// Run at least len(tenants) goroutines concurrently, but bump up if the
	// machine has more CPUs (more pressure on the shared *tsdb.DB).
	goroutineCount := runtime.NumCPU()
	if goroutineCount < len(tenants) {
		goroutineCount = len(tenants)
	}

	type result struct {
		tenant      string
		seenSeries  int
		foreignHits []string // labels we should not have seen
		err         error
	}

	results := make(chan result, goroutineCount)
	var wg sync.WaitGroup

	ctx := context.Background()

	for i := 0; i < goroutineCount; i++ {
		tenant := tenants[i%len(tenants)]
		wg.Add(1)
		go func(tenant string) {
			defer wg.Done()

			r := result{tenant: tenant}

			querier, qErr := provider.Querier(0, time.Now().UnixMilli())
			if qErr != nil {
				r.err = fmt.Errorf("Querier: %w", qErr)
				results <- r
				return
			}
			defer func() { _ = querier.Close() }()

			matcher := labels.MustNewMatcher(labels.MatchEqual, "projectId", tenant)
			ss := querier.Select(ctx, false, nil, matcher)

			for ss.Next() {
				series := ss.At()
				lbls := series.Labels()
				projectID := lbls.Get("projectId")
				if projectID != tenant {
					r.foreignHits = append(r.foreignHits,
						fmt.Sprintf("series %v has projectId=%q (expected %q)", lbls.String(), projectID, tenant))
					continue
				}
				r.seenSeries++
			}
			if sErr := ss.Err(); sErr != nil {
				r.err = fmt.Errorf("series-set iteration: %w", sErr)
			}

			results <- r
		}(tenant)
	}

	wg.Wait()
	close(results)

	// Drain results and verify every goroutine saw EXACTLY its tenant's series
	// and zero foreign series.
	tenantSeriesCounts := make(map[string]int)
	totalForeignHits := 0
	for r := range results {
		if r.err != nil {
			t.Errorf("goroutine for tenant %q errored: %v", r.tenant, r.err)
			continue
		}
		if r.seenSeries != expectedSeriesPerTenant {
			t.Errorf("tenant %q: saw %d series, expected %d (every goroutine must see every series for its tenant)",
				r.tenant, r.seenSeries, expectedSeriesPerTenant)
		}
		if len(r.foreignHits) > 0 {
			totalForeignHits += len(r.foreignHits)
			t.Errorf("CROSS-TENANT LEAK for tenant %q (%d foreign hits):\n  %v",
				r.tenant, len(r.foreignHits), r.foreignHits)
		}
		tenantSeriesCounts[r.tenant] += r.seenSeries
	}

	if totalForeignHits > 0 {
		t.Fatalf("cross-tenant leak: %d foreign series visible across %d goroutines — shared *tsdb.DB is not goroutine-safe under concurrent Select() with distinct projectId matchers",
			totalForeignHits, goroutineCount)
	}

	// Sanity: every requested tenant must have been queried at least once.
	for _, tenant := range tenants {
		if tenantSeriesCounts[tenant] == 0 {
			t.Errorf("tenant %q was scheduled but no goroutine reported its series count — possible goroutine leak or panic-without-error", tenant)
		}
	}

	t.Logf("isolation held: %d goroutines × %d series/tenant = %d total reads, zero cross-tenant series",
		goroutineCount, expectedSeriesPerTenant, goroutineCount*expectedSeriesPerTenant)
}
