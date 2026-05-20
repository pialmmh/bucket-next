package allocator

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/state"
)

func mustStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func registerNumeric(t *testing.T, st *state.Store, name string, dt datatype.DataType, shardID int) {
	t.Helper()
	if err := st.Put(state.Record{EntityName: name, DataType: dt, CurrentIteration: 0, ShardID: shardID}); err != nil {
		t.Fatal(err)
	}
}

// newAlloc is a test helper that auto-closes the allocator (waits for refill goroutines)
// when the test finishes — preventing temp-state-file races in t.TempDir cleanup.
func newAlloc(t *testing.T, cfg Config) *Allocator {
	t.Helper()
	a := New(cfg)
	t.Cleanup(a.Close)
	return a
}

// ---------- math helpers ----------

func TestComputeValue_inverse(t *testing.T) {
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 100, Watermark: 0.9, Store: mustStore(t)})
	for iter := int64(0); iter < 1000; iter++ {
		v := a.ComputeValue(iter)
		back := a.IterFromValue(v)
		if back != iter {
			t.Errorf("iter %d: ComputeValue=%d, IterFromValue=%d", iter, v, back)
		}
	}
}

func TestSnapForward_shard2of3(t *testing.T) {
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 100, Watermark: 0.9, Store: mustStore(t)})
	cases := map[int64]int64{
		0:    2,    // residue 0 → snap to 2
		1:    2,    // residue 1 → snap to 2
		2:    2,    // residue 2 → already valid
		3:    5,    // residue 0 → snap to 5
		1000: 1001, // residue 1 → snap to 1001
		1001: 1001, // already valid
		5000: 5000, // residue 2 → already valid
	}
	for in, want := range cases {
		if got := a.SnapForward(in); got != want {
			t.Errorf("SnapForward(%d)=%d, want %d", in, got, want)
		}
	}
}

func TestSnapForward_shard1of3(t *testing.T) {
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 3, SegmentSize: 100, Watermark: 0.9, Store: mustStore(t)})
	cases := map[int64]int64{
		0:    1, // residue 0 → snap to 1
		1:    1, // already valid
		2:    4, // residue 2 → snap to 4
		4:    4, // already valid
		7:    7,
		5000: 5002, // 5000%3=2 → 5002 (residue 1)
	}
	for in, want := range cases {
		if got := a.SnapForward(in); got != want {
			t.Errorf("SnapForward(%d)=%d, want %d", in, got, want)
		}
	}
}

// ---------- NextID core ----------

func TestNextID_shard2of3_emitsInterleavedPattern(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "cdr", datatype.Long, 2)
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 10, Watermark: 0.9, Store: st})

	want := []int64{2, 5, 8, 11, 14, 17, 20, 23, 26, 29, 32, 35, 38, 41}
	for i, w := range want {
		got, err := a.NextID("cdr", datatype.Long)
		if err != nil {
			t.Fatalf("NextID #%d: %v", i, err)
		}
		if got != w {
			t.Errorf("NextID #%d = %d, want %d", i, got, w)
		}
	}
}

func TestNextID_shard1of1_emits1to12(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Int, 1)
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 5, Watermark: 0.9, Store: st})

	for i := int64(1); i <= 12; i++ {
		got, err := a.NextID("x", datatype.Int)
		if err != nil {
			t.Fatal(err)
		}
		if got != i {
			t.Errorf("NextID #%d = %d, want %d", i, got, i)
		}
	}
}

func TestNextID_shard3of3_emitsThirdResidue(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 3)
	a := newAlloc(t, Config{ShardID: 3, TotalShards: 3, SegmentSize: 5, Watermark: 0.9, Store: st})

	want := []int64{3, 6, 9, 12, 15, 18, 21}
	for i, w := range want {
		got, _ := a.NextID("x", datatype.Long)
		if got != w {
			t.Errorf("NextID #%d = %d, want %d", i, got, w)
		}
	}
}

// ---------- error paths ----------

func TestNextID_typeMismatch(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 1)
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 5, Watermark: 0.9, Store: st})
	if _, err := a.NextID("x", datatype.Int); err == nil {
		t.Error("expected type-mismatch error")
	}
}

func TestNextID_unknownEntity(t *testing.T) {
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 5, Watermark: 0.9, Store: mustStore(t)})
	if _, err := a.NextID("missing", datatype.Long); err == nil {
		t.Error("expected error for unregistered entity")
	}
}

func TestNextID_intOverflow(t *testing.T) {
	st := mustStore(t)
	// Single shard, start near the int max.
	st.Put(state.Record{EntityName: "x", DataType: datatype.Int, CurrentIteration: datatype.MaxInt - 2, ShardID: 1})
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 5, Watermark: 0.9, Store: st})

	// First two should still fit, then overflow on the third.
	_, err1 := a.NextID("x", datatype.Int)
	_, err2 := a.NextID("x", datatype.Int)
	_, err3 := a.NextID("x", datatype.Int)
	if err1 != nil || err2 != nil {
		t.Fatalf("first two should succeed: %v %v", err1, err2)
	}
	if err3 == nil || !strings.Contains(err3.Error(), "overflow") {
		t.Errorf("expected overflow on third, got %v", err3)
	}
}

// ---------- segment caching ----------

func TestSegmentCaching_oneDiskWritePerSegment(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 2)
	// Watermark 0.99 means refill won't trigger until segment is nearly drained.
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 5, Watermark: 0.99, Store: st})

	// First emission reserves segment [0, 5) on disk → CurrentIteration=5.
	a.NextID("x", datatype.Long)
	rec, _ := st.Get("x")
	if rec.CurrentIteration != 5 {
		t.Errorf("after 1 emit, disk=%d, want 5", rec.CurrentIteration)
	}

	// Three more emissions still within segment — disk unchanged.
	for i := 0; i < 3; i++ {
		a.NextID("x", datatype.Long)
	}
	rec, _ = st.Get("x")
	if rec.CurrentIteration != 5 {
		t.Errorf("after 4 emits, disk=%d, want 5 (still one segment)", rec.CurrentIteration)
	}
}

func TestSegmentRefill_advancesDiskInBackground(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 1)
	// Watermark 0.5 means refill at iteration 5 of 10.
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 10, Watermark: 0.5, Store: st})

	// 5 emissions: still under watermark.
	for i := 0; i < 5; i++ {
		a.NextID("x", datatype.Long)
	}
	// 6th crosses 0.5 → triggers async refill.
	a.NextID("x", datatype.Long)

	// Wait up to 500 ms for the background worker to finish.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rec, _ := st.Get("x")
		if rec.CurrentIteration >= 20 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec, _ := st.Get("x")
	t.Errorf("refill never landed; disk=%d, want >= 20", rec.CurrentIteration)
}

// ---------- batch ----------

func TestNextBatch_smallFitsInSegment(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 2)
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 100, Watermark: 0.9, Store: st})

	batch, err := a.NextBatch("x", datatype.Long, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{2, 5, 8, 11, 14}
	if len(batch) != 5 {
		t.Fatalf("len=%d, want 5", len(batch))
	}
	for i, v := range batch {
		if v != want[i] {
			t.Errorf("batch[%d]=%d, want %d", i, v, want[i])
		}
	}
}

func TestNextBatch_largeReservesSegmentsInline(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 2)
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 10, Watermark: 0.9, Store: st})

	batch, err := a.NextBatch("x", datatype.Long, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 25 {
		t.Errorf("len=%d, want 25", len(batch))
	}
	// Pattern check: 2, 5, 8, …
	for i, v := range batch {
		want := int64(2 + i*3)
		if v != want {
			t.Errorf("batch[%d]=%d, want %d", i, v, want)
			break
		}
	}
	// Disk must have advanced to cover at least the 25 emitted iterations.
	rec, _ := st.Get("x")
	if rec.CurrentIteration < 25 {
		t.Errorf("disk=%d, want >= 25", rec.CurrentIteration)
	}
}

func TestNextBatch_followedByNextID_continues(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 1)
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 10, Watermark: 0.9, Store: st})

	batch, _ := a.NextBatch("x", datatype.Long, 5)
	if batch[4] != 5 {
		t.Errorf("batch end=%d, want 5", batch[4])
	}
	next, _ := a.NextID("x", datatype.Long)
	if next != 6 {
		t.Errorf("after batch, NextID=%d, want 6", next)
	}
}

// ---------- restart safety ----------

func TestRestartSafety_resumesFromDisk_withForfeitedTail(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "s.json")
	// First lifetime: emit 4 IDs in a 10-size segment, then "crash".
	{
		st, _ := state.Open(statePath)
		st.Put(state.Record{EntityName: "x", DataType: datatype.Long, CurrentIteration: 0, ShardID: 2})
		a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 10, Watermark: 0.99, Store: st})
		for i := 0; i < 4; i++ {
			a.NextID("x", datatype.Long)
		}
		// Disk should be at 10 (one segment reserved); RAM cursor at 4.
		rec, _ := st.Get("x")
		if rec.CurrentIteration != 10 {
			t.Fatalf("pre-crash disk=%d, want 10", rec.CurrentIteration)
		}
	}
	// Second lifetime: fresh process. Disk says 10 → next emission must be iter 10 → value 32.
	st2, _ := state.Open(statePath)
	a2 := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 10, Watermark: 0.99, Store: st2})
	got, err := a2.NextID("x", datatype.Long)
	if err != nil {
		t.Fatal(err)
	}
	if got != 32 {
		t.Errorf("post-restart NextID=%d, want 32 (iter 10, 5 iters forfeited)", got)
	}
}

// ---------- introspection ----------

func TestCurrentValueAndNextValue(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 2)
	a := newAlloc(t, Config{ShardID: 2, TotalShards: 3, SegmentSize: 10, Watermark: 0.9, Store: st})

	// Before any emission.
	if _, ok := a.CurrentValue("x"); ok {
		t.Error("CurrentValue should be absent before first emission")
	}
	if v, ok := a.NextValue("x"); !ok || v != 2 {
		t.Errorf("NextValue pre-emit = (%d, %v), want (2, true)", v, ok)
	}

	a.NextID("x", datatype.Long)
	if v, ok := a.CurrentValue("x"); !ok || v != 2 {
		t.Errorf("CurrentValue after 1 emit = (%d, %v), want (2, true)", v, ok)
	}
	if v, _ := a.NextValue("x"); v != 5 {
		t.Errorf("NextValue after 1 emit = %d, want 5", v)
	}
}

func TestSnapshot(t *testing.T) {
	st := mustStore(t)
	registerNumeric(t, st, "x", datatype.Long, 1)
	a := newAlloc(t, Config{ShardID: 1, TotalShards: 1, SegmentSize: 10, Watermark: 0.9, Store: st})

	// No in-RAM segment yet.
	if _, err := a.Snapshot("x"); err == nil {
		t.Error("Snapshot before emission should error")
	}
	a.NextID("x", datatype.Long)
	snap, err := a.Snapshot("x")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Size != 10 {
		t.Errorf("Size=%d, want 10", snap.Size)
	}
	if snap.Cursor != 1 {
		t.Errorf("Cursor=%d, want 1", snap.Cursor)
	}
	if snap.Remaining != 9 {
		t.Errorf("Remaining=%d, want 9", snap.Remaining)
	}
}

// ---------- overflow guard ----------

func TestCheckOverflow(t *testing.T) {
	if err := CheckOverflow(datatype.Int, datatype.MaxInt); err != nil {
		t.Errorf("MaxInt itself should not overflow: %v", err)
	}
	if err := CheckOverflow(datatype.Int, datatype.MaxInt+1); err == nil {
		t.Error("MaxInt+1 should overflow int")
	}
	if err := CheckOverflow(datatype.Long, datatype.MaxLong); err != nil {
		t.Errorf("MaxLong itself should not overflow: %v", err)
	}
	// snowflake / uuid never overflow via this function.
	if err := CheckOverflow(datatype.Snowflake, datatype.MaxLong); err != nil {
		t.Errorf("snowflake should not be subject to overflow checks: %v", err)
	}
}
