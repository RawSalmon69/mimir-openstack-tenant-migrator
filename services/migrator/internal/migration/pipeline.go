// Package migration orchestrates the TSDB-to-remote-write pipeline:
// reader → batcher → writer pool.
package migration

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NIPA-Mimir/services/migrator/internal/remotewrite"
	"github.com/NIPA-Mimir/services/migrator/internal/tsdb"
	"github.com/prometheus/prometheus/prompb"
	"golang.org/x/sync/errgroup"
)

// PipelineConfig controls the migration pipeline. The caller owns the
// QuerierProvider lifecycle and must Close it after Run returns.
type PipelineConfig struct {
	Provider          tsdb.QuerierProvider
	TenantID          string
	CortexURL         string
	MaxBatchBytes     int // Maximum encoded byte size per remote write request (default 3 MiB).
	WriterConcurrency int
	DryRun            bool
	MinTime           int64
	MaxTime           int64
}

// PipelineStats holds counters from a completed pipeline run.
type PipelineStats struct {
	SeriesRead  int
	SamplesRead int
	BatchesSent int
	SamplesSent int
	Duration    time.Duration
}

// ProgressFunc is called after each series is read. Both counters are cumulative.
type ProgressFunc func(seriesRead int, samplesRead int)

// Run executes the full migration pipeline: read TSDB → batch TimeSeries →
// send via remote write. It blocks until all data is processed or an error
// occurs. The context controls cancellation.
func Run(ctx context.Context, cfg PipelineConfig, progress ProgressFunc, logger *slog.Logger) (PipelineStats, error) {
	start := time.Now()

	if cfg.Provider == nil {
		return PipelineStats{}, fmt.Errorf("pipeline: cfg.Provider is required")
	}

	if cfg.MaxBatchBytes <= 0 {
		// 3 MiB keeps 25% headroom under Mimir's 4 MiB request body limit.
		cfg.MaxBatchBytes = 3 * 1024 * 1024
	}
	if cfg.WriterConcurrency <= 0 {
		cfg.WriterConcurrency = 4
	}
	if progress == nil {
		progress = func(int, int) {}
	}

	seriesCh := make(chan tsdb.SeriesData, 64)
	batchCh := make(chan []prompb.TimeSeries, cfg.WriterConcurrency*2)

	// Mutex guards stats from concurrent writer goroutines.
	var mu sync.Mutex
	stats := PipelineStats{}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		defer close(seriesCh)
		reader := tsdb.NewReaderWithProvider(cfg.Provider, tsdb.ReaderConfig{
			TenantID: cfg.TenantID,
			MinTime:  cfg.MinTime,
			MaxTime:  cfg.MaxTime,
		}, logger)
		return reader.Read(gctx, seriesCh)
	})

	// Batcher accumulates series up to MaxBatchBytes before flushing. A single
	// series that exceeds the budget is split into sample windows so oversize
	// series are always handled safely.
	g.Go(func() error {
		defer close(batchCh)

		// The caller must NOT reuse batch's backing array after sendBatch —
		// writer goroutines read it concurrently.
		sendBatch := func(batch []prompb.TimeSeries) error {
			if len(batch) == 0 {
				return nil
			}
			select {
			case <-gctx.Done():
				return gctx.Err()
			case batchCh <- batch:
				return nil
			}
		}

		batch := make([]prompb.TimeSeries, 0, 32)
		batchBytes := 0

		for sd := range seriesCh {
			mu.Lock()
			stats.SeriesRead++
			stats.SamplesRead += len(sd.Samples)
			seriesRead := stats.SeriesRead
			samplesRead := stats.SamplesRead
			mu.Unlock()

			progress(seriesRead, samplesRead)

			if len(sd.Samples) == 0 {
				continue
			}

			ts := prompb.TimeSeries{Labels: sd.Labels, Samples: sd.Samples}
			seriesBytes := ts.Size()

			if seriesBytes <= cfg.MaxBatchBytes {
				if batchBytes+seriesBytes > cfg.MaxBatchBytes && len(batch) > 0 {
					if err := sendBatch(batch); err != nil {
						return err
					}
					// Fresh slice required — writer reads the old backing array.
					batch = make([]prompb.TimeSeries, 0, 32)
					batchBytes = 0
				}
				batch = append(batch, ts)
				batchBytes += seriesBytes
			} else {
				if len(batch) > 0 {
					if err := sendBatch(batch); err != nil {
						return err
					}
					batch = make([]prompb.TimeSeries, 0, 32)
					batchBytes = 0
				}

				// Initial sample-window estimate based on the amortized
				// per-sample byte cost (includes label overhead inflation,
				// so it tends to be conservative).
				avgSampleBytes := seriesBytes / len(sd.Samples)
				if avgSampleBytes < 1 {
					avgSampleBytes = 1
				}
				samplesPerChunk := cfg.MaxBatchBytes / avgSampleBytes
				if samplesPerChunk < 1 {
					samplesPerChunk = 1
				}

				offset := 0
				for offset < len(sd.Samples) {
					end := offset + samplesPerChunk
					if end > len(sd.Samples) {
						end = len(sd.Samples)
					}

					chunkTS := prompb.TimeSeries{
						Labels:  sd.Labels,
						Samples: sd.Samples[offset:end],
					}

					// Verify the chunk actually fits. If the estimate
					// under-counted (e.g. labels are huge or prompb encoding
					// differs from the estimate), halve the window and retry
					// until it fits or we bottom out at a single sample.
					for chunkTS.Size() > cfg.MaxBatchBytes && end-offset > 1 {
						end = offset + (end-offset)/2
						chunkTS.Samples = sd.Samples[offset:end]
					}

					if chunkSize := chunkTS.Size(); chunkSize > cfg.MaxBatchBytes {
						logger.Warn("single-sample chunk exceeds MaxBatchBytes",
							"chunk_bytes", chunkSize,
							"max_batch_bytes", cfg.MaxBatchBytes,
							"label_count", len(sd.Labels),
						)
					}

					if err := sendBatch([]prompb.TimeSeries{chunkTS}); err != nil {
						return err
					}
					offset = end
				}
			}
		}

		return sendBatch(batch)
	})

	var writer *remotewrite.Writer
	if !cfg.DryRun {
		writer = remotewrite.NewWriter(remotewrite.WriterConfig{
			URL: cfg.CortexURL,
		}, logger)
	}

	for i := 0; i < cfg.WriterConcurrency; i++ {
		g.Go(func() error {
			for batch := range batchCh {
				sampleCount := 0
				totalBytes := 0
				maxItemBytes := 0
				for i := range batch {
					sampleCount += len(batch[i].Samples)
					b := batch[i].Size()
					totalBytes += b
					if b > maxItemBytes {
						maxItemBytes = b
					}
				}

				if !cfg.DryRun {
					if err := writer.Send(gctx, batch); err != nil {
						logger.Error("remote write failed",
							"items", len(batch),
							"samples", sampleCount,
							"total_bytes", totalBytes,
							"max_item_bytes", maxItemBytes,
							"err", err,
						)
						return fmt.Errorf("remote write: %w", err)
					}
				}

				mu.Lock()
				stats.BatchesSent++
				stats.SamplesSent += sampleCount
				mu.Unlock()
			}
			return nil
		})
	}

	err := g.Wait()
	stats.Duration = time.Since(start)

	if err != nil {
		return stats, err
	}
	return stats, nil
}
