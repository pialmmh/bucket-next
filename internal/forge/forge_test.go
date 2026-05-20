package forge

import (
	"strconv"
	"testing"
)

const testEpoch = 1704067200000

func TestNew_validatesShardID(t *testing.T) {
	bad := []int{0, -1, 1025, 2000}
	for _, sid := range bad {
		if _, err := New(sid, testEpoch, 0); err == nil {
			t.Errorf("New(shardID=%d) should fail", sid)
		}
	}
	good := []int{1, 2, 512, 1023, 1024}
	for _, sid := range good {
		if _, err := New(sid, testEpoch, 0); err != nil {
			t.Errorf("New(shardID=%d): %v", sid, err)
		}
	}
}

func TestGenerate_uniqueness(t *testing.T) {
	f, _ := New(7, testEpoch, 0)
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id, err := f.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate at i=%d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
	gen, _, _ := f.Stats()
	if gen != 10000 {
		t.Errorf("stats.generated = %d, want 10000", gen)
	}
}

func TestParse_roundTrip(t *testing.T) {
	for _, sid := range []int{1, 2, 100, 512, 1024} {
		f, _ := New(sid, testEpoch, 0)
		id, _ := f.Generate()
		p, err := f.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		if p.ShardID != sid {
			t.Errorf("shard %d -> decoded as %d", sid, p.ShardID)
		}
		if p.Sequence < 0 || p.Sequence > 4095 {
			t.Errorf("sequence out of range: %d", p.Sequence)
		}
		if p.Timestamp < testEpoch {
			t.Errorf("timestamp %d < epoch %d", p.Timestamp, testEpoch)
		}
		if len(p.Binary) != 64 {
			t.Errorf("binary len=%d, want 64", len(p.Binary))
		}
		// The returned ID string must round-trip into the same int64.
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			t.Errorf("id %s not a positive 64-bit int", id)
		}
		if p.ID != id {
			t.Errorf("Parsed.ID = %s, want %s", p.ID, id)
		}
	}
}

func TestParse_invalidInput(t *testing.T) {
	f, _ := New(1, testEpoch, 0)
	for _, s := range []string{"not-a-number", "", "abc"} {
		if _, err := f.Parse(s); err == nil {
			t.Errorf("Parse(%q) should error", s)
		}
	}
}

func TestGenerateBatch_uniquenessAcrossMs(t *testing.T) {
	// 8192 > 4096 → must span at least two milliseconds.
	f, _ := New(5, testEpoch, 0)
	batch, err := f.GenerateBatch(8192)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 8192 {
		t.Errorf("batch len=%d", len(batch))
	}
	seen := make(map[string]struct{}, len(batch))
	for _, id := range batch {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate in batch: %s", id)
		}
		seen[id] = struct{}{}
	}
	// First and last must both decode to shard 5.
	first, _ := f.Parse(batch[0])
	last, _ := f.Parse(batch[len(batch)-1])
	if first.ShardID != 5 || last.ShardID != 5 {
		t.Errorf("shard mismatch in batch: first=%d last=%d", first.ShardID, last.ShardID)
	}
	// last.timestamp must be >= first.timestamp + 1ms because we crossed at least one ms boundary.
	if last.Timestamp < first.Timestamp {
		t.Errorf("timestamp went backwards in batch: %d -> %d", first.Timestamp, last.Timestamp)
	}
}

func TestInfo(t *testing.T) {
	f, _ := New(2, testEpoch, 0)
	info := f.Info(3)
	if info.ShardID != 2 || info.TotalShards != 3 {
		t.Errorf("info shard=%d total=%d, want 2/3", info.ShardID, info.TotalShards)
	}
	if info.MaxIdsPerMs != 4096 {
		t.Errorf("MaxIdsPerMs=%d", info.MaxIdsPerMs)
	}
	if info.MaxShards != 1024 {
		t.Errorf("MaxShards=%d", info.MaxShards)
	}
	for k, want := range map[string]int{"timestamp": 41, "shardId": 10, "sequence": 12, "total": 64} {
		if info.BitsAllocation[k] != want {
			t.Errorf("BitsAllocation[%s]=%d, want %d", k, info.BitsAllocation[k], want)
		}
	}
}

func TestStats_increment(t *testing.T) {
	f, _ := New(1, testEpoch, 0)
	for i := 0; i < 100; i++ {
		f.Generate()
	}
	gen, _, _ := f.Stats()
	if gen != 100 {
		t.Errorf("generated=%d, want 100", gen)
	}
}
