// fakes.go — exported test seam. Intentionally named fakes.go (not
// fakes_test.go) so tests in internal/migration and internal/queue can
// import these types across package boundaries.
package tsdb

import (
	"context"
	"errors"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/annotations"
)

// FakeProvider is a QuerierProvider that returns a fixed set of series
// regardless of the [mint, maxt] window. Tests use it to avoid real TSDB
// fixtures. If QuerierErr is non-nil, Querier() returns it immediately.
type FakeProvider struct {
	Series     []SeriesData
	QuerierErr error
}

// Querier satisfies QuerierProvider. Returns QuerierErr when set; otherwise
// a FakeQuerier pre-loaded with f.Series.
func (f *FakeProvider) Querier(mint, maxt int64) (storage.Querier, error) {
	if f.QuerierErr != nil {
		return nil, f.QuerierErr
	}
	return &FakeQuerier{series: f.Series}, nil
}

// FakeQuerier returns the provider's SeriesData as a storage.SeriesSet.
// Matchers are ignored — tests wanting tenant-filter semantics should filter
// their own SeriesData slice before constructing FakeProvider.
type FakeQuerier struct {
	series []SeriesData
}

// Select ignores all matchers and returns every injected series in insertion
// order.
func (q *FakeQuerier) Select(ctx context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	return &FakeSeriesSet{series: q.series, idx: -1}
}

// LabelValues is unimplemented — the fake exists only to feed Reader.Read.
func (q *FakeQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, errors.New("FakeQuerier.LabelValues not implemented")
}

// LabelNames is unimplemented — the fake exists only to feed Reader.Read.
func (q *FakeQuerier) LabelNames(ctx context.Context, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, errors.New("FakeQuerier.LabelNames not implemented")
}

// Close releases resources held by the fake querier (none).
func (q *FakeQuerier) Close() error { return nil }

// FakeSeriesSet walks the fake series in order.
type FakeSeriesSet struct {
	series []SeriesData
	idx    int
}

// Next advances the cursor by one and reports whether another series is
// available.
func (s *FakeSeriesSet) Next() bool {
	s.idx++
	return s.idx < len(s.series)
}

// At returns the current series — behaviour is undefined before the first
// Next() call or after Next() has returned false.
func (s *FakeSeriesSet) At() storage.Series { return &fakeSeries{data: s.series[s.idx]} }

// Err reports iteration errors. The fake never errors.
func (s *FakeSeriesSet) Err() error { return nil }

// Warnings reports any non-fatal annotations. The fake never produces any.
func (s *FakeSeriesSet) Warnings() annotations.Annotations { return nil }

type fakeSeries struct {
	data SeriesData
}

func (fs *fakeSeries) Labels() labels.Labels {
	builder := labels.NewScratchBuilder(len(fs.data.Labels))
	for _, l := range fs.data.Labels {
		builder.Add(l.Name, l.Value)
	}
	builder.Sort()
	return builder.Labels()
}

func (fs *fakeSeries) Iterator(_ chunkenc.Iterator) chunkenc.Iterator {
	return &fakeIterator{samples: fs.data.Samples, idx: -1}
}

// fakeIterator embeds chunkenc.Iterator to satisfy the full interface surface
// (Seek, AtHistogram, AtFloatHistogram, AtT, AtST) via the embedded — and
// intentionally nil — interface value. Reader.Read only ever calls Next, At,
// and Err on the iterator, so the nil embed is never dereferenced. Defining
// Seek directly on this type would trigger go vet's stdmethods false positive
// (it flags any Seek(int64) T as conflicting with io.Seeker.Seek); upstream
// Prometheus types with the same chunkenc.Iterator.Seek signature rely on a
// golangci-lint exclusion for the same reason. Embedding bypasses the vet
// heuristic because this type no longer literally defines Seek.
type fakeIterator struct {
	chunkenc.Iterator // intentionally nil — tests must not call Seek/AtHistogram/AtFloatHistogram/AtT/AtST
	samples           []prompb.Sample
	idx               int
}

func (it *fakeIterator) Next() chunkenc.ValueType {
	it.idx++
	if it.idx >= len(it.samples) {
		return chunkenc.ValNone
	}
	return chunkenc.ValFloat
}

func (it *fakeIterator) At() (int64, float64) {
	s := it.samples[it.idx]
	return s.Timestamp, s.Value
}

func (it *fakeIterator) Err() error { return nil }
