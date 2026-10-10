package shardmath

import (
	"math"
	"testing"
)

func TestValueIterInverse(t *testing.T) {
	for _, l := range []Layout{New(1, 1), New(2, 3), New(3, 3), New(1024, 1024)} {
		for iter := int64(0); iter < 2000; iter++ {
			if got := l.Iter(l.Value(iter)); got != iter {
				t.Fatalf("%+v iter %d -> %d", l, iter, got)
			}
		}
	}
}

func TestShardsNeverCollide(t *testing.T) {
	const n = 7
	seen := map[int64]int64{}
	for k := int64(1); k <= n; k++ {
		l := Layout{ShardID: k, TotalShards: n}
		for iter := int64(0); iter < 5000; iter++ {
			v := l.Value(iter)
			if owner, dup := seen[v]; dup {
				t.Fatalf("value %d from shard %d and shard %d", v, owner, k)
			}
			seen[v] = k
		}
	}
}

func TestSnapForward(t *testing.T) {
	cases := []struct {
		l       Layout
		in, out int64
	}{
		{New(2, 3), 0, 2}, {New(2, 3), 1, 2}, {New(2, 3), 2, 2}, {New(2, 3), 3, 5},
		{New(2, 3), 1000, 1001}, {New(2, 3), 5000, 5000},
		{New(1, 3), 2, 4}, {New(1, 3), 5000, 5002},
		{New(3, 3), 1, 3}, {New(3, 3), 1000, 1002},
		{New(1, 1), 777, 777},
	}
	for _, c := range cases {
		if got := c.l.SnapForward(c.in); got != c.out {
			t.Errorf("%+v SnapForward(%d)=%d, want %d", c.l, c.in, got, c.out)
		}
		if c.l.Value(c.l.Iter(c.out)) != c.out {
			t.Errorf("%+v snapped %d does not round-trip", c.l, c.out)
		}
	}
}

func TestSnapForward_smallestPositiveIsShardID(t *testing.T) {
	// Any start value >= 1 snaps to a value >= shard id, so iterations are never negative.
	for _, l := range []Layout{New(1, 1), New(2, 3), New(3, 3), New(5, 8)} {
		for v := int64(1); v < 50; v++ {
			s := l.SnapForward(v)
			if s < l.ShardID || l.Iter(s) < 0 {
				t.Errorf("%+v SnapForward(%d)=%d iter=%d", l, v, s, l.Iter(s))
			}
		}
	}
}

func TestMaxIter(t *testing.T) {
	l := New(2, 3)
	if got := l.MaxIter(14); got != 4 { // 2,5,8,11,14 -> iter 4
		t.Errorf("MaxIter(14)=%d, want 4", got)
	}
	if got := l.MaxIter(1); got != -1 {
		t.Errorf("MaxIter(1)=%d, want -1", got)
	}
	if v := l.Value(l.MaxIter(math.MaxInt64)); v < 0 {
		t.Errorf("value at MaxIter(MaxInt64) wrapped: %d", v)
	}
}
