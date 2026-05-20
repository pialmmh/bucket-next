package state

import (
	"path/filepath"
	"testing"

	"github.com/telcobright/bucket-next/internal/datatype"
)

func TestOpen_missingFileIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.All(); len(got) != 0 {
		t.Errorf("len(All)=%d, want 0", len(got))
	}
}

func TestPutGetDelete(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	r := Record{EntityName: "cdr", DataType: datatype.Long, CurrentIteration: 42, ShardID: 2}
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("cdr")
	if !ok {
		t.Fatal("Get returned not-ok after Put")
	}
	if got.CurrentIteration != 42 || got.DataType != datatype.Long {
		t.Errorf("Get mismatch: %+v", got)
	}
	if _, ok := s.Get("missing"); ok {
		t.Error("Get of missing should be not-ok")
	}
	if err := s.Delete("cdr"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("cdr"); ok {
		t.Error("Get after Delete should be not-ok")
	}
}

func TestPersistence_acrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s, _ := Open(path)
	if err := s.Put(Record{EntityName: "a", DataType: datatype.Int, CurrentIteration: 7, ShardID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Record{EntityName: "b", DataType: datatype.Long, CurrentIteration: 99, ShardID: 1}); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.All()) != 2 {
		t.Errorf("expected 2 records after reopen, got %d", len(s2.All()))
	}
	a, _ := s2.Get("a")
	b, _ := s2.Get("b")
	if a.CurrentIteration != 7 || a.DataType != datatype.Int {
		t.Errorf("a mismatch: %+v", a)
	}
	if b.CurrentIteration != 99 || b.DataType != datatype.Long {
		t.Errorf("b mismatch: %+v", b)
	}
}

func TestClear(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	s.Put(Record{EntityName: "x", DataType: datatype.Int})
	s.Put(Record{EntityName: "y", DataType: datatype.Long})
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := s.All(); len(got) != 0 {
		t.Errorf("after Clear, len=%d", len(got))
	}
}

func TestGet_returnsIndependentCopy(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	s.Put(Record{EntityName: "x", DataType: datatype.Int, CurrentIteration: 1})
	r1, _ := s.Get("x")
	r1.CurrentIteration = 999 // mutate caller's copy
	r2, _ := s.Get("x")
	if r2.CurrentIteration != 1 {
		t.Errorf("Get returned mutable shared state; got %d, want 1", r2.CurrentIteration)
	}
}

func TestAtomicRename_createsParentDir(t *testing.T) {
	// state_path under a directory that doesn't exist yet
	dir := filepath.Join(t.TempDir(), "nested", "deep", "dir")
	path := filepath.Join(dir, "s.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Record{EntityName: "x", DataType: datatype.Int, CurrentIteration: 5}); err != nil {
		t.Fatalf("Put failed (parent dir should be created): %v", err)
	}
}
