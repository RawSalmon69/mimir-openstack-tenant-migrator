package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/NIPA-Mimir/services/migrator/internal/migration"
	"github.com/NIPA-Mimir/services/migrator/internal/tsdb"
	"github.com/hibiken/asynq"
)

// MigrationHandler processes migrate:tenant tasks via asynq. OpenProvider
// returns a QuerierProvider for one ProcessTask call; the closer is
// deferred inside ProcessTask. In server mode the closure returns the
// shared *tsdb.DB with a no-op closer — the DB lifecycle is owned by
// main().
type MigrationHandler struct {
	Store        *ProgressStore
	Logger       *slog.Logger
	History      *HistoryLogger
	OpenProvider func(path string) (tsdb.QuerierProvider, io.Closer, error)
}

// ProcessTask implements asynq.Handler. It parses the task payload,
// runs the migration pipeline, and tracks progress in Redis.
func (h *MigrationHandler) ProcessTask(ctx context.Context, task *asynq.Task) error {
	payload, err := ParseMigratePayload(task)
	if err != nil {
		return fmt.Errorf("parse payload: %w", err)
	}

	taskID := taskIDFromTask(task)
	startedAt := time.Now().UTC()
	logger := h.Logger.With("task_id", taskID, "project_id", payload.ProjectID)
	logger.Info("starting migration")

	if uErr := h.Store.Update(ctx, taskID, map[string]interface{}{
		"project_id": payload.ProjectID,
		"state":      "active",
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	}); uErr != nil {
		logger.Warn("failed to update progress on start", "err", uErr)
	}

	if h.OpenProvider == nil {
		return fmt.Errorf("handler: OpenProvider is nil")
	}

	provider, closer, oErr := h.OpenProvider(payload.TSDBPath)
	if oErr != nil {
		// Mirror the migration-failed bookkeeping so a TSDB-open failure is
		// observable in Redis + history exactly like a remote-write failure.
		if uErr := h.Store.Update(ctx, taskID, map[string]interface{}{
			"state": "failed",
			"error": oErr.Error(),
		}); uErr != nil {
			logger.Warn("failed to update progress on open failure", "err", uErr)
		}
		if hErr := h.History.Append(HistoryEntry{
			TaskID:    taskID,
			ProjectID: payload.ProjectID,
			State:     "failed",
			StartedAt: startedAt,
			EndedAt:   time.Now().UTC(),
			Error:     oErr.Error(),
		}); hErr != nil {
			logger.Warn("history append failed", "err", hErr)
		}
		return fmt.Errorf("open tsdb %s: %w", payload.ProjectID, oErr)
	}
	defer func() {
		if cErr := closer.Close(); cErr != nil {
			logger.Warn("closing TSDB provider", "err", cErr)
		}
	}()

	cfg := migration.PipelineConfig{
		Provider:          provider,
		TenantID:          payload.ProjectID,
		CortexURL:         payload.CortexURL,
		MaxBatchBytes:     payload.MaxBatchBytes,
		WriterConcurrency: payload.WriterConcurrency,
		DryRun:            payload.DryRun,
	}

	// Throttle progress updates to at most one Redis write per 100 series.
	var lastReported int
	progressFn := func(seriesRead int, samplesRead int) {
		if seriesRead-lastReported < 100 && seriesRead != 0 {
			return
		}
		lastReported = seriesRead
		if uErr := h.Store.Update(ctx, taskID, map[string]interface{}{
			"series_read":  strconv.Itoa(seriesRead),
			"samples_read": strconv.Itoa(samplesRead),
		}); uErr != nil {
			logger.Warn("failed to update progress", "err", uErr)
		}
	}

	stats, err := migration.Run(ctx, cfg, progressFn, logger)
	if err != nil {
		// Return the error so asynq retries; the progress store is
		// updated first so the failure state is observable.
		if uErr := h.Store.Update(ctx, taskID, map[string]interface{}{
			"state":        "failed",
			"error":        err.Error(),
			"series_read":  strconv.Itoa(stats.SeriesRead),
			"samples_read": strconv.Itoa(stats.SamplesRead),
			"samples_sent": strconv.Itoa(stats.SamplesSent),
			"batches_sent": strconv.Itoa(stats.BatchesSent),
		}); uErr != nil {
			logger.Warn("failed to update progress on failure", "err", uErr)
		}
		if hErr := h.History.Append(HistoryEntry{
			TaskID:      taskID,
			ProjectID:   payload.ProjectID,
			State:       "failed",
			SeriesRead:  stats.SeriesRead,
			SamplesRead: stats.SamplesRead,
			SamplesSent: stats.SamplesSent,
			BatchesSent: stats.BatchesSent,
			StartedAt:   startedAt,
			EndedAt:     time.Now().UTC(),
			Error:       err.Error(),
		}); hErr != nil {
			logger.Warn("history append failed", "err", hErr)
		}
		return fmt.Errorf("migration %s: %w", payload.ProjectID, err)
	}

	if uErr := h.Store.Update(ctx, taskID, map[string]interface{}{
		"state":        "completed",
		"series_read":  strconv.Itoa(stats.SeriesRead),
		"samples_read": strconv.Itoa(stats.SamplesRead),
		"samples_sent": strconv.Itoa(stats.SamplesSent),
		"batches_sent": strconv.Itoa(stats.BatchesSent),
	}); uErr != nil {
		logger.Warn("failed to update progress on completion", "err", uErr)
	}
	if hErr := h.History.Append(HistoryEntry{
		TaskID:      taskID,
		ProjectID:   payload.ProjectID,
		State:       "completed",
		SeriesRead:  stats.SeriesRead,
		SamplesRead: stats.SamplesRead,
		SamplesSent: stats.SamplesSent,
		BatchesSent: stats.BatchesSent,
		StartedAt:   startedAt,
		EndedAt:     time.Now().UTC(),
	}); hErr != nil {
		logger.Warn("history append failed", "err", hErr)
	}

	logger.Info("migration completed",
		"series_read", stats.SeriesRead,
		"samples_sent", stats.SamplesSent,
		"duration", stats.Duration,
	)
	return nil
}

// taskIDFromTask extracts the asynq task ID. When running outside a real
// asynq server (e.g. in tests), ResultWriter is nil, so we generate a
// random fallback ID.
func taskIDFromTask(task *asynq.Task) string {
	if rw := task.ResultWriter(); rw != nil {
		return rw.TaskID()
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
