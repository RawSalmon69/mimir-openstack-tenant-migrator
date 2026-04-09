// Package cortexcheck provides a boot-time validation probe for the CORTEX_URL
// endpoint. It verifies the URL ends with /push (cortex-tenant v2 requirement)
// and that the remote write path exists by sending a snappy-encoded empty
// WriteRequest and treating any non-404 response as a pass.
package cortexcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	proto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// Probe validates cfg.cortexURL at startup:
//  1. Suffix check: URL must end with "/push".
//  2. Live probe: POST a minimal snappy-encoded empty WriteRequest.
//     - 404 → fatal (path not found).
//     - Any other status (200, 400, 401, 403, 5xx) → pass (path exists).
//     - Network/dial error → fatal (can't reach cortex-tenant).
func Probe(ctx context.Context, url string, logger *slog.Logger) error {
	if !strings.HasSuffix(url, "/push") {
		return errors.New("CORTEX_URL must end with /push (cortex-tenant v2 requirement)")
	}

	wr := &prompb.WriteRequest{}
	data, err := proto.Marshal(wr)
	if err != nil {
		return fmt.Errorf("CORTEX_URL probe marshal failed: %w", err)
	}
	compressed := snappy.Encode(nil, data)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("CORTEX_URL probe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.Header.Set("X-Scope-OrgID", "__probe__")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("CORTEX_URL probe failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("CORTEX_URL probe returned 404 (path not found — check cortex-tenant deployment): %s", url)
	}

	logger.Info("cortex_url probe ok", "status", resp.StatusCode, "url", url)
	return nil
}
