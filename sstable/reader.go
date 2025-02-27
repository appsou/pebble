// Copyright 2011 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/bytealloc"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider/objiotracing"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/colblk"
	"github.com/cockroachdb/pebble/sstable/rowblk"
	"github.com/cockroachdb/pebble/sstable/valblk"
	"github.com/cockroachdb/pebble/vfs"
)

var errReaderClosed = errors.New("pebble/table: reader is closed")

type loadBlockResult int8

const (
	loadBlockOK loadBlockResult = iota
	// Could be due to error or because no block left to load.
	loadBlockFailed
	loadBlockIrrelevant
)

// Reader is a table reader.
type Reader struct {
	blockReader block.Reader

	// The following fields are copied from the ReadOptions.
	keySchema            *colblk.KeySchema
	filterMetricsTracker *FilterMetricsTracker
	Comparer             *base.Comparer

	tableFilter *tableFilterReader

	err error

	indexBH      block.Handle
	filterBH     block.Handle
	rangeDelBH   block.Handle
	rangeKeyBH   block.Handle
	valueBIH     valblk.IndexHandle
	propertiesBH block.Handle
	metaindexBH  block.Handle
	footerBH     block.Handle

	Properties  Properties
	tableFormat TableFormat
}

var _ CommonReader = (*Reader)(nil)

// Close the reader and the underlying objstorage.Readable.
func (r *Reader) Close() error {
	r.err = firstError(r.err, r.blockReader.Close())
	if r.err != nil {
		return r.err
	}
	// Make any future calls to Get, NewIter or Close return an error.
	r.err = errReaderClosed
	return nil
}

// NewPointIter returns an iterator for the point keys in the table.
//
// If transform.HideObsoletePoints is set, the callee assumes that filterer
// already includes obsoleteKeyBlockPropertyFilter. The caller can satisfy this
// contract by first calling TryAddBlockPropertyFilterForHideObsoletePoints.
func (r *Reader) NewPointIter(
	ctx context.Context,
	transforms IterTransforms,
	lower, upper []byte,
	filterer *BlockPropertiesFilterer,
	filterBlockSizeLimit FilterBlockSizeLimit,
	env block.ReadEnv,
	rp valblk.ReaderProvider,
) (Iterator, error) {
	return r.newPointIter(
		ctx, transforms, lower, upper, filterer, filterBlockSizeLimit,
		env, rp, nil)
}

// TryAddBlockPropertyFilterForHideObsoletePoints is expected to be called
// before the call to NewPointIter, to get the value of hideObsoletePoints and
// potentially add a block property filter.
func (r *Reader) TryAddBlockPropertyFilterForHideObsoletePoints(
	snapshotForHideObsoletePoints base.SeqNum,
	fileLargestSeqNum base.SeqNum,
	pointKeyFilters []BlockPropertyFilter,
) (hideObsoletePoints bool, filters []BlockPropertyFilter) {
	hideObsoletePoints = r.tableFormat >= TableFormatPebblev4 &&
		snapshotForHideObsoletePoints > fileLargestSeqNum
	if hideObsoletePoints {
		pointKeyFilters = append(pointKeyFilters, obsoleteKeyBlockPropertyFilter{})
	}
	return hideObsoletePoints, pointKeyFilters
}

func (r *Reader) newPointIter(
	ctx context.Context,
	transforms IterTransforms,
	lower, upper []byte,
	filterer *BlockPropertiesFilterer,
	filterBlockSizeLimit FilterBlockSizeLimit,
	env block.ReadEnv,
	rp valblk.ReaderProvider,
	vState *virtualState,
) (Iterator, error) {
	// NB: pebble.fileCache wraps the returned iterator with one which performs
	// reference counting on the Reader, preventing the Reader from being closed
	// until the final iterator closes.
	var res Iterator
	var err error
	if r.Properties.IndexType == twoLevelIndex {
		if r.tableFormat.BlockColumnar() {
			res, err = newColumnBlockTwoLevelIterator(
				ctx, r, vState, transforms, lower, upper, filterer, filterBlockSizeLimit,
				env, rp)
		} else {
			res, err = newRowBlockTwoLevelIterator(
				ctx, r, vState, transforms, lower, upper, filterer, filterBlockSizeLimit,
				env, rp)
		}
	} else {
		if r.tableFormat.BlockColumnar() {
			res, err = newColumnBlockSingleLevelIterator(
				ctx, r, vState, transforms, lower, upper, filterer, filterBlockSizeLimit,
				env, rp)
		} else {
			res, err = newRowBlockSingleLevelIterator(
				ctx, r, vState, transforms, lower, upper, filterer, filterBlockSizeLimit,
				env, rp)
		}
	}
	if err != nil {
		// Note: we don't want to return res here - it will be a nil
		// single/twoLevelIterator, not a nil Iterator.
		return nil, err
	}
	return res, nil
}

// NewIter returns an iterator for the point keys in the table. It is a
// simplified version of NewPointIter and should only be used for tests and
// tooling.
//
// NewIter must only be used when the Reader is guaranteed to outlive any
// LazyValues returned from the iter.
func (r *Reader) NewIter(transforms IterTransforms, lower, upper []byte) (Iterator, error) {
	// TODO(radu): we should probably not use bloom filters in this case, as there
	// likely isn't a cache set up.
	return r.NewPointIter(
		context.TODO(), transforms, lower, upper, nil, AlwaysUseFilterBlock,
		block.NoReadEnv, MakeTrivialReaderProvider(r))
}

// NewCompactionIter returns an iterator similar to NewIter but it also increments
// the number of bytes iterated. If an error occurs, NewCompactionIter cleans up
// after itself and returns a nil iterator.
func (r *Reader) NewCompactionIter(
	transforms IterTransforms, env block.ReadEnv, rp valblk.ReaderProvider,
) (Iterator, error) {
	return r.newCompactionIter(transforms, env, rp, nil)
}

func (r *Reader) newCompactionIter(
	transforms IterTransforms, env block.ReadEnv, rp valblk.ReaderProvider, vState *virtualState,
) (Iterator, error) {
	if vState != nil && vState.isSharedIngested {
		transforms.HideObsoletePoints = true
	}

	if r.Properties.IndexType == twoLevelIndex {
		if !r.tableFormat.BlockColumnar() {
			i, err := newRowBlockTwoLevelIterator(
				context.Background(),
				r, vState, transforms, nil /* lower */, nil /* upper */, nil,
				NeverUseFilterBlock, env, rp)
			if err != nil {
				return nil, err
			}
			i.SetupForCompaction()
			return i, nil
		}
		i, err := newColumnBlockTwoLevelIterator(
			context.Background(),
			r, vState, transforms, nil /* lower */, nil /* upper */, nil,
			NeverUseFilterBlock, env, rp)
		if err != nil {
			return nil, err
		}
		i.SetupForCompaction()
		return i, nil
	}
	if !r.tableFormat.BlockColumnar() {
		i, err := newRowBlockSingleLevelIterator(
			context.Background(), r, vState, transforms, nil /* lower */, nil, /* upper */
			nil, NeverUseFilterBlock, env, rp)
		if err != nil {
			return nil, err
		}
		i.SetupForCompaction()
		return i, nil
	}
	i, err := newColumnBlockSingleLevelIterator(
		context.Background(), r, vState, transforms, nil /* lower */, nil, /* upper */
		nil, NeverUseFilterBlock, env, rp)
	if err != nil {
		return nil, err
	}
	i.SetupForCompaction()
	return i, nil
}

// NewRawRangeDelIter returns an internal iterator for the contents of the
// range-del block for the table. Returns nil if the table does not contain
// any range deletions.
func (r *Reader) NewRawRangeDelIter(
	ctx context.Context, transforms FragmentIterTransforms, env block.ReadEnv,
) (iter keyspan.FragmentIterator, err error) {
	if r.rangeDelBH.Length == 0 {
		return nil, nil
	}
	// TODO(radu): plumb stats here.
	h, err := r.readRangeDelBlock(ctx, env, noReadHandle, r.rangeDelBH)
	if err != nil {
		return nil, err
	}
	if r.tableFormat.BlockColumnar() {
		iter = colblk.NewKeyspanIter(r.Comparer.Compare, h, transforms)
	} else {
		iter, err = rowblk.NewFragmentIter(r.blockReader.FileNum(), r.Comparer, h, transforms)
		if err != nil {
			return nil, err
		}
	}
	return keyspan.MaybeAssert(iter, r.Comparer.Compare), nil
}

// NewRawRangeKeyIter returns an internal iterator for the contents of the
// range-key block for the table. Returns nil if the table does not contain any
// range keys.
func (r *Reader) NewRawRangeKeyIter(
	ctx context.Context, transforms FragmentIterTransforms, env block.ReadEnv,
) (iter keyspan.FragmentIterator, err error) {
	if r.rangeKeyBH.Length == 0 {
		return nil, nil
	}
	// TODO(radu): plumb stats here.
	h, err := r.readRangeKeyBlock(ctx, env, noReadHandle, r.rangeKeyBH)
	if err != nil {
		return nil, err
	}
	if r.tableFormat.BlockColumnar() {
		iter = colblk.NewKeyspanIter(r.Comparer.Compare, h, transforms)
	} else {
		iter, err = rowblk.NewFragmentIter(r.blockReader.FileNum(), r.Comparer, h, transforms)
		if err != nil {
			return nil, err
		}
	}
	return keyspan.MaybeAssert(iter, r.Comparer.Compare), nil
}

// noReadHandle is used when we don't want to pass a ReadHandle to one of the
// read block methods.
var noReadHandle objstorage.ReadHandle = nil

var noInitBlockMetadataFn = func(*block.Metadata, []byte) error { return nil }

// readMetaindexBlock reads the metaindex block.
func (r *Reader) readMetaindexBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.MetadataBlock)
	return r.blockReader.Read(ctx, env, readHandle, r.metaindexBH, noInitBlockMetadataFn)
}

// readTopLevelIndexBlock reads the top-level index block.
func (r *Reader) readTopLevelIndexBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle,
) (block.BufferHandle, error) {
	return r.readIndexBlock(ctx, env, readHandle, r.indexBH)
}

// readIndexBlock reads a top-level or second-level index block.
func (r *Reader) readIndexBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle, bh block.Handle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.MetadataBlock)
	return r.blockReader.Read(ctx, env, readHandle, bh, r.initIndexBlockMetadata)
}

// initIndexBlockMetadata initializes the Metadata for a data block. This will
// later be used (and reused) when reading from the block.
func (r *Reader) initIndexBlockMetadata(metadata *block.Metadata, data []byte) error {
	if r.tableFormat.BlockColumnar() {
		return colblk.InitIndexBlockMetadata(metadata, data)
	}
	return nil
}

func (r *Reader) readDataBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle, bh block.Handle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.DataBlock)
	return r.blockReader.Read(ctx, env, readHandle, bh, r.initDataBlockMetadata)
}

// initDataBlockMetadata initializes the Metadata for a data block. This will
// later be used (and reused) when reading from the block.
func (r *Reader) initDataBlockMetadata(metadata *block.Metadata, data []byte) error {
	if r.tableFormat.BlockColumnar() {
		return colblk.InitDataBlockMetadata(r.keySchema, metadata, data)
	}
	return nil
}

func (r *Reader) readFilterBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle, bh block.Handle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.FilterBlock)
	return r.blockReader.Read(ctx, env, readHandle, bh, noInitBlockMetadataFn)
}

func (r *Reader) readRangeDelBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle, bh block.Handle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.MetadataBlock)
	return r.blockReader.Read(ctx, env, readHandle, bh, r.initKeyspanBlockMetadata)
}

func (r *Reader) readRangeKeyBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle, bh block.Handle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.MetadataBlock)
	return r.blockReader.Read(ctx, env, readHandle, bh, r.initKeyspanBlockMetadata)
}

// initKeyspanBlockMetadata initializes the Metadata for a rangedel or range key
// block. This will later be used (and reused) when reading from the block.
func (r *Reader) initKeyspanBlockMetadata(metadata *block.Metadata, data []byte) error {
	if r.tableFormat.BlockColumnar() {
		return colblk.InitKeyspanBlockMetadata(metadata, data)
	}
	return nil
}

// ReadValueBlockExternal implements valblk.ExternalBlockReader, allowing a
// base.LazyValue to read a value block.
func (r *Reader) ReadValueBlockExternal(
	ctx context.Context, bh block.Handle,
) (block.BufferHandle, error) {
	return r.readValueBlock(ctx, block.NoReadEnv, noReadHandle, bh)
}

func (r *Reader) readValueBlock(
	ctx context.Context, env block.ReadEnv, readHandle objstorage.ReadHandle, bh block.Handle,
) (block.BufferHandle, error) {
	ctx = objiotracing.WithBlockType(ctx, objiotracing.ValueBlock)
	return r.blockReader.Read(ctx, env, readHandle, bh, noInitBlockMetadataFn)
}

// metaBufferPools is a sync pool of BufferPools used exclusively when opening a
// table and loading its meta blocks.
var metaBufferPools = sync.Pool{
	New: func() any {
		bp := new(block.BufferPool)
		// New pools are initialized with a capacity of 3 to accommodate the
		// meta block (1), and both the compressed properties block (1) and
		// decompressed properties block (1) simultaneously.
		bp.Init(3)
		return bp
	},
}

func (r *Reader) readMetaindex(
	ctx context.Context,
	readHandle objstorage.ReadHandle,
	filters map[string]FilterPolicy,
	deniedUserProperties map[string]struct{},
) error {
	// We use a BufferPool when reading metaindex blocks in order to avoid
	// populating the block cache with these blocks. In heavy-write workloads,
	// especially with high compaction concurrency, new tables may be created
	// frequently. Populating the block cache with these metaindex blocks adds
	// additional contention on the block cache mutexes (see #1997).
	// Additionally, these blocks are exceedingly unlikely to be read again
	// while they're still in the block cache except in misconfigurations with
	// excessive sstables counts or a file cache that's far too small.
	bufferPool := metaBufferPools.Get().(*block.BufferPool)
	defer metaBufferPools.Put(bufferPool)
	// When we're finished, release the buffers we've allocated back to memory
	// allocator.
	defer bufferPool.Release()
	metaEnv := block.ReadEnv{BufferPool: bufferPool}

	b, err := r.readMetaindexBlock(ctx, metaEnv, readHandle)
	if err != nil {
		return err
	}
	data := b.BlockData()
	defer b.Release()

	if uint64(len(data)) != r.metaindexBH.Length {
		return base.CorruptionErrorf("pebble/table: unexpected metaindex block size: %d vs %d",
			errors.Safe(len(data)), errors.Safe(r.metaindexBH.Length))
	}

	var meta map[string]block.Handle
	meta, r.valueBIH, err = decodeMetaindex(data)
	if err != nil {
		return err
	}

	if bh, ok := meta[metaPropertiesName]; ok {
		b, err = r.blockReader.Read(ctx, metaEnv, readHandle, bh, noInitBlockMetadataFn)
		if err != nil {
			return err
		}
		r.propertiesBH = bh
		err := r.Properties.load(b.BlockData(), deniedUserProperties)
		b.Release()
		if err != nil {
			return err
		}
	}

	if bh, ok := meta[metaRangeDelV2Name]; ok {
		r.rangeDelBH = bh
	} else if _, ok := meta[metaRangeDelV1Name]; ok {
		// This version of Pebble requires a format major version at least as
		// high as FormatFlushableIngest (see pebble.FormatMinSupported). In
		// this format major verison, we have a guarantee that we've compacted
		// away all RocksDB sstables. It should not be possible to encounter an
		// sstable with a v1 range deletion block but not a v2 range deletion
		// block.
		err := errors.Newf("pebble/table: unexpected range-del block type: %s", metaRangeDelV1Name)
		return errors.Mark(err, base.ErrCorruption)
	}

	if bh, ok := meta[metaRangeKeyName]; ok {
		r.rangeKeyBH = bh
	}

	for name, fp := range filters {
		if bh, ok := meta["fullfilter."+name]; ok {
			r.filterBH = bh
			r.tableFilter = newTableFilterReader(fp, r.filterMetricsTracker)
			break
		}
	}
	return nil
}

// Layout returns the layout (block organization) for an sstable.
func (r *Reader) Layout() (*Layout, error) {
	if r.err != nil {
		return nil, r.err
	}

	l := &Layout{
		Data:       make([]block.HandleWithProperties, 0, r.Properties.NumDataBlocks),
		RangeDel:   r.rangeDelBH,
		RangeKey:   r.rangeKeyBH,
		ValueIndex: r.valueBIH.Handle,
		Properties: r.propertiesBH,
		MetaIndex:  r.metaindexBH,
		Footer:     r.footerBH,
		Format:     r.tableFormat,
	}
	if r.filterBH.Length > 0 {
		l.Filter = []NamedBlockHandle{{Name: "fullfilter." + r.tableFilter.policy.Name(), Handle: r.filterBH}}
	}
	ctx := context.TODO()

	indexH, err := r.readTopLevelIndexBlock(ctx, block.NoReadEnv, noReadHandle)
	if err != nil {
		return nil, err
	}
	defer indexH.Release()

	var alloc bytealloc.A

	if r.Properties.IndexPartitions == 0 {
		l.Index = append(l.Index, r.indexBH)
		iter := r.tableFormat.newIndexIter()
		err := iter.Init(r.Comparer, indexH.BlockData(), NoTransforms)
		if err != nil {
			return nil, errors.Wrap(err, "reading index block")
		}
		for valid := iter.First(); valid; valid = iter.Next() {
			dataBH, err := iter.BlockHandleWithProperties()
			if err != nil {
				return nil, errCorruptIndexEntry(err)
			}
			if len(dataBH.Props) > 0 {
				alloc, dataBH.Props = alloc.Copy(dataBH.Props)
			}
			l.Data = append(l.Data, dataBH)
		}
	} else {
		l.TopIndex = r.indexBH
		topIter := r.tableFormat.newIndexIter()
		err := topIter.Init(r.Comparer, indexH.BlockData(), NoTransforms)
		if err != nil {
			return nil, errors.Wrap(err, "reading index block")
		}
		iter := r.tableFormat.newIndexIter()
		for valid := topIter.First(); valid; valid = topIter.Next() {
			indexBH, err := topIter.BlockHandleWithProperties()
			if err != nil {
				return nil, errCorruptIndexEntry(err)
			}
			l.Index = append(l.Index, indexBH.Handle)

			subIndex, err := r.readIndexBlock(ctx, block.NoReadEnv, noReadHandle, indexBH.Handle)
			if err != nil {
				return nil, err
			}
			err = func() error {
				defer subIndex.Release()
				// TODO(msbutler): figure out how to pass virtualState to layout call.
				if err := iter.Init(r.Comparer, subIndex.BlockData(), NoTransforms); err != nil {
					return err
				}
				for valid := iter.First(); valid; valid = iter.Next() {
					dataBH, err := iter.BlockHandleWithProperties()
					if err != nil {
						return errCorruptIndexEntry(err)
					}
					if len(dataBH.Props) > 0 {
						alloc, dataBH.Props = alloc.Copy(dataBH.Props)
					}
					l.Data = append(l.Data, dataBH)
				}
				return nil
			}()
			if err != nil {
				return nil, err
			}
		}
	}
	if r.valueBIH.Handle.Length != 0 {
		vbiH, err := r.readValueBlock(context.Background(), block.NoReadEnv, noReadHandle, r.valueBIH.Handle)
		if err != nil {
			return nil, err
		}
		defer vbiH.Release()
		l.ValueBlock, err = valblk.DecodeIndex(vbiH.BlockData(), r.valueBIH)
		if err != nil {
			return nil, err
		}
	}

	return l, nil
}

// ValidateBlockChecksums validates the checksums for each block in the SSTable.
func (r *Reader) ValidateBlockChecksums() error {
	// Pre-compute the BlockHandles for the underlying file.
	l, err := r.Layout()
	if err != nil {
		return err
	}

	type blk struct {
		bh     block.Handle
		readFn func(context.Context, block.ReadEnv, objstorage.ReadHandle, block.Handle) (block.BufferHandle, error)
	}
	// Construct the set of blocks to check. Note that the footer is not checked
	// as it is not a block with a checksum.
	blocks := make([]blk, 0, len(l.Data)+6)
	for i := range l.Data {
		blocks = append(blocks, blk{
			bh:     l.Data[i].Handle,
			readFn: r.readDataBlock,
		})
	}
	for _, h := range l.Index {
		blocks = append(blocks, blk{
			bh:     h,
			readFn: r.readIndexBlock,
		})
	}
	blocks = append(blocks, blk{
		bh:     l.TopIndex,
		readFn: r.readIndexBlock,
	})
	for _, bh := range l.Filter {
		blocks = append(blocks, blk{
			bh:     bh.Handle,
			readFn: r.readFilterBlock,
		})
	}
	blocks = append(blocks, blk{
		bh:     l.RangeDel,
		readFn: r.readRangeDelBlock,
	})
	blocks = append(blocks, blk{
		bh:     l.RangeKey,
		readFn: r.readRangeKeyBlock,
	})
	readNoInit := func(ctx context.Context, env block.ReadEnv, rh objstorage.ReadHandle, bh block.Handle) (block.BufferHandle, error) {
		return r.blockReader.Read(ctx, env, rh, bh, noInitBlockMetadataFn)
	}
	blocks = append(blocks, blk{
		bh:     l.Properties,
		readFn: readNoInit,
	})
	blocks = append(blocks, blk{
		bh:     l.MetaIndex,
		readFn: readNoInit,
	})

	// Sorting by offset ensures we are performing a sequential scan of the
	// file.
	slices.SortFunc(blocks, func(a, b blk) int {
		return cmp.Compare(a.bh.Offset, b.bh.Offset)
	})

	ctx := context.Background()
	for _, b := range blocks {
		// Certain blocks may not be present, in which case we skip them.
		if b.bh.Length == 0 {
			continue
		}
		h, err := b.readFn(ctx, block.NoReadEnv, noReadHandle, b.bh)
		if err != nil {
			return err
		}
		h.Release()
	}

	return nil
}

// CommonProperties implemented the CommonReader interface.
func (r *Reader) CommonProperties() *CommonProperties {
	return &r.Properties.CommonProperties
}

// EstimateDiskUsage returns the total size of data blocks overlapping the range
// `[start, end]`. Even if a data block partially overlaps, or we cannot
// determine overlap due to abbreviated index keys, the full data block size is
// included in the estimation.
//
// This function does not account for any metablock space usage. Assumes there
// is at least partial overlap, i.e., `[start, end]` falls neither completely
// before nor completely after the file's range.
//
// Only blocks containing point keys are considered. Range deletion and range
// key blocks are not considered.
//
// TODO(ajkr): account for metablock space usage. Perhaps look at the fraction of
// data blocks overlapped and add that same fraction of the metadata blocks to the
// estimate.
func (r *Reader) EstimateDiskUsage(start, end []byte) (uint64, error) {
	if !r.tableFormat.BlockColumnar() {
		return estimateDiskUsage[rowblk.IndexIter, *rowblk.IndexIter](r, start, end)
	}
	return estimateDiskUsage[colblk.IndexIter, *colblk.IndexIter](r, start, end)
}

func estimateDiskUsage[I any, PI indexBlockIterator[I]](
	r *Reader, start, end []byte,
) (uint64, error) {
	if r.err != nil {
		return 0, r.err
	}
	ctx := context.TODO()

	indexH, err := r.readTopLevelIndexBlock(ctx, block.NoReadEnv, noReadHandle)
	if err != nil {
		return 0, err
	}
	// We are using InitHandle below but we never Close those iterators, which
	// allows us to release the index handle ourselves.
	// TODO(radu): clean this up.
	defer indexH.Release()

	// Iterators over the bottom-level index blocks containing start and end.
	// These may be different in case of partitioned index but will both point
	// to the same blockIter over the single index in the unpartitioned case.
	var startIdxIter, endIdxIter PI
	if r.Properties.IndexPartitions == 0 {
		startIdxIter = new(I)
		if err := startIdxIter.InitHandle(r.Comparer, indexH, NoTransforms); err != nil {
			return 0, err
		}
		endIdxIter = startIdxIter
	} else {
		var topIter PI = new(I)
		if err := topIter.InitHandle(r.Comparer, indexH, NoTransforms); err != nil {
			return 0, err
		}
		if !topIter.SeekGE(start) {
			// The range falls completely after this file.
			return 0, nil
		}
		startIndexBH, err := topIter.BlockHandleWithProperties()
		if err != nil {
			return 0, errCorruptIndexEntry(err)
		}
		startIdxBlock, err := r.readIndexBlock(ctx, block.NoReadEnv, noReadHandle, startIndexBH.Handle)
		if err != nil {
			return 0, err
		}
		defer startIdxBlock.Release()
		startIdxIter = new(I)
		err = startIdxIter.InitHandle(r.Comparer, startIdxBlock, NoTransforms)
		if err != nil {
			return 0, err
		}

		if topIter.SeekGE(end) {
			endIndexBH, err := topIter.BlockHandleWithProperties()
			if err != nil {
				return 0, errCorruptIndexEntry(err)
			}
			endIdxBlock, err := r.readIndexBlock(ctx, block.NoReadEnv, noReadHandle, endIndexBH.Handle)
			if err != nil {
				return 0, err
			}
			defer endIdxBlock.Release()
			endIdxIter = new(I)
			err = endIdxIter.InitHandle(r.Comparer, endIdxBlock, NoTransforms)
			if err != nil {
				return 0, err
			}
		}
	}
	// startIdxIter should not be nil at this point, while endIdxIter can be if the
	// range spans past the end of the file.

	if !startIdxIter.SeekGE(start) {
		// The range falls completely after this file.
		return 0, nil
	}
	startBH, err := startIdxIter.BlockHandleWithProperties()
	if err != nil {
		return 0, errCorruptIndexEntry(err)
	}

	includeInterpolatedValueBlocksSize := func(dataBlockSize uint64) uint64 {
		// INVARIANT: r.Properties.DataSize > 0 since startIdxIter is not nil.
		// Linearly interpolate what is stored in value blocks.
		//
		// TODO(sumeer): if we need more accuracy, without loading any data blocks
		// (which contain the value handles, and which may also be insufficient if
		// the values are in separate files), we will need to accumulate the
		// logical size of the key-value pairs and store the cumulative value for
		// each data block in the index block entry. This increases the size of
		// the BlockHandle, so wait until this becomes necessary.
		return dataBlockSize +
			uint64((float64(dataBlockSize)/float64(r.Properties.DataSize))*
				float64(r.Properties.ValueBlocksSize))
	}
	if endIdxIter == nil {
		// The range spans beyond this file. Include data blocks through the last.
		return includeInterpolatedValueBlocksSize(r.Properties.DataSize - startBH.Offset), nil
	}
	if !endIdxIter.SeekGE(end) {
		// The range spans beyond this file. Include data blocks through the last.
		return includeInterpolatedValueBlocksSize(r.Properties.DataSize - startBH.Offset), nil
	}
	endBH, err := endIdxIter.BlockHandleWithProperties()
	if err != nil {
		return 0, errCorruptIndexEntry(err)
	}
	return includeInterpolatedValueBlocksSize(
		endBH.Offset + endBH.Length + block.TrailerLen - startBH.Offset), nil
}

// TableFormat returns the format version for the table.
func (r *Reader) TableFormat() (TableFormat, error) {
	if r.err != nil {
		return TableFormatUnspecified, r.err
	}
	return r.tableFormat, nil
}

// NewReader returns a new table reader for the file. Closing the reader will
// close the file.
//
// The context is used for tracing any operations performed by NewReader; it is
// NOT stored for future use.
//
// In error cases, the Readable is closed.
func NewReader(ctx context.Context, f objstorage.Readable, o ReaderOptions) (*Reader, error) {
	if f == nil {
		return nil, errors.New("pebble/table: nil file")
	}
	o = o.ensureDefaults()

	r := &Reader{
		filterMetricsTracker: o.FilterMetricsTracker,
	}

	var preallocRH objstorageprovider.PreallocatedReadHandle
	rh := objstorageprovider.UsePreallocatedReadHandle(
		f, objstorage.ReadBeforeForNewReader, &preallocRH)
	defer rh.Close()

	footer, err := readFooter(ctx, f, rh, o.LoggerAndTracer, o.CacheOpts.FileNum)
	if err != nil {
		return nil, errors.CombineErrors(err, f.Close())
	}
	r.blockReader.Init(f, o.ReaderOptions, footer.checksum)
	r.tableFormat = footer.format
	r.indexBH = footer.indexBH
	r.metaindexBH = footer.metaindexBH
	r.footerBH = footer.footerBH

	// Read the metaindex and properties blocks.
	if err := r.readMetaindex(ctx, rh, o.Filters, o.DeniedUserProperties); err != nil {
		r.err = err
		return nil, r.Close()
	}

	if r.Properties.ComparerName == "" || o.Comparer.Name == r.Properties.ComparerName {
		r.Comparer = o.Comparer
	} else if comparer, ok := o.Comparers[r.Properties.ComparerName]; ok {
		r.Comparer = comparer
	} else {
		r.err = errors.Errorf("pebble/table: %d: unknown comparer %s",
			errors.Safe(r.blockReader.FileNum()), errors.Safe(r.Properties.ComparerName))
	}

	if mergerName := r.Properties.MergerName; mergerName != "" && mergerName != "nullptr" {
		if o.Merger != nil && o.Merger.Name == mergerName {
			// opts.Merger matches.
		} else if _, ok := o.Mergers[mergerName]; ok {
			// Known merger.
		} else {
			r.err = errors.Errorf("pebble/table: %d: unknown merger %s",
				errors.Safe(r.blockReader.FileNum()), errors.Safe(r.Properties.MergerName))
		}
	}

	if r.tableFormat.BlockColumnar() {
		if ks, ok := o.KeySchemas[r.Properties.KeySchemaName]; ok {
			r.keySchema = ks
		} else {
			var known []string
			for name := range o.KeySchemas {
				known = append(known, fmt.Sprintf("%q", name))
			}
			slices.Sort(known)

			r.err = errors.Newf("pebble/table: %d: unknown key schema %q; known key schemas: %s",
				errors.Safe(r.blockReader.FileNum()), errors.Safe(r.Properties.KeySchemaName), errors.Safe(known))
			panic(r.err)
		}
	}

	if r.err != nil {
		return nil, r.Close()
	}

	return r, nil
}

// ReadableFile describes the smallest subset of vfs.File that is required for
// reading SSTs.
type ReadableFile interface {
	io.ReaderAt
	io.Closer
	Stat() (vfs.FileInfo, error)
}

// NewSimpleReadable wraps a ReadableFile in a objstorage.Readable
// implementation (which does not support read-ahead)
func NewSimpleReadable(r ReadableFile) (objstorage.Readable, error) {
	info, err := r.Stat()
	if err != nil {
		return nil, err
	}
	res := &simpleReadable{
		f:    r,
		size: info.Size(),
	}
	res.rh = objstorage.MakeNoopReadHandle(res)
	return res, nil
}

// simpleReadable wraps a ReadableFile to implement objstorage.Readable.
type simpleReadable struct {
	f    ReadableFile
	size int64
	rh   objstorage.NoopReadHandle
}

var _ objstorage.Readable = (*simpleReadable)(nil)

// ReadAt is part of the objstorage.Readable interface.
func (s *simpleReadable) ReadAt(_ context.Context, p []byte, off int64) error {
	n, err := s.f.ReadAt(p, off)
	if invariants.Enabled && err == nil && n != len(p) {
		panic("short read")
	}
	return err
}

// Close is part of the objstorage.Readable interface.
func (s *simpleReadable) Close() error {
	return s.f.Close()
}

// Size is part of the objstorage.Readable interface.
func (s *simpleReadable) Size() int64 {
	return s.size
}

// NewReadHandle is part of the objstorage.Readable interface.
func (s *simpleReadable) NewReadHandle(
	readBeforeSize objstorage.ReadBeforeSize,
) objstorage.ReadHandle {
	return &s.rh
}

func errCorruptIndexEntry(err error) error {
	err = base.CorruptionErrorf("pebble/table: corrupt index entry: %v", err)
	if invariants.Enabled {
		panic(err)
	}
	return err
}

// MakeTrivialReaderProvider creates a valblk.ReaderProvider which always
// returns the given reader. It should be used when the Reader will outlive the
// iterator tree.
func MakeTrivialReaderProvider(r *Reader) valblk.ReaderProvider {
	return (*trivialReaderProvider)(r)
}

// trivialReaderProvider implements valblk.ReaderProvider for a Reader that will
// outlive the top-level iterator in the iterator tree.
//
// Defining the type in this manner (as opposed to a struct) avoids allocation.
type trivialReaderProvider Reader

var _ valblk.ReaderProvider = (*trivialReaderProvider)(nil)

// GetReader implements ReaderProvider.
func (trp *trivialReaderProvider) GetReader(
	ctx context.Context,
) (valblk.ExternalBlockReader, error) {
	return (*Reader)(trp), nil
}

// Close implements ReaderProvider.
func (trp *trivialReaderProvider) Close() {}
