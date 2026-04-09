package cortexcheck_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"

	"github.com/NIPA-Mimir/services/migrator/internal/cortexcheck"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestProbeMissingPushSuffix(t *testing.T) {
	err := cortexcheck.Probe(context.Background(), "http://example.com:8080", discardLogger())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	const want = "CORTEX_URL must end with /push (cortex-tenant v2 requirement)"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

func TestProbeReturns404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	err := cortexcheck.Probe(context.Background(), srv.URL+"/push", discardLogger())
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "probe returned 404") {
		t.Errorf("error %q does not contain 'probe returned 404'", err.Error())
	}
}

func TestProbeReturns200OK(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := cortexcheck.Probe(context.Background(), srv.URL+"/push", discardLogger())
	if err != nil {
		t.Fatalf("expected nil error for 200, got %v", err)
	}

	// Verify headers.
	if ct := captured.Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("Content-Type: got %q, want 'application/x-protobuf'", ct)
	}
	if ce := captured.Header.Get("Content-Encoding"); ce != "snappy" {
		t.Errorf("Content-Encoding: got %q, want 'snappy'", ce)
	}
	if org := captured.Header.Get("X-Scope-OrgID"); org != "__probe__" {
		t.Errorf("X-Scope-OrgID: got %q, want '__probe__'", org)
	}

	// Verify body decodes to an empty WriteRequest.
	decoded, err := snappy.Decode(nil, capturedBody)
	if err != nil {
		t.Fatalf("snappy decode failed: %v", err)
	}
	var wr prompb.WriteRequest
	if err := proto.Unmarshal(decoded, &wr); err != nil {
		t.Fatalf("proto unmarshal failed: %v", err)
	}
	if len(wr.Timeseries) != 0 {
		t.Errorf("expected empty Timeseries, got %d entries", len(wr.Timeseries))
	}
}

func TestProbeReturns400Accepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	// 400 means path exists — probe should pass (nil error).
	err := cortexcheck.Probe(context.Background(), srv.URL+"/push", discardLogger())
	if err != nil {
		t.Fatalf("expected nil error for 400 (path-exists semantics), got %v", err)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	// Port 1 is privileged and will immediately refuse.
	err := cortexcheck.Probe(context.Background(), "http://127.0.0.1:1/push", discardLogger())
	if err == nil {
		t.Fatal("expected error for connection refused, got nil")
	}
	if !strings.Contains(err.Error(), "probe failed") {
		t.Errorf("error %q does not contain 'probe failed'", err.Error())
	}
}
