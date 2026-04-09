package queue

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// HistoryEntry records one terminal migration task outcome.
type HistoryEntry struct {
	TaskID      string    `json:"task_id"`
	ProjectID   string    `json:"project_id"`
	State       string    `json:"state"`
	SeriesRead  int       `json:"series_read"`
	SamplesRead int       `json:"samples_read"`
	SamplesSent int       `json:"samples_sent"`
	BatchesSent int       `json:"batches_sent"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	Error       string    `json:"error,omitempty"`
}

// HistoryLogger writes HistoryEntry values as newline-delimited JSON to an
// underlying io.Writer. Concurrent callers are serialised by an internal
// mutex so bytes from different goroutines never interleave.
type HistoryLogger struct {
	w  io.Writer
	mu sync.Mutex
}

// NewHistoryLogger creates a HistoryLogger that writes to w.
func NewHistoryLogger(w io.Writer) *HistoryLogger {
	return &HistoryLogger{w: w}
}

// Append serialises entry as one JSON line (\n-terminated). Safe for
// concurrent callers. A nil receiver or nil writer is a no-op so the
// handler can call Append even when no log path is configured.
func (h *HistoryLogger) Append(entry HistoryEntry) error {
	if h == nil || h.w == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return json.NewEncoder(h.w).Encode(entry)
}
