package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/NIPA-Mimir/services/migrator/internal/queue"
	"github.com/hibiken/asynq"
)

type handlers struct {
	cfg ServerConfig
}

type migrateRequest struct {
	ProjectIDs []string `json:"project_ids"`
}

type taskRef struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
}

type migrateResponse struct {
	Tasks []taskRef `json:"tasks"`
}

type statusItem struct {
	queue.TaskProgress
	ETASeconds *float64 `json:"eta_seconds,omitempty"`
}

type statusResponse struct {
	Tasks     []statusItem `json:"tasks"`
	Total     int          `json:"total"`
	Completed int          `json:"completed"`
	Active    int          `json:"active"`
	Pending   int          `json:"pending"`
	Failed    int          `json:"failed"`
}

func (h *handlers) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req migrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if len(req.ProjectIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project_ids must be a non-empty array"})
		return
	}

	// Overrides must be applied before enqueue so tenants migrate under the
	// relaxed rate limits. Fail-fast: a failed apply leaves the queue empty.
	if err := h.cfg.Applier.ApplyOverrides(r.Context(), req.ProjectIDs); err != nil {
		h.cfg.Logger.Error("failed to apply mimir overrides", "tenants", req.ProjectIDs, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to apply mimir overrides: " + err.Error(),
		})
		return
	}
	h.cfg.Logger.Info("applied mimir overrides", "tenants", req.ProjectIDs, "ooo_window", "2880h")

	var resp migrateResponse
	for _, pid := range req.ProjectIDs {
		payload := queue.MigratePayload{
			ProjectID:         pid,
			TSDBPath:          h.cfg.TSDBPath,
			CortexURL:         h.cfg.CortexURL,
			MaxBatchBytes:     h.cfg.MaxBatchBytes,
			WriterConcurrency: h.cfg.WriterConcurrency,
			DryRun:            h.cfg.DryRun,
		}
		task, err := queue.NewMigrateTask(payload,
			asynq.MaxRetry(3),
			asynq.Timeout(6*time.Hour),
			asynq.Queue("migration"),
		)
		if err != nil {
			h.cfg.Logger.Error("failed to create task", "project_id", pid, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create task for " + pid})
			return
		}

		info, err := h.cfg.Client.Enqueue(task)
		if err != nil {
			h.cfg.Logger.Error("failed to enqueue task", "project_id", pid, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to enqueue task for " + pid})
			return
		}

		_ = h.cfg.ProgressStore.Update(r.Context(), info.ID, map[string]interface{}{
			"project_id": pid,
			"state":      "pending",
		})

		resp.Tasks = append(resp.Tasks, taskRef{ID: info.ID, ProjectID: pid})
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.cfg.ProgressStore.List(r.Context())
	if err != nil {
		h.cfg.Logger.Error("failed to list progress", slog.String("err", err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list progress"})
		return
	}

	resp := statusResponse{}
	now := time.Now()
	for _, t := range tasks {
		item := statusItem{TaskProgress: t}
		if t.State == "active" && t.SamplesRead > 100 && !t.StartedAt.IsZero() {
			elapsed := now.Sub(t.StartedAt).Seconds()
			if elapsed > 0 {
				rate := float64(t.SamplesRead) / elapsed
				remaining := float64(t.SamplesRead) * 0.1
				eta := math.Max(0, remaining/rate)
				item.ETASeconds = &eta
			}
		}

		resp.Tasks = append(resp.Tasks, item)

		switch t.State {
		case "completed":
			resp.Completed++
		case "active":
			resp.Active++
		case "pending":
			resp.Pending++
		case "failed":
			resp.Failed++
		}
	}
	resp.Total = len(tasks)

	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id is required"})
		return
	}

	p, err := h.cfg.ProgressStore.Get(r.Context(), taskID)
	if err != nil {
		h.cfg.Logger.Error("failed to get progress", "task_id", taskID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get progress"})
		return
	}

	if p.State == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	writeJSON(w, http.StatusOK, p)
}

// deleteTasksResponse is returned by DELETE /tasks.
type deleteTasksResponse struct {
	DeletedProgress int    `json:"deleted_progress"`
	DeletedTasks    int    `json:"deleted_tasks"`
	ProjectID       string `json:"project_id"`
	All             bool   `json:"all"`
}

func (h *handlers) handleDeleteTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	allParam := r.URL.Query().Get("all")
	allTrue := allParam == "true"

	if projectID == "" && !allTrue {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "either project_id or all=true is required"})
		return
	}
	if projectID != "" && allTrue {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project_id and all=true are mutually exclusive"})
		return
	}

	ctx := r.Context()

	type listFn func(string, ...asynq.ListOption) ([]*asynq.TaskInfo, error)
	listFuncs := []struct {
		name string
		fn   listFn
		state string
	}{
		{"pending", h.cfg.Inspector.ListPendingTasks, "pending"},
		{"active", h.cfg.Inspector.ListActiveTasks, "active"},
		{"scheduled", h.cfg.Inspector.ListScheduledTasks, "scheduled"},
		{"retry", h.cfg.Inspector.ListRetryTasks, "retry"},
		{"archived", h.cfg.Inspector.ListArchivedTasks, "archived"},
		{"completed", h.cfg.Inspector.ListCompletedTasks, "completed"},
	}

	var targetTaskIDs []string
	taskState := map[string]string{}

	for _, lf := range listFuncs {
		infos, err := lf.fn("migration")
		if err != nil {
			h.cfg.Logger.Error("failed to list tasks", "state", lf.name, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list " + lf.name + " tasks: " + err.Error()})
			return
		}
		for _, info := range infos {
			if !allTrue {
				var payload queue.MigratePayload
				if jsonErr := json.Unmarshal(info.Payload, &payload); jsonErr != nil {
					continue
				}
				if payload.ProjectID != projectID {
					continue
				}
			}
			targetTaskIDs = append(targetTaskIDs, info.ID)
			taskState[info.ID] = lf.state
		}
	}

	for _, id := range targetTaskIDs {
		if taskState[id] == "active" {
			if err := h.cfg.Inspector.CancelProcessing(id); err != nil {
				h.cfg.Logger.Warn("CancelProcessing failed", "task_id", id, "err", err)
			}
		}
	}

	// Swallow ErrTaskNotFound so DELETE is idempotent.
	deletedTasks := 0
	for _, id := range targetTaskIDs {
		if err := h.cfg.Inspector.DeleteTask("migration", id); err != nil {
			if errors.Is(err, asynq.ErrTaskNotFound) {
				deletedTasks++
				continue
			}
			h.cfg.Logger.Warn("DeleteTask failed", "task_id", id, "err", err)
			continue
		}
		deletedTasks++
	}

	var (
		deletedProgress int
		err             error
	)
	if allTrue {
		deletedProgress, err = h.cfg.ProgressStore.DeleteAll(ctx)
	} else {
		deletedProgress, err = h.cfg.ProgressStore.Delete(ctx, targetTaskIDs)
	}
	if err != nil {
		h.cfg.Logger.Error("progress delete failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "progress delete failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, deleteTasksResponse{
		DeletedProgress: deletedProgress,
		DeletedTasks:    deletedTasks,
		ProjectID:       projectID,
		All:             allTrue,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
