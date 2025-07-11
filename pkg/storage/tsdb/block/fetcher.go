// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/thanos-io/thanos/blob/main/pkg/block/fetcher.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Thanos Authors.

package block

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/golang/groupcache/singleflight"
	"github.com/grafana/dskit/multierror"
	"github.com/grafana/dskit/runutil"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/oklog/ulid/v2"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/thanos-io/objstore"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/mimir/pkg/util/extprom"
)

// FetcherMetrics holds metrics tracked by the metadata fetcher. This struct and its fields are exported
// to allow depending projects (eg. Cortex) to implement their own custom metadata fetcher while tracking
// compatible metrics.
type FetcherMetrics struct {
	Syncs        prometheus.Counter
	SyncFailures prometheus.Counter
	SyncDuration prometheus.Histogram

	Synced *extprom.TxGaugeVec
}

// Submit applies new values for metrics tracked by transaction GaugeVec.
func (s *FetcherMetrics) Submit() {
	s.Synced.Submit()
}

// ResetTx starts new transaction for metrics tracked by transaction GaugeVec.
func (s *FetcherMetrics) ResetTx() {
	s.Synced.ResetTx()
}

const (
	CorruptedMeta = "corrupted-meta-json"
	NoMeta        = "no-meta-json"
	LoadedMeta    = "loaded"
	FailedMeta    = "failed"

	// Synced label values.
	labelExcludedMeta = "label-excluded"
	timeExcludedMeta  = "time-excluded"

	// DuplicateMeta is the label for blocks that are contained in other compacted blocks.
	DuplicateMeta = "duplicate"

	// Blocks that are marked for deletion can be loaded as well. This is done to make sure that we load blocks that are meant to be deleted,
	// but don't have a replacement block yet.
	MarkedForDeletionMeta = "marked-for-deletion"

	// MarkedForNoCompactionMeta is label for blocks which are loaded but also marked for no compaction. This label is also counted in `loaded` label metric.
	MarkedForNoCompactionMeta = "marked-for-no-compact"

	// LookbackExcludedMeta is label for blocks which are not loaded because their ULID pre-dates the fetcher's configured lookback period
	LookbackExcludedMeta = "lookback-excluded"
)

func NewFetcherMetrics(reg prometheus.Registerer, syncedExtraLabels [][]string) *FetcherMetrics {
	var m FetcherMetrics

	m.Syncs = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "blocks_meta_syncs_total",
		Help: "Total blocks metadata synchronization attempts",
	})
	m.SyncFailures = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "blocks_meta_sync_failures_total",
		Help: "Total blocks metadata synchronization failures",
	})
	m.SyncDuration = promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name: "blocks_meta_sync_duration_seconds",
		Help: "Duration of the blocks metadata synchronization in seconds",
		// We've seen the syncing taking even hours in extreme cases. We configure the buckets to
		// make sure we can track such high latency.
		Buckets: []float64{0.01, 1, 10, 100, 300, 600, 1200, 2400, 3600, 7200, 14400, 21600},
	})
	m.Synced = extprom.NewTxGaugeVec(
		reg,
		prometheus.GaugeOpts{
			Name: "blocks_meta_synced",
			Help: "Number of block metadata synced",
		},
		[]string{"state"},
		append([][]string{
			{CorruptedMeta},
			{NoMeta},
			{LoadedMeta},
			{FailedMeta},
			{labelExcludedMeta},
			{timeExcludedMeta},
			{DuplicateMeta},
			{MarkedForDeletionMeta},
			{MarkedForNoCompactionMeta},
			{LookbackExcludedMeta},
		}, syncedExtraLabels...)...,
	)
	return &m
}

type MetadataFetcher interface {
	Fetch(ctx context.Context) (metas map[ulid.ULID]*Meta, partial map[ulid.ULID]error, err error)
}

// GaugeVec hides something like a Prometheus GaugeVec or an extprom.TxGaugeVec.
type GaugeVec interface {
	WithLabelValues(lvs ...string) prometheus.Gauge
}

// MetadataFilter allows filtering or modifying metas from the provided map or returns error.
type MetadataFilter interface {
	Filter(ctx context.Context, metas map[ulid.ULID]*Meta, synced GaugeVec) error
}

// MetaFetcher is a struct that synchronizes filtered metadata of all block in the object storage with the local state.
// Go-routine safe.
type MetaFetcher struct {
	logger      log.Logger
	concurrency int
	bkt         objstore.InstrumentedBucketReader
	metrics     *FetcherMetrics
	filters     []MetadataFilter
	maxLookback time.Duration

	// Optional local directory to cache meta.json files.
	cacheDir string
	g        singleflight.Group

	mtx    sync.Mutex
	cached map[ulid.ULID]*Meta

	// Cache reused between MetaFetchers.
	metaCache *MetaCache
}

// NewMetaFetcher returns a MetaFetcher.
func NewMetaFetcher(logger log.Logger, concurrency int, bkt objstore.InstrumentedBucketReader, dir string, reg prometheus.Registerer, filters []MetadataFilter, metaCache *MetaCache, lookback time.Duration) (*MetaFetcher, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	cacheDir := ""
	if dir != "" {
		cacheDir = filepath.Join(dir, "meta-syncer")
		if err := os.MkdirAll(cacheDir, os.ModePerm); err != nil {
			return nil, err
		}
	}

	return &MetaFetcher{
		logger:      log.With(logger, "component", "block.MetaFetcher"),
		concurrency: concurrency,
		bkt:         bkt,
		cacheDir:    cacheDir,
		cached:      map[ulid.ULID]*Meta{},
		metrics:     NewFetcherMetrics(reg, nil),
		filters:     filters,
		metaCache:   metaCache,
		maxLookback: lookback,
	}, nil
}

var (
	ErrorSyncMetaNotFound  = errors.New("meta.json not found")
	ErrorSyncMetaCorrupted = errors.New("meta.json corrupted")
)

// loadMeta returns metadata from object storage or error.
// It returns ErrorSyncMetaNotFound and ErrorSyncMetaCorrupted sentinel errors in those cases.
func (f *MetaFetcher) loadMeta(ctx context.Context, id ulid.ULID) (*Meta, error) {
	var (
		metaFile       = path.Join(id.String(), MetaFilename)
		cachedBlockDir = filepath.Join(f.cacheDir, id.String())
	)

	// Block meta.json file is immutable, so we lookup the cache as first thing without issuing
	// any API call to the object storage. This significantly reduce the pressure on the object
	// storage.
	//
	// Details of all possible cases:
	//
	// - The block upload is in progress: the meta.json file is guaranteed to be uploaded at last.
	//   When we'll try to read it from object storage (later on), it will fail with ErrorSyncMetaNotFound
	//   which is correctly handled by the caller (partial block).
	//
	// - The block upload is completed: this is the normal case. meta.json file still exists in the
	//   object storage and it's expected to match the locally cached one (because it's immutable by design).
	//
	// - The block has been marked for deletion: the deletion hasn't started yet, so the full block (including
	//   the meta.json file) is still in the object storage. This case is not different than the previous one.
	//
	// - The block deletion is in progress: loadMeta() function may return the cached meta.json while it should
	//   return ErrorSyncMetaNotFound. This is a race condition that could happen even if we check the meta.json
	//   file in the storage, because the deletion could start right after we check it but before the MetaFetcher
	//   completes its sync.
	//
	// - The block has been deleted: the loadMeta() function will not be called at all, because the block
	//   was not discovered while iterating the bucket since all its files were already deleted.
	if m, seen := f.cached[id]; seen {
		return m, nil
	}

	if f.metaCache != nil {
		m := f.metaCache.Get(id)
		if m != nil {
			return m, nil
		}
	}

	// Best effort load from local dir.
	if f.cacheDir != "" {
		m, err := ReadMetaFromDir(cachedBlockDir)
		if err == nil {
			if f.metaCache != nil {
				f.metaCache.Put(m)
			}
			return m, nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			level.Warn(f.logger).Log("msg", "best effort read of the local meta.json failed; removing cached block dir", "dir", cachedBlockDir, "err", err)
			if err := os.RemoveAll(cachedBlockDir); err != nil {
				level.Warn(f.logger).Log("msg", "best effort remove of cached dir failed; ignoring", "dir", cachedBlockDir, "err", err)
			}
		}
	}

	r, err := f.bkt.ReaderWithExpectedErrs(f.bkt.IsObjNotFoundErr).Get(ctx, metaFile)
	if f.bkt.IsObjNotFoundErr(err) {
		// Meta.json was deleted between bkt.Exists and here.
		return nil, errors.Wrapf(ErrorSyncMetaNotFound, "%v", err)
	}
	if err != nil {
		return nil, errors.Wrapf(err, "get meta file: %v", metaFile)
	}

	defer runutil.CloseWithLogOnErr(f.logger, r, "close bkt meta get")

	metaContent, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrapf(err, "read meta file: %v", metaFile)
	}

	m := &Meta{}
	if err := json.Unmarshal(metaContent, m); err != nil {
		return nil, errors.Wrapf(ErrorSyncMetaCorrupted, "meta.json %v unmarshal: %v", metaFile, err)
	}

	if m.Version != TSDBVersion1 {
		return nil, errors.Errorf("unexpected meta file: %s version: %d", metaFile, m.Version)
	}

	// Best effort cache in local dir.
	if f.cacheDir != "" {
		if err := os.MkdirAll(cachedBlockDir, os.ModePerm); err != nil {
			level.Warn(f.logger).Log("msg", "best effort mkdir of the meta.json block dir failed; ignoring", "dir", cachedBlockDir, "err", err)
		}

		if err := m.WriteToDir(f.logger, cachedBlockDir); err != nil {
			level.Warn(f.logger).Log("msg", "best effort save of the meta.json to local dir failed; ignoring", "dir", cachedBlockDir, "err", err)
		}
	}

	if f.metaCache != nil {
		f.metaCache.Put(m)
	}
	return m, nil
}

type response struct {
	metas   map[ulid.ULID]*Meta
	partial map[ulid.ULID]error

	// If metaErr > 0 it means incomplete view, so some metas, failed to be loaded.
	metaErrs multierror.MultiError

	// Track the number of blocks not returned because of various reasons.
	noMetasCount           float64
	corruptedMetasCount    float64
	markedForDeletionCount float64
	exceededLookbackCount  float64
}

func (f *MetaFetcher) fetchMetadata(ctx context.Context, excludeMarkedForDeletion bool) (interface{}, error) {
	var (
		resp = response{
			metas:   make(map[ulid.ULID]*Meta),
			partial: make(map[ulid.ULID]error),
		}
		eg  errgroup.Group
		ch  = make(chan ulid.ULID, f.concurrency)
		mtx sync.Mutex
	)

	level.Debug(f.logger).Log("msg", "fetching meta data", "concurrency", f.concurrency, "max-lookback", f.maxLookback)

	// The first 6 bytes of a ULID are sortable as a function of time. When maxLookback is set, we construct a ULID that
	// represents the beginning of the lookback period, compare all discovered block ULIDs against this
	// ULID, and skip processing on blocks that have IDs less than
	var minAllowedBlockID ulid.ULID
	if f.maxLookback > 0 {
		var err error
		minAllowedBlockID, err = ulid.New(ulid.Timestamp(time.Now().Add(-f.maxLookback)), nil)
		if err != nil {
			return nil, err
		}
	}

	// Get the list of blocks marked for deletion so that we'll exclude them (if required).
	var markedForDeletion map[ulid.ULID]struct{}
	if excludeMarkedForDeletion {
		var err error

		markedForDeletion, err = ListBlockDeletionMarks(ctx, f.bkt)
		if err != nil {
			return nil, err
		}
	}

	// Run workers.
	for i := 0; i < f.concurrency; i++ {
		eg.Go(func() error {
			for id := range ch {
				meta, err := f.loadMeta(ctx, id)
				if err == nil {
					mtx.Lock()
					resp.metas[id] = meta
					mtx.Unlock()
					continue
				}

				if errors.Is(err, ErrorSyncMetaNotFound) {
					mtx.Lock()
					resp.noMetasCount++
					mtx.Unlock()
				} else if errors.Is(err, ErrorSyncMetaCorrupted) {
					mtx.Lock()
					resp.corruptedMetasCount++
					mtx.Unlock()
				} else {
					mtx.Lock()
					resp.metaErrs.Add(err)
					mtx.Unlock()
					continue
				}

				mtx.Lock()
				resp.partial[id] = err
				mtx.Unlock()
			}
			return nil
		})
	}

	// Workers scheduled, distribute blocks.
	eg.Go(func() error {
		defer close(ch)
		return f.bkt.Iter(ctx, "", func(name string) error {
			id, ok := IsBlockDir(name)
			if !ok {
				return nil
			}

			// If requested, skip any block marked for deletion.
			if _, marked := markedForDeletion[id]; excludeMarkedForDeletion && marked {
				resp.markedForDeletionCount++
				return nil
			}

			// skip any blocks older than the fetcher's max lookback.
			if f.maxLookback > 0 && id.Compare(minAllowedBlockID) == -1 {
				resp.exceededLookbackCount++
				return nil
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- id:
			}

			return nil
		})
	})

	if err := eg.Wait(); err != nil {
		return nil, errors.Wrap(err, "MetaFetcher: iter bucket")
	}

	if len(resp.metaErrs) > 0 {
		return resp, nil
	}

	// Only for complete view of blocks update the cache.
	cached := make(map[ulid.ULID]*Meta, len(resp.metas))
	for id, m := range resp.metas {
		cached[id] = m
	}

	f.mtx.Lock()
	f.cached = cached
	f.mtx.Unlock()

	// Best effort cleanup of disk-cached metas.
	if f.cacheDir != "" {
		fis, err := os.ReadDir(f.cacheDir)
		names := make([]string, 0, len(fis))
		for _, fi := range fis {
			names = append(names, fi.Name())
		}
		if err != nil {
			level.Warn(f.logger).Log("msg", "best effort remove of not needed cached dirs failed; ignoring", "err", err)
		} else {
			for _, n := range names {
				id, ok := IsBlockDir(n)
				if !ok {
					continue
				}

				if _, ok := resp.metas[id]; ok {
					continue
				}

				cachedBlockDir := filepath.Join(f.cacheDir, id.String())

				// No such block loaded, remove the local dir.
				if err := os.RemoveAll(cachedBlockDir); err != nil {
					level.Warn(f.logger).Log("msg", "best effort remove of not needed cached dir failed; ignoring", "dir", cachedBlockDir, "err", err)
				}
			}
		}
	}
	return resp, nil
}

// Fetch returns all block metas as well as partial blocks (blocks without or with corrupted meta file) from the bucket.
// It's caller responsibility to not change the returned metadata files. Maps can be modified.
//
// Returned error indicates a failure in fetching metadata. Returned meta can be assumed as correct, with some blocks missing.
func (f *MetaFetcher) Fetch(ctx context.Context) (metas map[ulid.ULID]*Meta, partials map[ulid.ULID]error, err error) {
	metas, partials, err = f.fetch(ctx, false)
	return
}

// FetchWithoutMarkedForDeletion returns all block metas as well as partial blocks (blocks without or with corrupted meta file) from the bucket.
// This function excludes all blocks marked for deletion (no deletion delay applied).
// It's caller responsibility to not change the returned metadata files. Maps can be modified.
//
// Returned error indicates a failure in fetching metadata. Returned meta can be assumed as correct, with some blocks missing.
func (f *MetaFetcher) FetchWithoutMarkedForDeletion(ctx context.Context) (metas map[ulid.ULID]*Meta, partials map[ulid.ULID]error, err error) {
	metas, partials, err = f.fetch(ctx, true)
	return
}

func (f *MetaFetcher) fetch(ctx context.Context, excludeMarkedForDeletion bool) (_ map[ulid.ULID]*Meta, _ map[ulid.ULID]error, err error) {
	start := time.Now()
	defer func() {
		f.metrics.SyncDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			f.metrics.SyncFailures.Inc()
		}
	}()
	f.metrics.Syncs.Inc()
	f.metrics.ResetTx()

	// Run this in thread safe run group.
	v, err := f.g.Do("", func() (i interface{}, err error) {
		// NOTE: First go routine context will go through.
		return f.fetchMetadata(ctx, excludeMarkedForDeletion)
	})
	if err != nil {
		return nil, nil, err
	}
	resp := v.(response)

	// Copy as same response might be reused by different goroutines.
	metas := make(map[ulid.ULID]*Meta, len(resp.metas))
	for id, m := range resp.metas {
		metas[id] = m
	}

	f.metrics.Synced.WithLabelValues(FailedMeta).Set(float64(len(resp.metaErrs)))
	f.metrics.Synced.WithLabelValues(NoMeta).Set(resp.noMetasCount)
	f.metrics.Synced.WithLabelValues(CorruptedMeta).Set(resp.corruptedMetasCount)
	f.metrics.Synced.WithLabelValues(LookbackExcludedMeta).Set(resp.exceededLookbackCount)
	if excludeMarkedForDeletion {
		f.metrics.Synced.WithLabelValues(MarkedForDeletionMeta).Set(resp.markedForDeletionCount)
	}

	for _, filter := range f.filters {
		// NOTE: filter can update synced metric accordingly to the reason of the exclude.
		if err := filter.Filter(ctx, metas, f.metrics.Synced); err != nil {
			return nil, nil, errors.Wrap(err, "filter metas")
		}
	}

	f.metrics.Synced.WithLabelValues(LoadedMeta).Set(float64(len(metas)))
	f.metrics.Submit()

	if len(resp.metaErrs) > 0 {
		return metas, resp.partial, errors.Wrap(resp.metaErrs.Err(), "incomplete view")
	}

	level.Info(f.logger).Log("msg", "successfully synchronized block metadata", "duration", time.Since(start).String(), "duration_ms", time.Since(start).Milliseconds(), "cached", f.countCached(), "returned", len(metas), "partial", len(resp.partial))
	return metas, resp.partial, nil
}

func (f *MetaFetcher) countCached() int {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return len(f.cached)
}

// BlockIDLabel is a special label that will have an ULID of the meta.json being referenced to.
const BlockIDLabel = "__block_id"

// IgnoreDeletionMarkFilter is a filter that filters out the blocks that are marked for deletion after a given delay.
// The delay duration is to make sure that the replacement block can be fetched before we filter out the old block.
// Delay is not considered when computing DeletionMarkBlocks map.
// Not go-routine safe.
type IgnoreDeletionMarkFilter struct {
	logger      log.Logger
	delay       time.Duration
	concurrency int
	bkt         objstore.InstrumentedBucketReader

	mtx             sync.Mutex
	deletionMarkMap map[ulid.ULID]*DeletionMark
}

// NewIgnoreDeletionMarkFilter creates IgnoreDeletionMarkFilter.
func NewIgnoreDeletionMarkFilter(logger log.Logger, bkt objstore.InstrumentedBucketReader, delay time.Duration, concurrency int) *IgnoreDeletionMarkFilter {
	return &IgnoreDeletionMarkFilter{
		logger:      logger,
		bkt:         bkt,
		delay:       delay,
		concurrency: concurrency,
	}
}

// DeletionMarkBlocks returns block ids that were marked for deletion.
func (f *IgnoreDeletionMarkFilter) DeletionMarkBlocks() map[ulid.ULID]*DeletionMark {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	deletionMarkMap := make(map[ulid.ULID]*DeletionMark, len(f.deletionMarkMap))
	for id, meta := range f.deletionMarkMap {
		deletionMarkMap[id] = meta
	}

	return deletionMarkMap
}

// Filter filters out blocks that are marked for deletion after a given delay.
// It also returns the blocks that can be deleted since they were uploaded delay duration before current time.
func (f *IgnoreDeletionMarkFilter) Filter(ctx context.Context, metas map[ulid.ULID]*Meta, synced GaugeVec) error {
	deletionMarkMap := make(map[ulid.ULID]*DeletionMark)

	// Make a copy of block IDs to check, in order to avoid concurrency issues
	// between the scheduler and workers.
	blockIDs := make([]ulid.ULID, 0, len(metas))
	for id := range metas {
		blockIDs = append(blockIDs, id)
	}

	var (
		eg  errgroup.Group
		ch  = make(chan ulid.ULID, f.concurrency)
		mtx sync.Mutex
	)

	for i := 0; i < f.concurrency; i++ {
		eg.Go(func() error {
			var lastErr error
			for id := range ch {
				m := &DeletionMark{}
				if err := ReadMarker(ctx, f.logger, f.bkt, id.String(), m); err != nil {
					if errors.Is(err, ErrorMarkerNotFound) {
						continue
					}
					if errors.Is(err, ErrorUnmarshalMarker) {
						level.Warn(f.logger).Log("msg", "found partial deletion-mark.json; if we will see it happening often for the same block, consider manually deleting deletion-mark.json from the object storage", "block", id, "err", err)
						continue
					}
					// Remember the last error and continue to drain the channel.
					lastErr = err
					continue
				}

				// Keep track of the blocks marked for deletion and filter them out if their
				// deletion time is greater than the configured delay.
				mtx.Lock()
				deletionMarkMap[id] = m
				if time.Since(time.Unix(m.DeletionTime, 0)).Seconds() > f.delay.Seconds() {
					synced.WithLabelValues(MarkedForDeletionMeta).Inc()
					delete(metas, id)
				}
				mtx.Unlock()
			}

			return lastErr
		})
	}

	// Workers scheduled, distribute blocks.
	eg.Go(func() error {
		defer close(ch)

		for _, id := range blockIDs {
			select {
			case ch <- id:
				// Nothing to do.
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return errors.Wrap(err, "filter blocks marked for deletion")
	}

	f.mtx.Lock()
	f.deletionMarkMap = deletionMarkMap
	f.mtx.Unlock()

	return nil
}

// MetaCache is a LRU cache for parsed *Meta objects, optionally used by *MetaFetcher.
// While MetaFetcher.cache is per-instance, MetaCache can be reused between different *MetaFetcher instances.
type MetaCache struct {
	maxSize            int
	minCompactionLevel int
	minSources         int

	lru    *lru.Cache[ulid.ULID, *Meta]
	hits   atomic.Int64
	misses atomic.Int64
}

// NewMetaCache creates new *MetaCache with given max size, and parameters for storing *Meta objects.
// Only *Meta objects with specified minimum compaction level and number of sources are stored into the cache.
func NewMetaCache(maxSize, minCompactionLevel, minSources int) *MetaCache {
	l, err := lru.New[ulid.ULID, *Meta](maxSize)
	// This can only happen if size < 0.
	if err != nil {
		panic(err.Error())
	}

	return &MetaCache{
		maxSize:            maxSize,
		minCompactionLevel: minCompactionLevel,
		minSources:         minSources,
		lru:                l,
	}
}

func (mc *MetaCache) MaxSize() int {
	return mc.maxSize
}

func (mc *MetaCache) Put(meta *Meta) {
	if meta == nil {
		return
	}

	if mc.minCompactionLevel > 0 && meta.Compaction.Level < mc.minCompactionLevel {
		return
	}

	if mc.minSources > 0 && len(meta.Compaction.Sources) < mc.minSources {
		return
	}

	mc.lru.Add(meta.ULID, meta)
}

func (mc *MetaCache) Get(id ulid.ULID) *Meta {
	val, ok := mc.lru.Get(id)
	if !ok {
		mc.misses.Add(1)
		return nil
	}
	mc.hits.Add(1)
	return val
}

func (mc *MetaCache) Stats() (items int, bytesSize int64, hits, misses int) {
	for _, m := range mc.lru.Values() {
		items++
		bytesSize += sizeOfUlid // for a key
		bytesSize += MetaBytesSize(m)
	}
	return items, bytesSize, int(mc.hits.Load()), int(mc.misses.Load())
}

var sizeOfUlid = int64(unsafe.Sizeof(ulid.ULID{}))
var sizeOfBlockDesc = int64(unsafe.Sizeof(tsdb.BlockDesc{}))

func MetaBytesSize(m *Meta) int64 {
	size := int64(0)
	size += int64(unsafe.Sizeof(*m))
	size += int64(len(m.Compaction.Sources)) * sizeOfUlid
	size += int64(len(m.Compaction.Parents)) * sizeOfBlockDesc

	for _, h := range m.Compaction.Hints {
		size += int64(unsafe.Sizeof(h))
	}
	return size
}
