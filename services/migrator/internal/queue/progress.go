package queue

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// TaskProgress represents the progress of a single migration task.
type TaskProgress struct {
	TaskID      string    `json:"task_id"`
	ProjectID   string    `json:"project_id"`
	State       string    `json:"state"`
	SeriesRead  int       `json:"series_read"`
	SamplesRead int       `json:"samples_read"`
	SamplesSent int       `json:"samples_sent"`
	BatchesSent int       `json:"batches_sent"`
	StartedAt   time.Time `json:"started_at"`
	Error       string    `json:"error,omitempty"`
}

// ProgressStore persists migration progress in Redis hashes.
type ProgressStore struct {
	rdb *redis.Client
}

// NewProgressStore returns a store backed by the given Redis client.
func NewProgressStore(rdb *redis.Client) *ProgressStore {
	return &ProgressStore{rdb: rdb}
}

func progressKey(taskID string) string {
	return "migrate:progress:" + taskID
}

// Update sets one or more fields on the progress hash for the given task.
func (s *ProgressStore) Update(ctx context.Context, taskID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	if err := s.rdb.HSet(ctx, progressKey(taskID), fields).Err(); err != nil {
		return fmt.Errorf("progress update %s: %w", taskID, err)
	}
	return nil
}

// Get retrieves the progress for a single task. Returns a zero-value
// TaskProgress (with the requested TaskID) if the key does not exist.
func (s *ProgressStore) Get(ctx context.Context, taskID string) (TaskProgress, error) {
	vals, err := s.rdb.HGetAll(ctx, progressKey(taskID)).Result()
	if err != nil {
		return TaskProgress{}, fmt.Errorf("progress get %s: %w", taskID, err)
	}
	p := TaskProgress{TaskID: taskID}
	if len(vals) == 0 {
		return p, nil
	}
	p.ProjectID = vals["project_id"]
	p.State = vals["state"]
	p.SeriesRead, _ = strconv.Atoi(vals["series_read"])
	p.SamplesRead, _ = strconv.Atoi(vals["samples_read"])
	p.SamplesSent, _ = strconv.Atoi(vals["samples_sent"])
	p.BatchesSent, _ = strconv.Atoi(vals["batches_sent"])
	p.Error = vals["error"]
	if t, err := time.Parse(time.RFC3339Nano, vals["started_at"]); err == nil {
		p.StartedAt = t
	}
	return p, nil
}

// Delete removes progress entries for the given task IDs. Returns the number
// of keys actually deleted. Idempotent — deleting a missing key is not an error.
func (s *ProgressStore) Delete(ctx context.Context, taskIDs []string) (int, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	count := 0
	for _, id := range taskIDs {
		n, err := s.rdb.Del(ctx, progressKey(id)).Result()
		if err != nil {
			return count, fmt.Errorf("progress delete %s: %w", id, err)
		}
		count += int(n)
	}
	return count, nil
}

// DeleteAll removes all progress entries by scanning migrate:progress:* keys
// and deleting each batch. Returns the number of keys deleted.
func (s *ProgressStore) DeleteAll(ctx context.Context) (int, error) {
	prefix := "migrate:progress:"
	count := 0
	var cursor uint64
	for {
		keys, nextCursor, err := s.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return count, fmt.Errorf("progress delete all scan: %w", err)
		}
		if len(keys) > 0 {
			n, err := s.rdb.Del(ctx, keys...).Result()
			if err != nil {
				return count, fmt.Errorf("progress delete all del: %w", err)
			}
			count += int(n)
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return count, nil
}

// ReconcileStaleActive marks any progress entries currently in the "active"
// state as failed. It is meant to be called exactly once at server startup,
// before the asynq worker begins processing tasks: any hash still showing
// "active" at that moment belongs to a task whose worker was killed without
// running its terminal-state handler (e.g. OOMKilled, pod eviction, SIGKILL).
// If asynq subsequently re-enqueues one of those tasks, the handler's own
// progress updates will overwrite the failed state back to active on the
// first poll, so the reconciliation is transparent for retries.
func (s *ProgressStore) ReconcileStaleActive(ctx context.Context) (int, error) {
	tasks, err := s.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("reconcile list: %w", err)
	}
	reconciled := 0
	for _, t := range tasks {
		if t.State != "active" {
			continue
		}
		if err := s.Update(ctx, t.TaskID, map[string]interface{}{
			"state": "failed",
			"error": "worker restarted mid-task (likely OOMKilled or pod eviction); asynq will retry if retries remain",
		}); err != nil {
			return reconciled, fmt.Errorf("reconcile update %s: %w", t.TaskID, err)
		}
		reconciled++
	}
	return reconciled, nil
}

// List returns progress for all tracked migration tasks by scanning
// for keys matching the progress prefix.
func (s *ProgressStore) List(ctx context.Context) ([]TaskProgress, error) {
	var results []TaskProgress
	prefix := "migrate:progress:"

	var cursor uint64
	for {
		keys, nextCursor, err := s.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("progress list scan: %w", err)
		}
		for _, key := range keys {
			taskID := key[len(prefix):]
			p, err := s.Get(ctx, taskID)
			if err != nil {
				return nil, err
			}
			results = append(results, p)
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return results, nil
}
