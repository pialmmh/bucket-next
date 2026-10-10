package allocator

import (
	"errors"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"

	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/testutil"
)

func testIdentity(shardID, total int) state.Identity {
	return state.Identity{ShardID: shardID, TotalShards: total, UUIDKeyFingerprint: "fp", SnowflakeEpochMs: 1}
}

func newStore(t *testing.T, shardID, total int) (*state.Store, string) {
	t.Helper()
	path := filepath.Join(testutil.TempDir(t), "s.json")
	st, err := state.Create(path, testIdentity(shardID, total))
	if err != nil {
		t.Fatal(err)
	}
	return st, path
}

func newAlloc(t *testing.T, st *state.Store, shardID, total, segment int, watermark float64) *Allocator {
	t.Helper()
	a := New(Config{ShardID: shardID, TotalShards: total, SegmentSize: segment, Watermark: watermark, Store: st})
	t.Cleanup(a.Close)
	return a
}

func register(t *testing.T, st *state.Store, name string, dt datatype.DataType, startIter int64) {
	t.Helper()
	if _, _, err := st.Register(name, dt, startIter); err != nil {
		t.Fatal(err)
	}
}

func mark(st *state.Store, name string) int64 {
	r, _ := st.Get(name)
	return r.CurrentIteration
}

// ---------- values ----------

func TestNext_shard2of3_interleaved(t *testing.T) {
	st, _ := newStore(t, 2, 3)
	register(t, st, "cdr", datatype.Long, 0)
	a := newAlloc(t, st, 2, 3, 10, 0.9)
	want := []int64{2, 5, 8, 11, 14, 17, 20, 23, 26, 29, 32, 35, 38, 41}
	for i, w := range want {
		first, err := a.Next("cdr", datatype.Long, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Layout().Value(first); got != w {
			t.Errorf("#%d = %d, want %d", i, got, w)
		}
	}
}

func TestNext_shard1of1_sequential(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	register(t, st, "x", datatype.Int, 0)
	a := newAlloc(t, st, 1, 1, 5, 0.9)
	for want := int64(1); want <= 12; want++ {
		first, _ := a.Next("x", datatype.Int, 1)
		if got := a.Layout().Value(first); got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	}
}

func TestNext_errors(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	register(t, st, "x", datatype.Long, 0)
	a := newAlloc(t, st, 1, 1, 5, 0.9)
	if _, err := a.Next("missing", datatype.Long, 1); !errors.Is(err, ErrUnknownEntity) {
		t.Errorf("unknown entity: %v", err)
	}
	if _, err := a.Next("x", datatype.Int, 1); err == nil {
		t.Error("type mismatch must fail")
	}
	if _, err := a.Next("x", datatype.Snowflake, 1); err == nil {
		t.Error("snowflake is not counter-based")
	}
	if _, err := a.Next("x", datatype.Long, 0); err == nil {
		t.Error("count 0 must fail")
	}
}

func TestNext_exhaustedHandsOutNothing(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	// Value = 1 + iter, so iter MaxInt-3 is value MaxInt-2: room for exactly 3 more ints.
	register(t, st, "x", datatype.Int, datatype.MaxInt-3)
	a := newAlloc(t, st, 1, 1, 5, 0.9)
	if _, err := a.Next("x", datatype.Int, 4); !errors.Is(err, ErrExhausted) {
		t.Fatalf("4 more must not fit: %v", err)
	}
	first, err := a.Next("x", datatype.Int, 3)
	if err != nil {
		t.Fatalf("3 more must still fit (nothing was consumed): %v", err)
	}
	if v := a.Layout().Value(first + 2); v != datatype.MaxInt {
		t.Errorf("last value %d, want MaxInt", v)
	}
	if _, err := a.Next("x", datatype.Int, 1); !errors.Is(err, ErrExhausted) {
		t.Errorf("past MaxInt: %v", err)
	}
}

func TestNext_uuid8Capacity(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	register(t, st, "u", datatype.UUID8, datatype.MaxUUID8-1) // value MaxUUID8
	a := newAlloc(t, st, 1, 1, 5, 0.9)
	if _, err := a.Next("u", datatype.UUID8, 1); err != nil {
		t.Fatalf("last uuid8 value must fit: %v", err)
	}
	if _, err := a.Next("u", datatype.UUID8, 1); !errors.Is(err, ErrExhausted) {
		t.Errorf("want ErrExhausted, got %v", err)
	}
}

// ---------- reservations ----------

func TestReservation_oneDiskWritePerSegment(t *testing.T) {
	st, _ := newStore(t, 2, 3)
	register(t, st, "x", datatype.Long, 0)
	a := newAlloc(t, st, 2, 3, 10, 0.5) // refill when <= 5 remain
	a.Next("x", datatype.Long, 1)
	if m := mark(st, "x"); m != 10 {
		t.Fatalf("first call should reserve one segment: mark=%d", m)
	}
	for i := 0; i < 3; i++ {
		a.Next("x", datatype.Long, 1)
	}
	if m := mark(st, "x"); m != 10 {
		t.Errorf("4 IDs out, 6 remain: no refill yet, mark=%d", m)
	}
	a.Next("x", datatype.Long, 1) // 5 remain -> background refill
	a.Close()
	if m := mark(st, "x"); m != 20 {
		t.Errorf("refill should have landed: mark=%d, want 20", m)
	}
}

func TestBatch_contiguousAcrossSegments(t *testing.T) {
	st, _ := newStore(t, 2, 3)
	register(t, st, "x", datatype.Long, 0)
	a := newAlloc(t, st, 2, 3, 10, 0.9)
	first, err := a.Next("x", datatype.Long, 25)
	if err != nil {
		t.Fatal(err)
	}
	if first != 0 {
		t.Errorf("first iteration %d, want 0", first)
	}
	if m := mark(st, "x"); m < 25 {
		t.Errorf("mark %d must cover the 25 handed out", m)
	}
	next, _ := a.Next("x", datatype.Long, 1)
	if next != 25 {
		t.Errorf("after a batch of 25 the next iteration is %d, want 25", next)
	}
}

func TestRestart_forfeitsTailNeverRepeats(t *testing.T) {
	st, path := newStore(t, 2, 3)
	register(t, st, "x", datatype.Long, 0)
	a := New(Config{ShardID: 2, TotalShards: 3, SegmentSize: 10, Watermark: 0.99, Store: st})
	for i := 0; i < 4; i++ {
		a.Next("x", datatype.Long, 1)
	}
	a.Close()
	if m := mark(st, "x"); m != 10 {
		t.Fatalf("pre-crash mark=%d, want 10", m)
	}
	st2, err := state.Open(path, testIdentity(2, 3))
	if err != nil {
		t.Fatal(err)
	}
	a2 := newAlloc(t, st2, 2, 3, 10, 0.99)
	first, _ := a2.Next("x", datatype.Long, 1)
	if first != 10 || a2.Layout().Value(first) != 32 {
		t.Errorf("after restart: iter %d value %d, want iter 10 value 32", first, a2.Layout().Value(first))
	}
}

// ---------- the bug this rewrite fixes ----------

// TestConcurrent_everyIterationExactlyOnce hammers one entity from many goroutines with tiny
// segments, so background refills overlap inline reservations constantly. Some goroutines
// also reset the counter forward. Every iteration handed out must be unique.
func TestConcurrent_everyIterationExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		segment   int
		watermark float64
		resets    bool
	}{{7, 0.5, false}, {1, 0.9, false}, {10, 0.5, true}, {1000, 0.9, false}} {
		st, _ := newStore(t, 1, 1)
		register(t, st, "x", datatype.Long, 0)
		a := New(Config{ShardID: 1, TotalShards: 1, SegmentSize: tc.segment, Watermark: tc.watermark, Store: st})

		var mu sync.Mutex
		seen := map[int64]bool{}
		var dup int64 = -1
		var wg sync.WaitGroup
		for g := 0; g < 32; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(g)))
				for i := 0; i < 400; i++ {
					if tc.resets && g == 0 && i%50 == 0 {
						snap, _ := a.Snapshot("x")
						a.Reset("x", snap.NextIteration+int64(rng.Intn(30)), false)
						continue
					}
					n := int64(1 + rng.Intn(40))
					first, err := a.Next("x", datatype.Long, n)
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					for it := first; it < first+n; it++ {
						if seen[it] {
							dup = it
						}
						seen[it] = true
					}
					mu.Unlock()
				}
			}(g)
		}
		wg.Wait()
		a.Close()
		if dup >= 0 {
			t.Fatalf("segment=%d watermark=%v resets=%v: iteration %d issued twice",
				tc.segment, tc.watermark, tc.resets, dup)
		}
		var maxIt int64
		for it := range seen {
			if it > maxIt {
				maxIt = it
			}
		}
		if m := mark(st, "x"); m <= maxIt {
			t.Fatalf("durable mark %d must be above every issued iteration (max %d)", m, maxIt)
		}
		if !tc.resets && int64(len(seen)) != maxIt+1 {
			t.Errorf("segment=%d: %d issued but max is %d — gaps without a restart", tc.segment, len(seen), maxIt)
		}
	}
}

func TestConcurrent_entitiesAreIndependent(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	names := []string{"a", "b", "c", "d"}
	for _, n := range names {
		register(t, st, n, datatype.Long, 0)
	}
	a := newAlloc(t, st, 1, 1, 5, 0.5)
	var wg sync.WaitGroup
	counts := make([]int64, len(names))
	for i, n := range names {
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(i int, n string) {
				defer wg.Done()
				for k := 0; k < 250; k++ {
					a.Next(n, datatype.Long, 1)
				}
			}(i, n)
		}
		counts[i] = 1000
	}
	wg.Wait()
	for i, n := range names {
		snap, _ := a.Snapshot(n)
		if snap.NextIteration != counts[i] {
			t.Errorf("%s: next iteration %d, want %d", n, snap.NextIteration, counts[i])
		}
	}
}

// ---------- reset ----------

func TestReset_forwardBackwardForce(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	register(t, st, "x", datatype.Long, 0)
	a := newAlloc(t, st, 1, 1, 10, 0.9)
	for i := 0; i < 5; i++ {
		a.Next("x", datatype.Long, 1) // iterations 0..4 issued
	}
	if _, err := a.Reset("x", 3, false); !errors.Is(err, ErrBackward) {
		t.Fatalf("backward reset must be refused: %v", err)
	}
	if prev, err := a.Reset("x", 7, false); err != nil || prev != 5 {
		t.Fatalf("forward reset inside the window: prev=%d err=%v", prev, err)
	}
	if first, _ := a.Next("x", datatype.Long, 1); first != 7 {
		t.Errorf("after reset to 7: %d", first)
	}
	if _, err := a.Reset("x", 2, true); err != nil {
		t.Fatalf("forced backward reset: %v", err)
	}
	if first, _ := a.Next("x", datatype.Long, 1); first != 2 {
		t.Errorf("after forced reset to 2: %d", first)
	}
}

func TestReset_floorIsDiskMarkWhenNotLoaded(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	register(t, st, "x", datatype.Long, 50) // an earlier process may have issued up to 49
	a := newAlloc(t, st, 1, 1, 10, 0.9)
	if _, err := a.Reset("x", 40, false); !errors.Is(err, ErrBackward) {
		t.Fatalf("reset below the disk mark must be refused: %v", err)
	}
	if _, err := a.Reset("x", 60, false); err != nil {
		t.Fatal(err)
	}
	if m := mark(st, "x"); m != 60 {
		t.Errorf("mark=%d, want 60", m)
	}
}

// ---------- snapshot ----------

func TestSnapshot(t *testing.T) {
	st, _ := newStore(t, 1, 1)
	register(t, st, "x", datatype.Long, 0)
	a := newAlloc(t, st, 1, 1, 10, 0.5)
	snap, err := a.Snapshot("x")
	if err != nil || snap.Loaded || snap.NextIteration != 0 {
		t.Fatalf("before use: %+v %v", snap, err)
	}
	a.Next("x", datatype.Long, 3)
	snap, _ = a.Snapshot("x")
	if !snap.Loaded || snap.Issued != 3 || snap.NextIteration != 3 || snap.Remaining != 7 || snap.DiskMark != 10 {
		t.Errorf("after 3: %+v", snap)
	}
	if _, err := a.Snapshot("missing"); !errors.Is(err, ErrUnknownEntity) {
		t.Errorf("missing: %v", err)
	}
}
