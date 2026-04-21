// Package tsdb provides a filtered reader for Prometheus TSDB blocks.
// It opens a TSDB directory via tsdb.Open (WAL replay into memory, no
// sandbox or hardlinks required), queries series matching a projectId
// label, and streams the results through a channel.
package tsdb

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	promtsdb "github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// ReaderConfig controls which data the reader selects. TSDBPath is consumed
// only by ReaderFromPath and by the NewReader path-fallback codepath; the
// injection codepath (NewReaderWithProvider) ignores TSDBPath.
type ReaderConfig struct {
	// TSDBPath is the filesystem path to the TSDB directory. Consumed only by
	// ReaderFromPath and by the NewReader path-fallback codepath.
	TSDBPath string
	// TenantID is the value of the "projectId" label to filter on.
	// An empty string means "match all series" (no projectId filter).
	TenantID string
	// MinTime is the minimum timestamp (ms) to query. 0 means math.MinInt64.
	MinTime int64
	// MaxTime is the maximum timestamp (ms) to query. 0 means math.MaxInt64.
	MaxTime int64
}

// SeriesData holds the labels and samples for a single time series.
type SeriesData struct {
	// Labels must be sorted by name — Mimir rejects unsorted label sets.
	Labels  []prompb.Label
	Samples []prompb.Sample
}

// Reader reads filtered series from a QuerierProvider. It is single-goroutine:
// callers must not invoke Read concurrently on the same Reader. A Reader
// constructed with NewReader has a nil provider and will open the TSDB at
// cfg.TSDBPath inside Read (path-fallback mode). A Reader constructed with
// NewReaderWithProvider uses the injected provider and never touches
// cfg.TSDBPath.
type Reader struct {
	provider QuerierProvider
	cfg      ReaderConfig
	logger   *slog.Logger
}

// NewReader creates a Reader in path-fallback mode. Its Read method opens the
// TSDB at cfg.TSDBPath on each call. Prefer NewReaderWithProvider — the
// path-fallback codepath exists only for callers that cannot inject a
// provider.
func NewReader(cfg ReaderConfig, logger *slog.Logger) *Reader {
	return &Reader{provider: nil, cfg: cfg, logger: logger}
}

// NewReaderWithProvider creates a Reader backed by an injected QuerierProvider.
// The Reader does not own the provider's lifecycle — callers must manage any
// underlying resources (e.g. *tsdb.DB.Close) themselves. Each Read call opens
// and closes its own per-call storage.Querier via provider.Querier(mint, maxt).
func NewReaderWithProvider(provider QuerierProvider, cfg ReaderConfig, logger *slog.Logger) *Reader {
	return &Reader{provider: provider, cfg: cfg, logger: logger}
}

// Read opens a per-call querier from the Reader's QuerierProvider, queries
// series matching the configured projectId, and sends each series as a
// SeriesData value on ch. The channel is NOT closed by Read — the caller owns
// channel lifecycle.
//
// Read returns nil when all matching series have been sent, or an error
// if the querier cannot be created / iterated. It respects ctx cancellation
// and will return ctx.Err() if the context is cancelled mid-iteration.
//
// If the Reader was constructed via NewReader (nil provider), Read opens the
// TSDB at cfg.TSDBPath for the duration of this call and closes it on return.
func (r *Reader) Read(ctx context.Context, ch chan<- SeriesData) error {
	minT, maxT := r.cfg.MinTime, r.cfg.MaxTime
	if minT == 0 {
		minT = math.MinInt64
	}
	if maxT == 0 {
		maxT = math.MaxInt64
	}

	provider := r.provider
	if provider == nil {
		if _, err := os.Stat(r.cfg.TSDBPath); err != nil {
			return fmt.Errorf("tsdb path %q: %w", r.cfg.TSDBPath, err)
		}
		// tsdb.Open replays the WAL into memory and never hardlinks files, so it
		// is safe when the TSDB lives on a separate filesystem from /tmp.
		// RetentionDuration=0 disables auto-deletion.
		opts := promtsdb.DefaultOptions()
		opts.RetentionDuration = 0
		db, err := promtsdb.Open(r.cfg.TSDBPath, r.logger, nil, opts, nil)
		if err != nil {
			return fmt.Errorf("opening TSDB at %q: %w", r.cfg.TSDBPath, err)
		}
		defer func() {
			if cerr := db.Close(); cerr != nil {
				r.logger.Warn("closing TSDB", "err", cerr)
			}
		}()
		provider = db
	}

	querier, err := provider.Querier(minT, maxT)
	if err != nil {
		return fmt.Errorf("creating querier [%d, %d]: %w", minT, maxT, err)
	}
	defer querier.Close()

	var matchers []*labels.Matcher
	if r.cfg.TenantID != "" {
		matchers = append(matchers, labels.MustNewMatcher(labels.MatchEqual, "projectId", r.cfg.TenantID))
	} else {
		matchers = append(matchers, labels.MustNewMatcher(labels.MatchRegexp, "__name__", ".+"))
	}

	ss := querier.Select(ctx, false, nil, matchers...)
	seriesCount := 0

	for ss.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}

		series := ss.At()
		lbls := series.Labels()

		promLabels := make([]prompb.Label, 0, lbls.Len())
		lbls.Range(func(l labels.Label) {
			promLabels = append(promLabels, prompb.Label{
				Name:  l.Name,
				Value: l.Value,
			})
		})

		// Mimir rejects unsorted label sets.
		sort.Slice(promLabels, func(i, j int) bool {
			return promLabels[i].Name < promLabels[j].Name
		})

		it := series.Iterator(nil)
		var samples []prompb.Sample
		for it.Next() == chunkenc.ValFloat {
			ts, val := it.At()
			samples = append(samples, prompb.Sample{
				Timestamp: ts,
				Value:     val,
			})
		}
		if err := it.Err(); err != nil {
			return fmt.Errorf("iterating samples for series %d: %w", seriesCount, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- SeriesData{Labels: promLabels, Samples: samples}:
		}

		seriesCount++
	}

	if err := ss.Err(); err != nil {
		return fmt.Errorf("iterating series set: %w", err)
	}

	r.logger.Info("TSDB read complete", "series", seriesCount)
	return nil
}
