// Command migrator reads a Prometheus TSDB block directory and writes
// matching series to a cortex-tenant endpoint via remote write.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/NIPA-Mimir/services/migrator/internal/migration"
	"github.com/NIPA-Mimir/services/migrator/internal/tsdb"
)

func main() {
	tsdbPath := flag.String("tsdb-path", "", "Path to the TSDB directory (required)")
	cortexURL := flag.String("cortex-url", "", "Remote write endpoint URL (required)")
	tenant := flag.String("tenant", "", "projectId to filter (empty = all tenants)")
	maxBatchBytes := flag.Int("max-batch-bytes", 3*1024*1024, "Max bytes per remote write request (byte-budget batcher)")
	writers := flag.Int("writers", 4, "Number of concurrent writer goroutines")
	dryRun := flag.Bool("dry-run", false, "Read and count without sending")
	minTime := flag.Int64("min-time", 0, "Minimum timestamp (ms epoch), 0 = no lower bound")
	maxTime := flag.Int64("max-time", 0, "Maximum timestamp (ms epoch), 0 = no upper bound")
	flag.Parse()

	if *tsdbPath == "" || *cortexURL == "" {
		fmt.Fprintf(os.Stderr, "Usage: migrator --tsdb-path <path> --cortex-url <url> [flags]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cfg := migration.PipelineConfig{
		TenantID:          *tenant,
		CortexURL:         *cortexURL,
		MaxBatchBytes:     *maxBatchBytes,
		WriterConcurrency: *writers,
		DryRun:            *dryRun,
		MinTime:           *minTime,
		MaxTime:           *maxTime,
	}

	provider, closer, err := tsdb.OpenProvider(*tsdbPath, logger)
	if err != nil {
		logger.Error("failed to open TSDB", "err", err, "tsdb_path", *tsdbPath)
		os.Exit(1)
	}
	defer func() {
		if cErr := closer.Close(); cErr != nil {
			logger.Warn("closing TSDB", "err", cErr)
		}
	}()
	cfg.Provider = provider

	progress := func(seriesRead, samplesRead int) {
		fmt.Fprintf(os.Stderr, "\r  series: %d  samples: %d", seriesRead, samplesRead)
	}

	stats, err := migration.Run(ctx, cfg, progress, logger)
	fmt.Fprintln(os.Stderr)

	if err != nil {
		logger.Error("migration failed", "err", err, "duration", stats.Duration.Round(time.Millisecond))
		os.Exit(1)
	}

	fmt.Printf("Migration complete: series=%d samples_read=%d batches=%d samples_sent=%d duration=%s dry_run=%v\n",
		stats.SeriesRead, stats.SamplesRead, stats.BatchesSent, stats.SamplesSent,
		stats.Duration.Round(time.Millisecond), cfg.DryRun)
}
