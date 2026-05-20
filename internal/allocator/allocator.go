package allocator

import (
	"fmt"
	"sync"

	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/state"
)

// Allocator owns numeric entity counters with in-RAM segment caching.
//
// Disk semantics: state.Record.CurrentIteration is the reserved high-water mark — the
// smallest iteration not yet allocated to a segment. Each segment reservation advances
// the disk value by segmentSize in one atomic write. In-RAM, a cursor walks through the
// reserved range and emits IDs without touching disk. Crash forfeits the unused tail of
// the in-flight segment, never duplicates IDs.
type Allocator struct {
	shardID     int64
	totalShards int64
	segmentSize int64
	watermark   float64
	store       *state.Store

	mu       sync.Mutex
	segments map[string]*segment
	wg       sync.WaitGroup // tracks in-flight refill goroutines
}

// Close blocks until all background refill goroutines have finished.
// Call before shutting down to avoid leaking temp state files.
func (a *Allocator) Close() {
	a.wg.Wait()
}

type segment struct {
	iterStart int64 // inclusive
	iterEnd   int64 // exclusive
	cursor    int64 // next iteration to emit; iterStart <= cursor <= iterEnd

	refillInFlight bool
	next           *segment // pre-fetched next segment, if refill already wrote to disk
}

type Config struct {
	ShardID     int
	TotalShards int
	SegmentSize int
	Watermark   float64
	Store       *state.Store
}

func New(cfg Config) *Allocator {
	return &Allocator{
		shardID:     int64(cfg.ShardID),
		totalShards: int64(cfg.TotalShards),
		segmentSize: int64(cfg.SegmentSize),
		watermark:   cfg.Watermark,
		store:       cfg.Store,
		segments:    make(map[string]*segment),
	}
}

// ComputeValue maps an iteration index to its emitted value on this shard.
// Shard k of N: iter 0 -> k, iter 1 -> k+N, iter 2 -> k+2N, ...
func (a *Allocator) ComputeValue(iter int64) int64 {
	return a.shardID + iter*a.totalShards
}

// IterFromValue is the inverse. Caller must ensure v has the right residue.
func (a *Allocator) IterFromValue(v int64) int64 {
	return (v - a.shardID) / a.totalShards
}

// SnapForward rounds v up to the nearest value (>= v) whose residue mod totalShards
// matches this shard's residue. If v already matches, returns v unchanged.
func (a *Allocator) SnapForward(v int64) int64 {
	rem := ((v % a.totalShards) + a.totalShards) % a.totalShards
	target := a.shardID % a.totalShards
	if rem == target {
		return v
	}
	diff := target - rem
	if diff < 0 {
		diff += a.totalShards
	}
	return v + diff
}

// NextID returns one numeric ID for the entity. Handles segment reservation and refill.
func (a *Allocator) NextID(entityName string, dt datatype.DataType) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	seg, err := a.ensureSegmentLocked(entityName, dt)
	if err != nil {
		return 0, err
	}
	if seg.cursor >= seg.iterEnd {
		// drained but no next prepared — reserve inline
		if seg.next == nil {
			n, err := a.reserveOneSegment(entityName)
			if err != nil {
				return 0, err
			}
			a.segments[entityName] = n
			seg = n
		} else {
			a.segments[entityName] = seg.next
			seg = seg.next
		}
	}

	iter := seg.cursor
	seg.cursor++

	consumed := seg.cursor - seg.iterStart
	if !seg.refillInFlight && seg.next == nil &&
		float64(consumed)/float64(a.segmentSize) >= a.watermark {
		a.triggerRefillLocked(entityName)
	}

	val := a.ComputeValue(iter)
	if err := CheckOverflow(dt, val); err != nil {
		return 0, err
	}
	return val, nil
}

// NextBatch reserves segments inline (single durable write) to cover n IDs at once.
func (a *Allocator) NextBatch(entityName string, dt datatype.DataType, n int) ([]int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	seg, err := a.ensureSegmentLocked(entityName, dt)
	if err != nil {
		return nil, err
	}

	needed := int64(n)
	available := seg.iterEnd - seg.cursor
	// fold pre-fetched next into available
	if seg.next != nil {
		available += seg.next.iterEnd - seg.next.iterStart
	}
	if available < needed {
		extra := needed - available
		extraSegments := (extra + a.segmentSize - 1) / a.segmentSize
		rec, ok := a.store.Get(entityName)
		if !ok {
			return nil, fmt.Errorf("entity %q vanished during batch reservation", entityName)
		}
		newDisk := rec.CurrentIteration + extraSegments*a.segmentSize
		rec.CurrentIteration = newDisk
		if err := a.store.Put(*rec); err != nil {
			return nil, fmt.Errorf("reserve segments for batch: %w", err)
		}
		// extend current segment end (or its pre-fetched next) — simplest: collapse next into a single big segment
		if seg.next != nil {
			seg.iterEnd = seg.next.iterEnd + extraSegments*a.segmentSize
			seg.next = nil
		} else {
			seg.iterEnd += extraSegments * a.segmentSize
		}
	}

	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		// roll over into pre-fetched next segment if needed (it was folded above, so this is rare)
		if seg.cursor >= seg.iterEnd && seg.next != nil {
			a.segments[entityName] = seg.next
			seg = seg.next
		}
		val := a.ComputeValue(seg.cursor)
		if err := CheckOverflow(dt, val); err != nil {
			return nil, err
		}
		out = append(out, val)
		seg.cursor++
	}
	return out, nil
}

// SegmentSnapshot returns the diagnostic view for /api/segment-state.
type SegmentSnapshot struct {
	Size              int64    `json:"size"`
	Cursor            int64    `json:"cursor"`
	Remaining         int64    `json:"remaining"`
	Watermark         float64  `json:"watermark"`
	RefillInFlight    bool     `json:"refillInFlight"`
	NextSegmentStart  *string  `json:"nextSegmentStart,omitempty"`
	DiskHighWaterMark string   `json:"diskHighWaterMark"`
}

func (a *Allocator) Snapshot(entityName string) (*SegmentSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	seg, ok := a.segments[entityName]
	if !ok {
		return nil, fmt.Errorf("no in-memory segment for entity %q", entityName)
	}
	rec, _ := a.store.Get(entityName)
	snap := &SegmentSnapshot{
		Size:              a.segmentSize,
		Cursor:            seg.cursor - seg.iterStart,
		Remaining:         seg.iterEnd - seg.cursor,
		Watermark:         a.watermark,
		RefillInFlight:    seg.refillInFlight,
		DiskHighWaterMark: "0",
	}
	if seg.next != nil {
		s := fmt.Sprintf("%d", a.ComputeValue(seg.next.iterStart))
		snap.NextSegmentStart = &s
	}
	if rec != nil {
		snap.DiskHighWaterMark = fmt.Sprintf("%d", a.ComputeValue(rec.CurrentIteration))
	}
	return snap, nil
}

// CurrentValue returns the most recently emitted value for an entity (RAM cursor if known,
// else nil). For uninitialised numeric entities, returns ok=false.
func (a *Allocator) CurrentValue(entityName string) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seg, ok := a.segments[entityName]; ok && seg.cursor > seg.iterStart {
		return a.ComputeValue(seg.cursor - 1), true
	}
	return 0, false
}

// NextValue returns the next value the allocator would emit for an entity.
// Reads in-RAM segment if present; otherwise from the persisted record.
func (a *Allocator) NextValue(entityName string) (int64, bool) {
	a.mu.Lock()
	if seg, ok := a.segments[entityName]; ok && seg.cursor < seg.iterEnd {
		v := a.ComputeValue(seg.cursor)
		a.mu.Unlock()
		return v, true
	}
	a.mu.Unlock()
	rec, ok := a.store.Get(entityName)
	if !ok {
		return 0, false
	}
	return a.ComputeValue(rec.CurrentIteration), true
}

// Invalidate drops the in-RAM segment for an entity. Subsequent NextID calls will
// re-read the State store and reserve a fresh segment from the on-disk high-water mark.
// Used by /api/reset so an operator's snap-forward to a new value takes effect immediately.
func (a *Allocator) Invalidate(entityName string) {
	a.mu.Lock()
	delete(a.segments, entityName)
	a.mu.Unlock()
}

// ----- internal helpers -----

func (a *Allocator) ensureSegmentLocked(entityName string, dt datatype.DataType) (*segment, error) {
	if seg, ok := a.segments[entityName]; ok {
		return seg, nil
	}
	rec, ok := a.store.Get(entityName)
	if !ok {
		return nil, fmt.Errorf("entity %q not registered", entityName)
	}
	if rec.DataType != dt {
		return nil, fmt.Errorf("entity %q is registered as %s, not %s", entityName, rec.DataType, dt)
	}
	return a.reserveOneSegment(entityName)
}

// reserveOneSegment advances disk by one segmentSize and returns a fresh in-RAM segment.
// Caller must hold a.mu. Also installs the segment in a.segments.
func (a *Allocator) reserveOneSegment(entityName string) (*segment, error) {
	rec, ok := a.store.Get(entityName)
	if !ok {
		return nil, fmt.Errorf("entity %q vanished during reservation", entityName)
	}
	iterStart := rec.CurrentIteration
	iterEnd := iterStart + a.segmentSize
	rec.CurrentIteration = iterEnd
	if err := a.store.Put(*rec); err != nil {
		return nil, fmt.Errorf("reserve segment for %q: %w", entityName, err)
	}
	seg := &segment{
		iterStart: iterStart,
		iterEnd:   iterEnd,
		cursor:    iterStart,
	}
	a.segments[entityName] = seg
	return seg, nil
}

// triggerRefillLocked starts a background reservation for the next segment.
// Caller must hold a.mu.
func (a *Allocator) triggerRefillLocked(entityName string) {
	seg := a.segments[entityName]
	seg.refillInFlight = true
	a.wg.Add(1)
	go a.runRefill(entityName)
}

func (a *Allocator) runRefill(entityName string) {
	defer a.wg.Done()
	// reserve outside the lock — the disk write is the slow part
	a.mu.Lock()
	rec, ok := a.store.Get(entityName)
	a.mu.Unlock()
	if !ok {
		return
	}
	iterStart := rec.CurrentIteration
	iterEnd := iterStart + a.segmentSize
	rec.CurrentIteration = iterEnd
	if err := a.store.Put(*rec); err != nil {
		a.mu.Lock()
		if s, ok := a.segments[entityName]; ok {
			s.refillInFlight = false
		}
		a.mu.Unlock()
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.segments[entityName]
	if !ok {
		return
	}
	next := &segment{
		iterStart: iterStart,
		iterEnd:   iterEnd,
		cursor:    iterStart,
	}
	if cur.cursor >= cur.iterEnd {
		a.segments[entityName] = next
	} else {
		cur.next = next
		cur.refillInFlight = false
	}
}

// CheckOverflow returns an error if v exceeds the type's maximum.
func CheckOverflow(dt datatype.DataType, v int64) error {
	if dt == datatype.Int && v > datatype.MaxInt {
		return fmt.Errorf("int overflow: value %d exceeds max %d", v, datatype.MaxInt)
	}
	if dt == datatype.Long && v > datatype.MaxLong {
		return fmt.Errorf("long overflow: value %d exceeds max %d", v, datatype.MaxLong)
	}
	return nil
}
