package queue

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistoryLoggerAppend(t *testing.T) {
	var buf bytes.Buffer
	h := NewHistoryLogger(&buf)

	now := time.Now().UTC().Truncate(time.Second)
	e1 := HistoryEntry{
		TaskID:      "task-1",
		ProjectID:   "proj-a",
		State:       "completed",
		SeriesRead:  3,
		SamplesRead: 30,
		SamplesSent: 30,
		BatchesSent: 2,
		StartedAt:   now,
		EndedAt:     now.Add(5 * time.Second),
	}
	e2 := HistoryEntry{
		TaskID:    "task-2",
		ProjectID: "proj-b",
		State:     "failed",
		StartedAt: now,
		EndedAt:   now.Add(time.Second),
		Error:     "remote write: 500",
	}

	if err := h.Append(e1); err != nil {
		t.Fatalf("Append e1: %v", err)
	}
	if err := h.Append(e2); err != nil {
		t.Fatalf("Append e2: %v", err)
	}

	raw := buf.String()
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), raw)
	}

	var out1, out2 HistoryEntry
	if err := json.Unmarshal([]byte(lines[0]), &out1); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &out2); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}

	if out1.TaskID != e1.TaskID || out1.State != "completed" || out1.SamplesSent != 30 {
		t.Errorf("e1 round-trip mismatch: %+v", out1)
	}
	if out2.TaskID != e2.TaskID || out2.State != "failed" || out2.Error == "" {
		t.Errorf("e2 round-trip mismatch: %+v", out2)
	}
}

func TestHistoryLoggerNilSafe(t *testing.T) {
	var h *HistoryLogger
	// Must not panic and must return nil error.
	if err := h.Append(HistoryEntry{}); err != nil {
		t.Fatalf("nil receiver Append returned error: %v", err)
	}
}

func TestHistoryLoggerConcurrent(t *testing.T) {
	var buf bytes.Buffer
	h := NewHistoryLogger(&buf)

	const goroutines = 10
	const perGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				entry := HistoryEntry{
					TaskID:    "task",
					ProjectID: "proj",
					State:     "completed",
					StartedAt: time.Now().UTC(),
					EndedAt:   time.Now().UTC(),
				}
				if err := h.Append(entry); err != nil {
					t.Errorf("goroutine %d append %d: %v", idx, j, err)
				}
			}
		}(i)
	}
	wg.Wait()

	raw := buf.String()
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("expected %d lines, got %d", goroutines*perGoroutine, len(lines))
	}
	for i, line := range lines {
		var entry HistoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("line %d is not valid JSON (%q): %v", i, line, err)
		}
	}
}
