// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"context"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/rangekey"
)

// ErrSnapshotExcised is returned from WaitForFileOnlySnapshot if an excise
// overlapping with one of the EventuallyFileOnlySnapshot's KeyRanges gets
// applied before the transition of that EFOS to a file-only snapshot.
var ErrSnapshotExcised = errors.New("pebble: snapshot excised before conversion to file-only snapshot")

// Snapshot provides a read-only point-in-time view of the DB state.
type Snapshot struct {
	// The db the snapshot was created from.
	db     *DB
	seqNum uint64

	// Set if part of an EventuallyFileOnlySnapshot.
	efos *EventuallyFileOnlySnapshot

	// The list the snapshot is linked into.
	list *snapshotList

	// The next/prev link for the snapshotList doubly-linked list of snapshots.
	prev, next *Snapshot
}

var _ Reader = (*Snapshot)(nil)

// Get gets the value for the given key. It returns ErrNotFound if the Snapshot
// does not contain the key.
//
// The caller should not modify the contents of the returned slice, but it is
// safe to modify the contents of the argument after Get returns. The returned
// slice will remain valid until the returned Closer is closed. On success, the
// caller MUST call closer.Close() or a memory leak will occur.
func (s *Snapshot) Get(key []byte) ([]byte, io.Closer, error) {
	if s.db == nil {
		panic(ErrClosed)
	}
	return s.db.getInternal(key, nil /* batch */, s)
}

// NewIter returns an iterator that is unpositioned (Iterator.Valid() will
// return false). The iterator can be positioned via a call to SeekGE,
// SeekLT, First or Last.
func (s *Snapshot) NewIter(o *IterOptions) (*Iterator, error) {
	return s.NewIterWithContext(context.Background(), o)
}

// NewIterWithContext is like NewIter, and additionally accepts a context for
// tracing.
func (s *Snapshot) NewIterWithContext(ctx context.Context, o *IterOptions) (*Iterator, error) {
	if s.db == nil {
		panic(ErrClosed)
	}
	return s.db.newIter(ctx, nil /* batch */, snapshotIterOpts{seqNum: s.seqNum}, o), nil
}

// ScanInternal scans all internal keys within the specified bounds, truncating
// any rangedels and rangekeys to those bounds. For use when an external user
// needs to be aware of all internal keys that make up a key range.
//
// See comment on db.ScanInternal for the behaviour that can be expected of
// point keys deleted by range dels and keys masked by range keys.
func (s *Snapshot) ScanInternal(
	ctx context.Context,
	lower, upper []byte,
	visitPointKey func(key *InternalKey, value LazyValue, iterInfo IteratorLevel) error,
	visitRangeDel func(start, end []byte, seqNum uint64) error,
	visitRangeKey func(start, end []byte, keys []rangekey.Key) error,
	visitSharedFile func(sst *SharedSSTMeta) error,
) error {
	if s.db == nil {
		panic(ErrClosed)
	}
	scanInternalOpts := &scanInternalOptions{
		visitPointKey:    visitPointKey,
		visitRangeDel:    visitRangeDel,
		visitRangeKey:    visitRangeKey,
		visitSharedFile:  visitSharedFile,
		skipSharedLevels: visitSharedFile != nil,
		IterOptions: IterOptions{
			KeyTypes:   IterKeyTypePointsAndRanges,
			LowerBound: lower,
			UpperBound: upper,
		},
	}

	iter := s.db.newInternalIter(snapshotIterOpts{seqNum: s.seqNum}, scanInternalOpts)
	defer iter.close()

	return scanInternalImpl(ctx, lower, upper, iter, scanInternalOpts)
}

// closeLocked is similar to Close(), except it requires that db.mu be held
// by the caller.
func (s *Snapshot) closeLocked() error {
	s.db.mu.snapshots.remove(s)

	// If s was the previous earliest snapshot, we might be able to reclaim
	// disk space by dropping obsolete records that were pinned by s.
	if e := s.db.mu.snapshots.earliest(); e > s.seqNum {
		s.db.maybeScheduleCompactionPicker(pickElisionOnly)
	}
	s.db = nil
	return nil
}

// Close closes the snapshot, releasing its resources. Close must be called.
// Failure to do so will result in a tiny memory leak and a large leak of
// resources on disk due to the entries the snapshot is preventing from being
// deleted.
//
// d.mu must NOT be held by the caller.
func (s *Snapshot) Close() error {
	db := s.db
	if db == nil {
		panic(ErrClosed)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return s.closeLocked()
}

type snapshotList struct {
	root Snapshot
}

func (l *snapshotList) init() {
	l.root.next = &l.root
	l.root.prev = &l.root
}

func (l *snapshotList) empty() bool {
	return l.root.next == &l.root
}

func (l *snapshotList) count() int {
	if l.empty() {
		return 0
	}
	var count int
	for i := l.root.next; i != &l.root; i = i.next {
		count++
	}
	return count
}

func (l *snapshotList) earliest() uint64 {
	v := uint64(math.MaxUint64)
	if !l.empty() {
		v = l.root.next.seqNum
	}
	return v
}

func (l *snapshotList) toSlice() []uint64 {
	if l.empty() {
		return nil
	}
	var results []uint64
	for i := l.root.next; i != &l.root; i = i.next {
		results = append(results, i.seqNum)
	}
	return results
}

func (l *snapshotList) pushBack(s *Snapshot) {
	if s.list != nil || s.prev != nil || s.next != nil {
		panic("pebble: snapshot list is inconsistent")
	}
	s.prev = l.root.prev
	s.prev.next = s
	s.next = &l.root
	s.next.prev = s
	s.list = l
}

func (l *snapshotList) remove(s *Snapshot) {
	if s == &l.root {
		panic("pebble: cannot remove snapshot list root node")
	}
	if s.list != l {
		panic("pebble: snapshot list is inconsistent")
	}
	s.prev.next = s.next
	s.next.prev = s.prev
	s.next = nil // avoid memory leaks
	s.prev = nil // avoid memory leaks
	s.list = nil // avoid memory leaks
}

// EventuallyFileOnlySnapshot (aka EFOS) provides a read-only point-in-time view
// of the database state, similar to Snapshot. A EventuallyFileOnlySnapshot
// induces less write amplification than Snapshot, at the cost of increased space
// amplification. While a Snapshot may increase write amplification across all
// flushes and compactions for the duration of its lifetime, an
// EventuallyFileOnlySnapshot only incurs that cost for flushes/compactions if
// memtables at the time of EFOS instantiation contained keys that the EFOS is
// interested in (i.e. its protectedRanges). In that case, the EFOS prevents
// elision of keys visible to it, similar to a Snapshot, until those memtables
// are flushed, and once that happens, the "EventuallyFileOnlySnapshot"
// transitions to a file-only snapshot state in which it pins zombies sstables
// like an open Iterator would, without pinning any memtables. Callers that can
// tolerate the increased space amplification of pinning zombie sstables until
// the snapshot is closed may prefer EventuallyFileOnlySnapshots for their
// reduced write amplification. Callers that desire the benefits of the file-only
// state that requires no pinning of memtables should call
// `WaitForFileOnlySnapshot()` (and possibly re-mint an EFOS if it returns
// ErrSnapshotExcised) before relying on the EFOS to keep producing iterators
// with zero write-amp and zero pinning of memtables in memory.
//
// EventuallyFileOnlySnapshots interact with the IngestAndExcise operation in
// subtle ways. Unlike Snapshots, EFOS guarantees that their read-only
// point-in-time view is unaltered by the excision. However, if a concurrent
// excise were to happen on one of the protectedRanges, WaitForFileOnlySnapshot()
// would return ErrSnapshotExcised and the EFOS would maintain a reference to the
// underlying readState (and by extension, zombie memtables) for its lifetime.
// This could lead to increased memory utilization, which is why callers should
// call WaitForFileOnlySnapshot() if they expect an EFOS to be long-lived.
type EventuallyFileOnlySnapshot struct {
	mu struct {
		// NB: If both this mutex and db.mu are being grabbed, db.mu should be
		// grabbed _before_ grabbing this one.
		sync.Mutex

		// transitioned is signalled when this EFOS transitions to being a file-only
		// snapshot.
		transitioned sync.Cond

		// Either the {snap,readState} fields are set below, or the version is set at
		// any given point of time. If a snapshot is referenced, this is not a
		// file-only snapshot yet, and if a version is set (and ref'd) this is a
		// file-only snapshot.

		// The wrapped regular snapshot, if not a file-only snapshot yet. The
		// readState has already been ref()d once if it's set.
		snap      *Snapshot
		readState *readState
		// The wrapped version reference, if a file-only snapshot.
		vers *version
	}

	// Key ranges to watch for an excise on.
	protectedRanges []KeyRange
	// excised, if true, signals that the above ranges were excised during the
	// lifetime of this snapshot.
	excised atomic.Bool

	// The db the snapshot was created from.
	db     *DB
	seqNum uint64

	closed chan struct{}
}

func (d *DB) makeEventuallyFileOnlySnapshot(
	keyRanges []KeyRange, internalKeyRanges []internalKeyRange,
) *EventuallyFileOnlySnapshot {
	isFileOnly := true

	d.mu.Lock()
	defer d.mu.Unlock()
	seqNum := d.mu.versions.visibleSeqNum.Load()
	// Check if any of the keyRanges overlap with a memtable.
	for i := range d.mu.mem.queue {
		mem := d.mu.mem.queue[i]
		if ingestMemtableOverlaps(d.cmp, mem, internalKeyRanges) {
			isFileOnly = false
			break
		}
	}
	es := &EventuallyFileOnlySnapshot{
		db:              d,
		seqNum:          seqNum,
		protectedRanges: keyRanges,
		closed:          make(chan struct{}),
	}
	es.mu.transitioned.L = &es.mu
	if isFileOnly {
		es.mu.vers = d.mu.versions.currentVersion()
		es.mu.vers.Ref()
	} else {
		s := &Snapshot{
			db:     d,
			seqNum: seqNum,
		}
		s.efos = es
		es.mu.snap = s
		es.mu.readState = d.loadReadState()
		d.mu.snapshots.pushBack(s)
	}
	return es
}

// Transitions this EventuallyFileOnlySnapshot to a file-only snapshot. Requires
// earliestUnflushedSeqNum and vers to correspond to the same Version from the
// current or a past acquisition of db.mu. vers must have been Ref()'d before
// that mutex was released, if it was released.
//
// NB: The caller is expected to check for es.excised before making this
// call.
//
// d.mu must be held when calling this method.
func (es *EventuallyFileOnlySnapshot) transitionToFileOnlySnapshot(vers *version) error {
	es.mu.Lock()
	select {
	case <-es.closed:
		vers.UnrefLocked()
		es.mu.Unlock()
		return ErrClosed
	default:
	}
	if es.mu.snap == nil {
		es.mu.Unlock()
		panic("pebble: tried to transition an eventually-file-only-snapshot twice")
	}
	// The caller has already called Ref() on vers.
	es.mu.vers = vers
	// NB: The callers should have already done a check of es.excised.
	oldSnap := es.mu.snap
	oldReadState := es.mu.readState
	es.mu.snap = nil
	es.mu.readState = nil
	es.mu.transitioned.Broadcast()
	es.mu.Unlock()
	// It's okay to close a snapshot even if iterators are already open on it.
	oldReadState.unrefLocked()
	return oldSnap.closeLocked()
}

// releaseReadState is called to release reference to a readState when
// es.excised == true. This is to free up memory as quickly as possible; all
// other snapshot resources are kept around until Close() is called. Safe for
// idempotent calls.
//
// d.mu must be held when calling this method.
func (es *EventuallyFileOnlySnapshot) releaseReadState() {
	if !es.excised.Load() {
		panic("pebble: releasing read state of eventually-file-only-snapshot but was not excised")
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.mu.readState != nil {
		es.mu.readState.unrefLocked()
		es.db.maybeScheduleObsoleteTableDeletionLocked()
	}
}

// WaitForFileOnlySnapshot blocks the calling goroutine until this snapshot
// has been converted into a file-only snapshot (i.e. all memtables containing
// keys < seqNum are flushed). A duration can be passed in, and if nonzero,
// a delayed flush will be scheduled at that duration if necessary.
//
// Idempotent; can be called multiple times with no side effects.
func (es *EventuallyFileOnlySnapshot) WaitForFileOnlySnapshot(dur time.Duration) error {
	es.mu.Lock()
	if es.mu.vers != nil {
		// Fast path.
		es.mu.Unlock()
		return nil
	}
	es.mu.Unlock()

	es.db.mu.Lock()
	earliestUnflushedSeqNum := es.db.getEarliestUnflushedSeqNumLocked()
	for earliestUnflushedSeqNum < es.seqNum {
		select {
		case <-es.closed:
			es.db.mu.Unlock()
			return ErrClosed
		default:
		}
		// Check if the current mutable memtable contains keys less than seqNum.
		// If so, rotate it.
		if es.db.mu.mem.mutable.logSeqNum < es.seqNum && dur.Nanoseconds() > 0 {
			es.db.maybeScheduleDelayedFlush(es.db.mu.mem.mutable, dur)
		} else {
			es.db.maybeScheduleFlush()
		}
		es.db.mu.compact.cond.Wait()

		earliestUnflushedSeqNum = es.db.getEarliestUnflushedSeqNumLocked()
	}
	if es.excised.Load() {
		es.db.mu.Unlock()
		return ErrSnapshotExcised
	}
	es.db.mu.Unlock()

	es.mu.Lock()
	defer es.mu.Unlock()

	// Wait for transition to file-only snapshot.
	if es.mu.vers == nil {
		es.mu.transitioned.Wait()
	}
	return nil
}

// Close closes the file-only snapshot and releases all referenced resources.
// Not idempotent.
func (es *EventuallyFileOnlySnapshot) Close() error {
	close(es.closed)
	es.db.mu.Lock()
	defer es.db.mu.Unlock()
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.mu.snap != nil {
		if err := es.mu.snap.closeLocked(); err != nil {
			return err
		}
	}
	if es.mu.readState != nil {
		es.mu.readState.unrefLocked()
		es.db.maybeScheduleObsoleteTableDeletionLocked()
	}
	if es.mu.vers != nil {
		es.mu.vers.UnrefLocked()
	}
	return nil
}

// Get implements the Reader interface.
func (es *EventuallyFileOnlySnapshot) Get(key []byte) (value []byte, closer io.Closer, err error) {
	panic("unimplemented")
}

// NewIter returns an iterator that is unpositioned (Iterator.Valid() will
// return false). The iterator can be positioned via a call to SeekGE,
// SeekLT, First or Last.
func (es *EventuallyFileOnlySnapshot) NewIter(o *IterOptions) (*Iterator, error) {
	return es.NewIterWithContext(context.Background(), o)
}

// NewIterWithContext is like NewIter, and additionally accepts a context for
// tracing.
func (es *EventuallyFileOnlySnapshot) NewIterWithContext(
	ctx context.Context, o *IterOptions,
) (*Iterator, error) {
	select {
	case <-es.closed:
		panic(ErrClosed)
	default:
	}

	es.mu.Lock()
	defer es.mu.Unlock()
	if es.mu.vers != nil {
		sOpts := snapshotIterOpts{seqNum: es.seqNum, vers: es.mu.vers}
		return es.db.newIter(ctx, nil /* batch */, sOpts, o), nil
	}

	if es.excised.Load() {
		return nil, ErrSnapshotExcised
	}
	sOpts := snapshotIterOpts{seqNum: es.seqNum, readState: es.mu.readState}
	return es.db.newIter(ctx, nil /* batch */, sOpts, o), nil
}

// ScanInternal scans all internal keys within the specified bounds, truncating
// any rangedels and rangekeys to those bounds. For use when an external user
// needs to be aware of all internal keys that make up a key range.
//
// See comment on db.ScanInternal for the behaviour that can be expected of
// point keys deleted by range dels and keys masked by range keys.
func (es *EventuallyFileOnlySnapshot) ScanInternal(
	ctx context.Context,
	lower, upper []byte,
	visitPointKey func(key *InternalKey, value LazyValue, iterInfo IteratorLevel) error,
	visitRangeDel func(start, end []byte, seqNum uint64) error,
	visitRangeKey func(start, end []byte, keys []rangekey.Key) error,
	visitSharedFile func(sst *SharedSSTMeta) error,
) error {
	if es.db == nil {
		panic(ErrClosed)
	}
	if es.excised.Load() {
		return ErrSnapshotExcised
	}
	var sOpts snapshotIterOpts
	es.mu.Lock()
	if es.mu.vers != nil {
		sOpts = snapshotIterOpts{
			seqNum: es.seqNum,
			vers:   es.mu.vers,
		}
	} else {
		sOpts = snapshotIterOpts{
			seqNum:    es.seqNum,
			readState: es.mu.readState,
		}
	}
	es.mu.Unlock()
	opts := &scanInternalOptions{
		IterOptions: IterOptions{
			KeyTypes:   IterKeyTypePointsAndRanges,
			LowerBound: lower,
			UpperBound: upper,
		},
		visitPointKey:    visitPointKey,
		visitRangeDel:    visitRangeDel,
		visitRangeKey:    visitRangeKey,
		visitSharedFile:  visitSharedFile,
		skipSharedLevels: visitSharedFile != nil,
	}
	iter := es.db.newInternalIter(sOpts, opts)
	defer iter.close()

	return scanInternalImpl(ctx, lower, upper, iter, opts)
}
