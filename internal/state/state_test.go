package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/telcobright/bucket-next/internal/datatype"
)

var testID = Identity{ShardID: 2, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.json")
	s, err := Create(path, testID)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestOpen_missingFileIsRefused(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent.json"), testID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestOpen_emptyFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(path, []byte("  \n"), 0o600)
	if _, err := Open(path, testID); err == nil {
		t.Fatal("empty file must be refused")
	}
}

func TestCreate_refusesExistingFile(t *testing.T) {
	_, path := newStore(t)
	if _, err := Create(path, testID); err == nil {
		t.Fatal("Create over an existing file must fail")
	}
}

func TestOpen_identityMismatchIsRefused(t *testing.T) {
	_, path := newStore(t)
	cases := map[string]Identity{
		"shard_id":           {ShardID: 1, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000},
		"total_shards":       {ShardID: 2, TotalShards: 4, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000},
		"uuid_key":           {ShardID: 2, TotalShards: 3, UUIDKeyFingerprint: "other", SnowflakeEpochMs: 1704067200000},
		"snowflake_epoch_ms": {ShardID: 2, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1},
	}
	for field, id := range cases {
		_, err := Open(path, id)
		var ie *IdentityError
		if !errors.As(err, &ie) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: want IdentityError mentioning it, got %v", field, err)
		}
	}
	if _, err := Open(path, testID); err != nil {
		t.Fatalf("matching identity must open: %v", err)
	}
}

func TestRegister_createsOnceAndPersists(t *testing.T) {
	s, path := newStore(t)
	r, created, err := s.Register("cdr", datatype.Long, 7)
	if err != nil || !created || r.CurrentIteration != 7 || r.ShardID != 2 {
		t.Fatalf("first Register: %+v created=%v err=%v", r, created, err)
	}
	r, created, _ = s.Register("cdr", datatype.Int, 99)
	if created || r.DataType != datatype.Long || r.CurrentIteration != 7 {
		t.Fatalf("second Register must return the existing record: %+v created=%v", r, created)
	}
	s2, err := Open(path, testID)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get("cdr"); !ok || got.CurrentIteration != 7 {
		t.Errorf("after reopen: %+v %v", got, ok)
	}
}

func TestRegister_concurrentFirstCallsOneWinner(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	wins := make(chan datatype.DataType, 20)
	for i := 0; i < 20; i++ {
		dt := datatype.Long
		if i%2 == 0 {
			dt = datatype.Int
		}
		wg.Add(1)
		go func(dt datatype.DataType) {
			defer wg.Done()
			if _, created, _ := s.Register("x", dt, 0); created {
				wins <- dt
			}
		}(dt)
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d winners, want 1", n)
	}
}

func TestRaiseMark_onlyMovesUp(t *testing.T) {
	s, _ := newStore(t)
	s.Register("x", datatype.Long, 0)
	s.RaiseMark("x", 100)
	s.RaiseMark("x", 50)
	if r, _ := s.Get("x"); r.CurrentIteration != 100 {
		t.Errorf("mark=%d, want 100", r.CurrentIteration)
	}
	s.SetMark("x", 10)
	if r, _ := s.Get("x"); r.CurrentIteration != 10 {
		t.Errorf("SetMark: mark=%d, want 10", r.CurrentIteration)
	}
	if err := s.RaiseMark("missing", 1); err == nil {
		t.Error("RaiseMark on missing entity must fail")
	}
}

func TestSnowflakeMark_persists(t *testing.T) {
	s, path := newStore(t)
	s.RaiseSnowflakeMark(5000)
	s.RaiseSnowflakeMark(4000)
	s2, _ := Open(path, testID)
	if got := s2.SnowflakeMark(); got != 5000 {
		t.Errorf("mark=%d, want 5000", got)
	}
}

func TestDeleteAndClear(t *testing.T) {
	s, _ := newStore(t)
	s.Register("a", datatype.Int, 0)
	s.Register("b", datatype.Long, 0)
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("a"); ok {
		t.Error("a still present")
	}
	if err := s.Delete("a"); err == nil {
		t.Error("deleting a missing entity must fail")
	}
	s.Clear()
	if n := len(s.All()); n != 0 {
		t.Errorf("after Clear: %d records", n)
	}
}

func TestAll_sorted(t *testing.T) {
	s, _ := newStore(t)
	for _, n := range []string{"c", "a", "b"} {
		s.Register(n, datatype.Int, 0)
	}
	all := s.All()
	if all[0].EntityName != "a" || all[2].EntityName != "c" {
		t.Errorf("not sorted: %+v", all)
	}
}

func TestOpen_migratesV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	v1 := `[{"entityName":"cdr","dataType":"long","currentIteration":40,"shardId":2}]`
	os.WriteFile(path, []byte(v1), 0o600)
	s, err := Open(path, testID)
	if err != nil {
		t.Fatal(err)
	}
	if !s.MigratedFromV1() {
		t.Error("MigratedFromV1 = false")
	}
	if r, _ := s.Get("cdr"); r.CurrentIteration != 40 {
		t.Errorf("migrated mark=%d", r.CurrentIteration)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"version": 2`) {
		t.Error("file not rewritten as version 2")
	}
	if _, err := Open(path, testID); err != nil {
		t.Errorf("reopen after migration: %v", err)
	}
}

func TestOpen_v1FromAnotherShardIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(path, []byte(`[{"entityName":"cdr","dataType":"long","currentIteration":4,"shardId":1}]`), 0o600)
	var ie *IdentityError
	if _, err := Open(path, testID); !errors.As(err, &ie) {
		t.Fatalf("want IdentityError, got %v", err)
	}
}

func TestCreate_makesParentDirAndLeavesNoTempFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep")
	s, err := Create(filepath.Join(dir, "s.json"), testID)
	if err != nil {
		t.Fatal(err)
	}
	s.Register("x", datatype.Int, 0)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("want only s.json, got %d entries", len(entries))
	}
}
