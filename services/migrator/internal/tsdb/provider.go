package tsdb

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/prometheus/prometheus/storage"
	promtsdb "github.com/prometheus/prometheus/tsdb"
)

// QuerierProvider opens a new Prometheus storage.Querier for a closed
// time interval [mint, maxt]. The returned Querier is the caller's to
// Close; the provider itself retains no reference to it.
//
// The signature mirrors *tsdb.DB.Querier(mint, maxt int64) (storage.Querier, error)
// verbatim so that *tsdb.DB structurally satisfies QuerierProvider without
// an adapter shim. Do not add a context.Context parameter — Prometheus's
// upstream *tsdb.DB.Querier does not take one in v0.310.0, and cancellation
// is already carried on the storage.Querier.Select(ctx, ...) call that
// Reader.Read issues internally.
type QuerierProvider interface {
	Querier(mint, maxt int64) (storage.Querier, error)
}

// ReaderFromPath opens the TSDB at cfg.TSDBPath (via promtsdb.Open, which
// replays the WAL into memory with RetentionDuration=0), constructs a
// Reader backed by that DB, and returns both along with an io.Closer that
// closes the underlying DB. Callers MUST defer Close() on the returned
// io.Closer after Reader.Read completes. The Reader itself does not close
// the DB — only the per-call querier it creates inside Read.
func ReaderFromPath(cfg ReaderConfig, logger *slog.Logger) (*Reader, io.Closer, error) {
	if _, err := os.Stat(cfg.TSDBPath); err != nil {
		return nil, nil, fmt.Errorf("tsdb path %q: %w", cfg.TSDBPath, err)
	}

	// tsdb.Open replays the WAL into memory and never hardlinks files, so it
	// is safe when the TSDB lives on a separate filesystem from /tmp.
	// RetentionDuration=0 disables auto-deletion.
	opts := promtsdb.DefaultOptions()
	opts.RetentionDuration = 0
	db, err := promtsdb.Open(cfg.TSDBPath, logger, nil, opts, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("opening TSDB at %q: %w", cfg.TSDBPath, err)
	}

	reader := NewReaderWithProvider(db, cfg, logger)

	// Closer returns the raw db.Close() error without logging; the caller
	// owns the single log site so a single close failure doesn't produce
	// a double-warning (see cmd/migrator/main.go).
	closer := closerFunc(func() error {
		return db.Close()
	})

	return reader, closer, nil
}

// closerFunc adapts a func() error to io.Closer.
type closerFunc func() error

// Close invokes the underlying function.
func (f closerFunc) Close() error { return f() }

// OpenProvider opens the TSDB at path and returns it as a QuerierProvider
// plus an io.Closer that closes the underlying DB. Callers MUST defer
// Close() after the consumer (e.g. migration.Run) returns.
func OpenProvider(path string, logger *slog.Logger) (QuerierProvider, io.Closer, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("tsdb path %q: %w", path, err)
	}
	opts := promtsdb.DefaultOptions()
	opts.RetentionDuration = 0
	db, err := promtsdb.Open(path, logger, nil, opts, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("opening TSDB at %q: %w", path, err)
	}
	return db, closerFunc(func() error { return db.Close() }), nil
}
