package remotewrite

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	proto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// makeSeries creates n TimeSeries with varied labels and samplesPerSeries samples each.
func makeSeries(n, samplesPerSeries int) []prompb.TimeSeries {
	series := make([]prompb.TimeSeries, n)
	for i := range n {
		labels := []prompb.Label{
			{Name: "__name__", Value: "test_metric"},
			{Name: "instance", Value: "localhost:9090"},
			{Name: "projectId", Value: "tenant-abc"},
			{Name: "job", Value: "test"},
		}
		// Sort labels by name.
		sort.Slice(labels, func(a, b int) bool {
			return labels[a].Name < labels[b].Name
		})

		samples := make([]prompb.Sample, samplesPerSeries)
		for j := range samplesPerSeries {
			samples[j] = prompb.Sample{
				Timestamp: int64(1700000000000 + i*1000 + j*30000),
				Value:     float64(i*100 + j),
			}
		}
		series[i] = prompb.TimeSeries{Labels: labels, Samples: samples}
	}
	return series
}

func TestBuildWriteRequestRoundTrip(t *testing.T) {
	original := makeSeries(3, 5)

	compressed, err := BuildWriteRequest(original)
	if err != nil {
		t.Fatalf("BuildWriteRequest: %v", err)
	}

	// Decompress.
	decoded, err := snappy.Decode(nil, compressed)
	if err != nil {
		t.Fatalf("snappy.Decode: %v", err)
	}

	// Unmarshal.
	var req prompb.WriteRequest
	if err := proto.Unmarshal(decoded, &req); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}

	if len(req.Timeseries) != 3 {
		t.Fatalf("expected 3 timeseries, got %d", len(req.Timeseries))
	}

	for i, ts := range req.Timeseries {
		if len(ts.Samples) != 5 {
			t.Errorf("series %d: expected 5 samples, got %d", i, len(ts.Samples))
		}

		// Verify labels are sorted by name.
		for j := 1; j < len(ts.Labels); j++ {
			if ts.Labels[j-1].Name >= ts.Labels[j].Name {
				t.Errorf("series %d: labels not sorted: %s >= %s", i, ts.Labels[j-1].Name, ts.Labels[j].Name)
			}
		}

		// Verify sample values match.
		for j, s := range ts.Samples {
			expected := float64(i*100 + j)
			if s.Value != expected {
				t.Errorf("series %d sample %d: expected value %f, got %f", i, j, expected, s.Value)
			}
		}
	}
}

func TestSendToMockServer(t *testing.T) {
	var receivedHeaders http.Header
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := NewWriter(WriterConfig{URL: server.URL, Timeout: 5 * time.Second}, logger)

	series := makeSeries(2, 3)
	if err := writer.Send(context.Background(), series); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Check required headers.
	if ct := receivedHeaders.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want application/x-protobuf", ct)
	}
	if ce := receivedHeaders.Get("Content-Encoding"); ce != "snappy" {
		t.Errorf("Content-Encoding = %q, want snappy", ce)
	}
	if rwv := receivedHeaders.Get("X-Prometheus-Remote-Write-Version"); rwv != "0.1.0" {
		t.Errorf("X-Prometheus-Remote-Write-Version = %q, want 0.1.0", rwv)
	}

	// Decompress and verify the payload.
	decoded, err := snappy.Decode(nil, receivedBody)
	if err != nil {
		t.Fatalf("snappy.Decode: %v", err)
	}
	var req prompb.WriteRequest
	if err := proto.Unmarshal(decoded, &req); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	if len(req.Timeseries) != 2 {
		t.Errorf("expected 2 timeseries in payload, got %d", len(req.Timeseries))
	}
}

func TestSendHandlesServerError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"HTTP 500", http.StatusInternalServerError},
		{"HTTP 429", http.StatusTooManyRequests},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte("error body"))
			}))
			defer server.Close()

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			writer := NewWriter(WriterConfig{URL: server.URL, Timeout: 5 * time.Second}, logger)

			err := writer.Send(context.Background(), makeSeries(1, 1))
			if err == nil {
				t.Fatal("expected error for server error response, got nil")
			}
			// Error should contain the status code.
			if got := err.Error(); !containsStatusCode(got, tt.statusCode) {
				t.Errorf("error %q does not mention status %d", got, tt.statusCode)
			}
		})
	}
}

func containsStatusCode(msg string, code int) bool {
	return len(msg) > 0 && contains(msg, http.StatusText(code)) || contains(msg, itoa(code))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestSendRespectsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // block longer than context deadline
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := NewWriter(WriterConfig{URL: server.URL, Timeout: 10 * time.Second}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := writer.Send(ctx, makeSeries(1, 1))
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestSendConnectionRefused(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Point at a port that's not listening.
	writer := NewWriter(WriterConfig{URL: "http://127.0.0.1:1", Timeout: 2 * time.Second}, logger)

	err := writer.Send(context.Background(), makeSeries(1, 1))
	if err == nil {
		t.Fatal("expected error for connection refused, got nil")
	}
}

func TestSendEmptySeries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		decoded, err := snappy.Decode(nil, body)
		if err != nil {
			t.Errorf("snappy.Decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req prompb.WriteRequest
		if err := proto.Unmarshal(decoded, &req); err != nil {
			t.Errorf("proto.Unmarshal: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.Timeseries) != 0 {
			t.Errorf("expected 0 timeseries, got %d", len(req.Timeseries))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := NewWriter(WriterConfig{URL: server.URL, Timeout: 5 * time.Second}, logger)

	// Empty series should produce a valid empty WriteRequest.
	if err := writer.Send(context.Background(), nil); err != nil {
		t.Fatalf("Send with empty series: %v", err)
	}
}

func TestSendSingleSeriesZeroSamples(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		decoded, _ := snappy.Decode(nil, body)
		var req prompb.WriteRequest
		_ = proto.Unmarshal(decoded, &req)
		if len(req.Timeseries) != 1 {
			t.Errorf("expected 1 timeseries, got %d", len(req.Timeseries))
		}
		if len(req.Timeseries) > 0 && len(req.Timeseries[0].Samples) != 0 {
			t.Errorf("expected 0 samples, got %d", len(req.Timeseries[0].Samples))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := NewWriter(WriterConfig{URL: server.URL, Timeout: 5 * time.Second}, logger)

	series := []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "empty_metric"}},
		Samples: nil,
	}}
	if err := writer.Send(context.Background(), series); err != nil {
		t.Fatalf("Send with zero-sample series: %v", err)
	}
}
