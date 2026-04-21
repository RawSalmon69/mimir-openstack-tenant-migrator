// Package main implements a one-shot tenant-discovery scanner.
// Enumerates distinct projectId label values across all TSDB blocks under
// the given path and reports the top-N by total series count.
//
// Uses tsdb.OpenBlock per block (no exclusive TSDB lock) so it can run
// alongside the migrator's shared tsdb.Open. Matches the projectId matcher
// semantics of internal/tsdb/reader.go exactly.
//
// Build:  go build -o tenant-scan ./cmd/tenant-scan
// Usage:  tenant-scan /data/tsdb 8     # top-8 tenants by series count
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/prometheus/prometheus/model/labels"
	promtsdb "github.com/prometheus/prometheus/tsdb"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tenant-scan <tsdb-path> [top-n=8]")
		os.Exit(2)
	}
	tsdbPath := os.Args[1]
	topN := 8
	if len(os.Args) >= 3 {
		n, err := strconv.Atoi(os.Args[2])
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "invalid top-n %q: %v\n", os.Args[2], err)
			os.Exit(2)
		}
		topN = n
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	blockDirs, err := listBlockDirs(tsdbPath)
	if err != nil {
		logger.Error("list blocks", "err", err)
		os.Exit(1)
	}
	logger.Info("scanning", "path", tsdbPath, "blocks", len(blockDirs))

	// Aggregate: projectId → total series count across blocks
	counts := make(map[string]int64)
	for i, bdir := range blockDirs {
		n, err := countProjectSeries(bdir, counts, logger)
		if err != nil {
			logger.Warn("block skipped", "block", filepath.Base(bdir), "err", err)
			continue
		}
		logger.Info("block scanned", "i", i+1, "of", len(blockDirs), "block", filepath.Base(bdir), "projectIds_in_block", n)
	}

	// Rank projectIds by total series count desc
	type entry struct {
		ProjectID string `json:"projectId"`
		Series    int64  `json:"series_count"`
	}
	all := make([]entry, 0, len(counts))
	for k, v := range counts {
		all = append(all, entry{ProjectID: k, Series: v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Series > all[j].Series })

	if topN > len(all) {
		topN = len(all)
	}
	top := all[:topN]

	out := map[string]any{
		"tsdb_path":           tsdbPath,
		"total_project_ids":   len(all),
		"top_n":               topN,
		"top":                 top,
		"all_project_ids":     all,
		"blocks_scanned":      len(blockDirs),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		logger.Error("encode", "err", err)
		os.Exit(1)
	}
	logger.Info("done", "distinct_projectIds", len(all), "top_selected", topN)
}

// listBlockDirs returns subdirectories of root that contain a meta.json file
// (the standard Prometheus TSDB block layout).
func listBlockDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta := filepath.Join(root, e.Name(), "meta.json")
		if _, err := os.Stat(meta); err == nil {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// countProjectSeries opens a single block (no exclusive DB lock) and
// aggregates series counts per projectId into counts. Returns the number
// of distinct projectIds found in this block.
func countProjectSeries(blockDir string, counts map[string]int64, logger *slog.Logger) (int, error) {
	blk, err := promtsdb.OpenBlock(nil, blockDir, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("OpenBlock: %w", err)
	}
	defer blk.Close()

	ixr, err := blk.Index()
	if err != nil {
		return 0, fmt.Errorf("Index: %w", err)
	}
	defer ixr.Close()

	ctx := context.Background()
	vals, err := ixr.LabelValues(ctx, "projectId", nil)
	if err != nil {
		return 0, fmt.Errorf("LabelValues projectId: %w", err)
	}

	local := 0
	for _, v := range vals {
		m := labels.MustNewMatcher(labels.MatchEqual, "projectId", v)
		p, err := promtsdb.PostingsForMatchers(ctx, ixr, m)
		if err != nil {
			logger.Warn("PostingsForMatchers", "projectId", v, "err", err)
			continue
		}
		// Count postings (series) for this projectId in this block
		n := int64(0)
		for p.Next() {
			n++
		}
		if err := p.Err(); err != nil {
			logger.Warn("postings.Err", "projectId", v, "err", err)
			continue
		}
		// Clone v before using as map key — Prometheus TSDB uses yoloString
		// so v's backing memory becomes invalid once the block's mmap closes.
		counts[string([]byte(v))] += n
		local++
	}
	return local, nil
}
