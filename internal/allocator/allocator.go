// Package allocator hands out counter iterations (for int, long and uuid* entities) from
// in-RAM reservations backed by the state file.
//
// Per entity it keeps a window [cursor, reservedEnd): those iterations are durably reserved
// on disk and owned by this process alone. Requests take from the window without touching
// disk. When the window runs low a background refill extends it; when a request needs more
// than the window holds it extends it inline.
//
// Exactly one reservation per entity can be in flight. The background refill holds a
// "reserving" token, and inline reservations and resets wait for it to finish. Every
// reservation starts at the in-memory reservedEnd, which equals the durable mark. So no two
// reservations can cover the same iterations, and the same ID can never be issued twice —
// even under heavy concurrent load.
package allocator

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sync"

	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/shardmath"
	"github.com/telcobright/bucket-next/internal/state"
)

var (
	// ErrUnknownEntity: the entity is not in the state store.
	ErrUnknownEntity = errors.New("entity not registered")
	// ErrExhausted: the entity's type has no values left on this shard.
	ErrExhausted = errors.New("no values left for this entity's type on this shard")
	// ErrBackward: a reset would move the counter below IDs that may already be issued.
	ErrBackward = errors.New("reset would move the counter backward and re-issue IDs")
)

type Config struct {
	ShardID     int
	TotalShards int
	SegmentSize int
	Watermark   float64
	Store       *state.Store
}

type Allocator struct {
	layout      shardmath.Layout
	segmentSize int64
	threshold   int64 // start a background refill when this many or fewer iterations remain
	store       *state.Store

	mu       sync.Mutex // guards entities
	entities map[string]*entity
	wg       sync.WaitGroup // in-flight background refills
}

type entity struct {
	mu   sync.Mutex
	cond *sync.Cond // broadcast when a background refill finishes

	loaded      bool
	dataType    datatype.DataType
	loadMark    int64 // durable mark when this process loaded the entity
	cursor      int64 // next iteration to hand out
	reservedEnd int64 // exclusive end of the window owned by this process
	reserving   bool  // a background refill holds the reservation token
}

func New(cfg Config) *Allocator {
	threshold := int64(math.Ceil((1 - cfg.Watermark) * float64(cfg.SegmentSize)))
	return &Allocator{
		layout:      shardmath.New(cfg.ShardID, cfg.TotalShards),
		segmentSize: int64(cfg.SegmentSize),
		threshold:   threshold,
		store:       cfg.Store,
		entities:    make(map[string]*entity),
	}
}

func (a *Allocator) Layout() shardmath.Layout { return a.layout }

// Close blocks until all background refills have finished.
func (a *Allocator) Close() { a.wg.Wait() }

// Next hands out n consecutive iterations for the entity and returns the first one. The
// caller turns iteration i into a value with Layout().Value(i). If the entity's type cannot
// hold all n values, Next fails with ErrExhausted and hands out nothing.
func (a *Allocator) Next(name string, dt datatype.DataType, n int64) (int64, error) {
	if n < 1 {
		return 0, fmt.Errorf("count must be >= 1, got %d", n)
	}
	if !dt.IsCounter() {
		return 0, fmt.Errorf("type %s is not counter-based", dt)
	}
	e := a.entityFor(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := a.loadLocked(e, name); err != nil {
		return 0, err
	}
	if e.dataType != dt {
		return 0, fmt.Errorf("entity %q is registered as %s, not %s", name, e.dataType, dt)
	}
	if maxIter := a.layout.MaxIter(dt.MaxValue()); e.cursor+n-1 > maxIter {
		return 0, ErrExhausted
	}
	for e.reservedEnd-e.cursor < n {
		if e.reserving {
			e.cond.Wait() // a background refill is extending the window; take turns
			continue
		}
		short := n - (e.reservedEnd - e.cursor)
		grow := (short + a.segmentSize - 1) / a.segmentSize * a.segmentSize
		if err := a.store.RaiseMark(name, e.reservedEnd+grow); err != nil {
			return 0, fmt.Errorf("reserve for %q: %w", name, err)
		}
		e.reservedEnd += grow
	}
	first := e.cursor
	e.cursor += n
	a.maybeRefillLocked(e, name)
	return first, nil
}

// Reset moves the entity's next iteration to target. Moving below anything this or an
// earlier process may have issued requires force. It returns the next iteration before the
// reset.
func (a *Allocator) Reset(name string, target int64, force bool) (int64, error) {
	if target < 0 {
		return 0, fmt.Errorf("target iteration must be >= 0, got %d", target)
	}
	e := a.entityFor(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := a.loadLocked(e, name); err != nil {
		return 0, err
	}
	for e.reserving {
		e.cond.Wait()
	}
	prev := e.cursor
	if target < e.cursor && !force {
		return prev, ErrBackward
	}
	// Iterations in [cursor, reservedEnd) were reserved but never handed out, so lowering the
	// durable mark to a forward target is safe.
	if err := a.store.SetMark(name, target); err != nil {
		return prev, err
	}
	e.cursor, e.reservedEnd = target, target
	if target < e.loadMark {
		e.loadMark = target
	}
	return prev, nil
}

// Snapshot is a diagnostic view of one entity's window.
type Snapshot struct {
	Loaded         bool  `json:"loaded"`
	Issued         int64 `json:"issued"`        // handed out by this process since it loaded the entity
	NextIteration  int64 `json:"nextIteration"` // next iteration this process will hand out
	ReservedEnd    int64 `json:"reservedEnd"`
	Remaining      int64 `json:"remaining"` // reserved in RAM, not yet handed out
	RefillInFlight bool  `json:"refillInFlight"`
	DiskMark       int64 `json:"diskMark"`
}

func (a *Allocator) Snapshot(name string) (Snapshot, error) {
	rec, ok := a.store.Get(name)
	if !ok {
		return Snapshot{}, ErrUnknownEntity
	}
	a.mu.Lock()
	e, ok := a.entities[name]
	a.mu.Unlock()
	if !ok {
		return Snapshot{NextIteration: rec.CurrentIteration, ReservedEnd: rec.CurrentIteration, DiskMark: rec.CurrentIteration}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.loaded {
		return Snapshot{NextIteration: rec.CurrentIteration, ReservedEnd: rec.CurrentIteration, DiskMark: rec.CurrentIteration}, nil
	}
	return Snapshot{
		Loaded:         true,
		Issued:         e.cursor - e.loadMark,
		NextIteration:  e.cursor,
		ReservedEnd:    e.reservedEnd,
		Remaining:      e.reservedEnd - e.cursor,
		RefillInFlight: e.reserving,
		DiskMark:       rec.CurrentIteration,
	}, nil
}

// ----- internals -----

func (a *Allocator) entityFor(name string) *entity {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entities[name]
	if !ok {
		e = &entity{}
		e.cond = sync.NewCond(&e.mu)
		a.entities[name] = e
	}
	return e
}

func (a *Allocator) loadLocked(e *entity, name string) error {
	if e.loaded {
		return nil
	}
	rec, ok := a.store.Get(name)
	if !ok {
		return ErrUnknownEntity
	}
	e.dataType = rec.DataType
	e.loadMark = rec.CurrentIteration
	e.cursor = rec.CurrentIteration
	e.reservedEnd = rec.CurrentIteration
	e.loaded = true
	return nil
}

// maybeRefillLocked starts a background refill when the window runs low. Caller holds e.mu.
func (a *Allocator) maybeRefillLocked(e *entity, name string) {
	if e.reserving || e.reservedEnd-e.cursor > a.threshold {
		return
	}
	e.reserving = true
	from := e.reservedEnd
	a.wg.Add(1)
	go a.refill(e, name, from)
}

// refill extends the window by one segment. It writes the disk without holding e.mu so
// requests keep being served; nobody else can move reservedEnd while e.reserving is set.
func (a *Allocator) refill(e *entity, name string, from int64) {
	defer a.wg.Done()
	to := from + a.segmentSize
	err := a.store.RaiseMark(name, to)
	e.mu.Lock()
	if err == nil {
		e.reservedEnd = to
	}
	e.reserving = false
	e.cond.Broadcast()
	e.mu.Unlock()
	if err != nil {
		log.Printf("allocator: background refill for %q failed (requests will retry inline): %v", name, err)
	}
}
