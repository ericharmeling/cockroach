// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package bulk

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/pkg/errors"
)

// BufferingAdder is a wrapper for an SSTBatcher that allows out-of-order calls
// to Add, buffering them up and then sorting them before then passing them in
// order into an SSTBatcher
type BufferingAdder struct {
	sink SSTBatcher
	// timestamp applied to mvcc keys created from keys during SST construction.
	timestamp hlc.Timestamp

	// threshold at which buffered entries will be flushed to SSTBatcher.
	curBufferSize uint64

	// ceiling till which we can grow curBufferSize if bulkMon permits.
	maxBufferSize uint64

	// unit by which we increment the curBufferSize.
	incrementBufferSize uint64

	// currently buffered kvs.
	curBuf kvBuf

	flushCounts struct {
		total      int
		bufferSize int
	}

	// name of the BufferingAdder for the purpose of logging only.
	name string

	bulkMon *mon.BytesMonitor
	memAcc  mon.BoundAccount

	onFlush func()
}

var _ storagebase.BulkAdder = &BufferingAdder{}

// MakeBulkAdder makes a storagebase.BulkAdder that buffers and sorts K/Vs
// passed to add into SSTs that are then ingested. rangeCache if set is
// consulted to avoid generating an SST that will span a range boundary and thus
// encounter an error and need to be split and retired to be applied.
func MakeBulkAdder(
	ctx context.Context,
	db sender,
	rangeCache *kv.RangeDescriptorCache,
	timestamp hlc.Timestamp,
	opts storagebase.BulkAdderOptions,
	bulkMon *mon.BytesMonitor,
) (*BufferingAdder, error) {
	if opts.MinBufferSize == 0 {
		opts.MinBufferSize = 32 << 20
	}
	if opts.MaxBufferSize == 0 {
		opts.MaxBufferSize = 128 << 20
	}
	if opts.StepBufferSize == 0 {
		opts.StepBufferSize = 32 << 20
	}
	if opts.SSTSize == 0 {
		opts.SSTSize = 16 << 20
	}

	b := &BufferingAdder{
		name: opts.Name,
		sink: SSTBatcher{
			db:                db,
			maxSize:           int64(opts.SSTSize),
			rc:                rangeCache,
			skipDuplicates:    opts.SkipDuplicates,
			disallowShadowing: opts.DisallowShadowing,
		},
		timestamp:           timestamp,
		curBufferSize:       opts.MinBufferSize,
		maxBufferSize:       opts.MaxBufferSize,
		incrementBufferSize: opts.StepBufferSize,
		bulkMon:             bulkMon,
	}

	// If no monitor is attached to the instance of a bulk adder, we do not
	// control its memory usage.
	if bulkMon == nil {
		return b, nil
	}

	// At minimum a bulk adder needs enough space to store a buffer of
	// curBufferSize, and a subsequent SST of SSTSize in-memory. If the memory
	// account is unable to reserve this minimum threshold we cannot continue.
	//
	// TODO(adityamaru): IMPORT should also reserve memory for a single SST which
	// it will store in-memory before sending it to RocksDB.
	b.memAcc = bulkMon.MakeBoundAccount()
	if err := b.memAcc.Grow(ctx, int64(b.curBufferSize)); err != nil {
		return nil, errors.Wrap(err, "Not enough memory available to create a BulkAdder. Try setting a higher --max-sql-memory.")
	}

	return b, nil
}

// SetOnFlush sets a callback to run after the buffering adder flushes.
func (b *BufferingAdder) SetOnFlush(fn func()) {
	b.onFlush = fn
}

// Close closes the underlying SST builder.
func (b *BufferingAdder) Close(ctx context.Context) {
	log.VEventf(ctx, 2,
		"bulk adder %s ingested %s, flushed %d times, %d due to buffer size. Flushed %d files, %d due to ranges, %d due to sst size. Used %d bytes.",
		b.name,
		sz(b.sink.totalRows.DataSize),
		b.flushCounts.total, b.flushCounts.bufferSize,
		b.sink.flushCounts.total, b.sink.flushCounts.split, b.sink.flushCounts.sstSize,
		b.memAcc.Used(),
	)
	b.sink.Close()

	if b.bulkMon != nil {
		b.memAcc.Close(ctx)
		b.bulkMon.Stop(ctx)
	}
}

// Add adds a key to the buffer and checks if it needs to flush.
func (b *BufferingAdder) Add(ctx context.Context, key roachpb.Key, value []byte) error {
	if err := b.curBuf.append(key, value); err != nil {
		return err
	}

	if b.curBuf.MemSize > int(b.curBufferSize) {
		// This is an optimization to try and increase the current buffer size if
		// our memory account permits it. This would lead to creation of a fewer
		// number of SSTs.
		//
		// To prevent a single import from growing its buffer indefinitely we check
		// if it has exceeded its upper bound.
		if b.bulkMon != nil && b.curBufferSize < b.maxBufferSize {
			if err := b.memAcc.Grow(ctx, int64(b.incrementBufferSize)); err != nil {
				// If we are unable to reserve the additional memory then flush the
				// buffer, and continue as normal.
				b.flushCounts.bufferSize++
				log.VEventf(ctx, 3, "buffer size triggering flush of %s buffer", sz(b.curBuf.MemSize))
				return b.Flush(ctx)
			}
			b.curBufferSize += b.incrementBufferSize
		} else {
			b.flushCounts.bufferSize++
			log.VEventf(ctx, 3, "buffer size triggering flush of %s buffer", sz(b.curBuf.MemSize))
			return b.Flush(ctx)
		}
	}
	return nil
}

// CurrentBufferFill returns the current buffer fill percentage.
func (b *BufferingAdder) CurrentBufferFill() float32 {
	return float32(b.curBuf.MemSize) / float32(b.curBufferSize)
}

// IsEmpty returns true if the adder has no un-flushed data in its buffer.
func (b *BufferingAdder) IsEmpty() bool {
	return b.curBuf.Len() == 0
}

// Flush flushes any buffered kvs to the batcher.
func (b *BufferingAdder) Flush(ctx context.Context) error {
	if b.curBuf.Len() == 0 {
		if b.onFlush != nil {
			b.onFlush()
		}
		return nil
	}
	if err := b.sink.Reset(); err != nil {
		return err
	}
	b.flushCounts.total++

	before := b.sink.flushCounts
	beforeSize := b.sink.totalRows.DataSize

	sort.Sort(&b.curBuf)
	mvccKey := engine.MVCCKey{Timestamp: b.timestamp}

	for i := range b.curBuf.entries {
		mvccKey.Key = b.curBuf.Key(i)
		if err := b.sink.AddMVCCKey(ctx, mvccKey, b.curBuf.Value(i)); err != nil {
			return err
		}
	}
	if err := b.sink.Flush(ctx); err != nil {
		return err
	}

	if log.V(3) {
		written := b.sink.totalRows.DataSize - beforeSize
		files := b.sink.flushCounts.total - before.total
		dueToSplits := b.sink.flushCounts.split - before.split
		dueToSize := b.sink.flushCounts.sstSize - before.sstSize

		log.Infof(ctx,
			"flushing %s buffer wrote %d SSTs (avg: %s) with %d for splits, %d for size",
			sz(b.curBuf.MemSize), files, sz(written/int64(files)), dueToSplits, dueToSize,
		)
	}
	if b.onFlush != nil {
		b.onFlush()
	}
	b.curBuf.Reset()
	return nil
}

// GetSummary returns this batcher's total added rows/bytes/etc.
func (b *BufferingAdder) GetSummary() roachpb.BulkOpSummary {
	return b.sink.GetSummary()
}
