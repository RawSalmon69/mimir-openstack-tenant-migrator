// Package queue integrates the migration pipeline with asynq job processing.
package queue

import (
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

// TypeMigrateTenant is the asynq task type for tenant migration jobs.
const TypeMigrateTenant = "migrate:tenant"

// MigratePayload carries the configuration for a single tenant migration.
type MigratePayload struct {
	ProjectID         string `json:"project_id"`
	TSDBPath          string `json:"tsdb_path"`
	CortexURL         string `json:"cortex_url"`
	MaxBatchBytes     int    `json:"max_batch_bytes"`
	WriterConcurrency int    `json:"writer_concurrency"`
	DryRun            bool   `json:"dry_run"`
}

// NewMigrateTask creates an asynq task for migrating a tenant's data.
func NewMigrateTask(payload MigratePayload, opts ...asynq.Option) (*asynq.Task, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal migrate payload: %w", err)
	}
	return asynq.NewTask(TypeMigrateTenant, data, opts...), nil
}

// ParseMigratePayload deserializes a MigratePayload from an asynq task.
func ParseMigratePayload(task *asynq.Task) (MigratePayload, error) {
	var p MigratePayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return p, fmt.Errorf("unmarshal migrate payload: %w", err)
	}
	if p.ProjectID == "" {
		return p, fmt.Errorf("project_id is required")
	}
	return p, nil
}
