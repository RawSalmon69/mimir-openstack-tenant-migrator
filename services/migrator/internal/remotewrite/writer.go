// Package remotewrite provides a Prometheus remote write client that builds
// prompb.WriteRequest payloads, marshals to protobuf, compresses with snappy,
// and POSTs to a configurable endpoint (e.g. cortex-tenant).
package remotewrite

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	proto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// WriterConfig controls the remote write client's behavior.
type WriterConfig struct {
	// URL is the remote write endpoint (e.g. "http://cortex-tenant:8080/push").
	URL string
	// Timeout is the per-request timeout. Zero means 30s default.
	Timeout time.Duration
}

// Writer sends protobuf+snappy-encoded WriteRequests to a remote write endpoint.
type Writer struct {
	cfg    WriterConfig
	client *http.Client
	logger *slog.Logger
}

// NewWriter creates a Writer with connection-pooled HTTP transport.
func NewWriter(cfg WriterConfig, logger *slog.Logger) *Writer {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	transport := &http.Transport{
		// 32 idle conns per host accommodates QC × writer fan-out with
		// headroom. MaxConnsPerHost is left unset due to a known panic in
		// Go < 1.25.2; revisit when the toolchain is upgraded.
		MaxIdleConnsPerHost: 32,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	return &Writer{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
		logger: logger,
	}
}

// BuildWriteRequest marshals the given TimeSeries into a snappy-compressed
// protobuf WriteRequest payload. Exported for testing.
func BuildWriteRequest(series []prompb.TimeSeries) ([]byte, error) {
	req := &prompb.WriteRequest{
		Timeseries: series,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal WriteRequest: %w", err)
	}

	compressed := snappy.Encode(nil, data)
	return compressed, nil
}

// Send builds a WriteRequest from the given series and POSTs it to the
// configured endpoint. Returns nil on 2xx, or an error with status code
// and truncated response body on failure.
func (w *Writer) Send(ctx context.Context, series []prompb.TimeSeries) error {
	compressed, err := BuildWriteRequest(series)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("remote write POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		// Drain for connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("remote write failed: HTTP %d: %s", resp.StatusCode, string(body))
}
