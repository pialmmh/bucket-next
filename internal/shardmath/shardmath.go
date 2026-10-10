// Package shardmath holds the interleaving arithmetic shared by the service and the CLI.
//
// Shard k of N owns every value v with v ≡ k (mod N). Iteration i maps to v = k + i·N, so
// iteration 0 → k, 1 → k+N, 2 → k+2N, … Two shards of the same cluster can never produce
// the same value, without talking to each other.
package shardmath

// Layout is one shard's position in the cluster.
type Layout struct {
	ShardID     int64
	TotalShards int64
}

func New(shardID, totalShards int) Layout {
	return Layout{ShardID: int64(shardID), TotalShards: int64(totalShards)}
}

// Value maps an iteration to the value this shard emits for it.
func (l Layout) Value(iter int64) int64 {
	return l.ShardID + iter*l.TotalShards
}

// Iter is the inverse of Value. v must carry this shard's residue (see SnapForward).
func (l Layout) Iter(v int64) int64 {
	return (v - l.ShardID) / l.TotalShards
}

// SnapForward rounds v up to the nearest value ≥ v that belongs to this shard.
// Callers must check the result against the type's maximum: near math.MaxInt64 the
// addition can wrap, which shows up as a result smaller than v.
func (l Layout) SnapForward(v int64) int64 {
	n := l.TotalShards
	rem := ((v % n) + n) % n
	target := l.ShardID % n
	if rem == target {
		return v
	}
	diff := target - rem
	if diff < 0 {
		diff += n
	}
	return v + diff
}

// MaxIter is the largest iteration whose value does not exceed maxValue, or -1 if even
// iteration 0 is too large.
func (l Layout) MaxIter(maxValue int64) int64 {
	if maxValue < l.ShardID {
		return -1
	}
	return (maxValue - l.ShardID) / l.TotalShards
}
