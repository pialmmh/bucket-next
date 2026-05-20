package forge

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Bit layout: 41-bit timestamp | 10-bit shard | 12-bit sequence = 63 bits + 1 sign bit.
const (
	TimestampBits = 41
	ShardIDBits   = 10
	SequenceBits  = 12

	maxShardID  = 1<<ShardIDBits - 1  // 1023 (0-based)
	maxSequence = 1<<SequenceBits - 1 // 4095

	shardIDShift   = SequenceBits                // 12
	timestampShift = SequenceBits + ShardIDBits  // 22
)

// Forge mints 64-bit Snowflake IDs for one shard.
type Forge struct {
	shardID         int64 // 0-based for bit packing
	epochMs         int64
	clockDriftTolMs int64

	mu            sync.Mutex
	lastTimestamp int64
	sequence      int64

	generated  atomic.Int64
	collisions atomic.Int64
	waits      atomic.Int64
}

// Info is the metadata reported by /api/types.
type Info struct {
	ShardID        int               `json:"shardId"`
	TotalShards    int               `json:"totalShards"`
	Epoch          string            `json:"epoch"`
	MaxIdsPerMs    int               `json:"maxIdsPerMs"`
	MaxShards      int               `json:"maxShards"`
	BitsAllocation map[string]int    `json:"bitsAllocation"`
}

// Parsed is the decoded form of a Snowflake ID.
type Parsed struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	Date      string `json:"date"`
	ShardID   int    `json:"shardId"`
	Sequence  int    `json:"sequence"`
	Binary    string `json:"binary"`
}

func New(shardID int, epochMs, clockDriftTolMs int64) (*Forge, error) {
	if shardID < 1 || shardID > maxShardID+1 {
		return nil, fmt.Errorf("shardID must be in [1, %d], got %d", maxShardID+1, shardID)
	}
	return &Forge{
		shardID:         int64(shardID) - 1,
		epochMs:         epochMs,
		clockDriftTolMs: clockDriftTolMs,
		lastTimestamp:   -1,
	}, nil
}

// Generate returns one Snowflake ID as a decimal string.
func (f *Forge) Generate() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ts := nowMs()
	if ts < f.lastTimestamp {
		drift := f.lastTimestamp - ts
		if drift > f.clockDriftTolMs {
			return "", fmt.Errorf("clock moved backwards by %d ms (tolerance %d)", drift, f.clockDriftTolMs)
		}
		for ts < f.lastTimestamp {
			ts = nowMs()
		}
	}

	if ts == f.lastTimestamp {
		f.sequence = (f.sequence + 1) & maxSequence
		if f.sequence == 0 {
			f.waits.Add(1)
			for ts <= f.lastTimestamp {
				ts = nowMs()
			}
		}
		f.collisions.Add(1)
	} else {
		f.sequence = 0
	}

	f.lastTimestamp = ts
	id := ((ts - f.epochMs) << timestampShift) | (f.shardID << shardIDShift) | f.sequence
	f.generated.Add(1)
	return strconv.FormatInt(id, 10), nil
}

// GenerateBatch mints n IDs in a tight loop.
func (f *Forge) GenerateBatch(n int) ([]string, error) {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := f.Generate()
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// Parse decodes a Snowflake ID into its component parts.
func (f *Forge) Parse(idStr string) (*Parsed, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid id %q: %w", idStr, err)
	}
	seq := int(id & maxSequence)
	sid := int((id>>shardIDShift)&maxShardID) + 1
	ts := (id >> timestampShift) + f.epochMs
	return &Parsed{
		ID:        idStr,
		Timestamp: ts,
		Date:      time.UnixMilli(ts).UTC().Format(time.RFC3339Nano),
		ShardID:   sid,
		Sequence:  seq,
		Binary:    fmt.Sprintf("%064b", id),
	}, nil
}

// Stats returns running counters.
func (f *Forge) Stats() (generated, collisions, waits int64) {
	return f.generated.Load(), f.collisions.Load(), f.waits.Load()
}

// Info returns the forge configuration for /api/types.
func (f *Forge) Info(totalShards int) Info {
	return Info{
		ShardID:     int(f.shardID) + 1,
		TotalShards: totalShards,
		Epoch:       time.UnixMilli(f.epochMs).UTC().Format(time.RFC3339Nano),
		MaxIdsPerMs: maxSequence + 1,
		MaxShards:   maxShardID + 1,
		BitsAllocation: map[string]int{
			"timestamp": TimestampBits,
			"shardId":   ShardIDBits,
			"sequence":  SequenceBits,
			"total":     64,
		},
	}
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
