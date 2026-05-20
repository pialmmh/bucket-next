package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_minimalAppliesDefaults(t *testing.T) {
	p := writeYAML(t, "shard_id: 1\ntotal_shards: 1\nstate_path: ./s.json\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenPort != 7001 {
		t.Errorf("default listen_port=%d, want 7001", c.ListenPort)
	}
	if c.SegmentSize != 1000 {
		t.Errorf("default segment_size=%d, want 1000", c.SegmentSize)
	}
	if c.SegmentRefillWatermark != 0.9 {
		t.Errorf("default watermark=%v, want 0.9", c.SegmentRefillWatermark)
	}
	if c.SnowflakeEpochMs != 1704067200000 {
		t.Errorf("default epoch=%d, want 1704067200000", c.SnowflakeEpochMs)
	}
}

func TestLoad_explicitValuesOverrideDefaults(t *testing.T) {
	p := writeYAML(t, `
shard_id: 3
total_shards: 5
state_path: /var/lib/bn/state.json
listen_port: 9000
segment_size: 250
segment_refill_watermark: 0.5
clock_drift_tolerance_ms: 100
snowflake_epoch_ms: 1500000000000
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ShardID != 3 || c.TotalShards != 5 {
		t.Errorf("shard/total mismatch: %+v", c)
	}
	if c.ListenPort != 9000 || c.SegmentSize != 250 {
		t.Errorf("explicit values not honoured: %+v", c)
	}
	if c.ClockDriftToleranceMs != 100 || c.SnowflakeEpochMs != 1500000000000 {
		t.Errorf("clock/epoch not honoured: %+v", c)
	}
}

func TestLoad_validationFailures(t *testing.T) {
	cases := map[string]string{
		"shard_id zero":          "shard_id: 0\ntotal_shards: 1\nstate_path: x\n",
		"shard_id too large":     "shard_id: 2000\ntotal_shards: 2000\nstate_path: x\n",
		"total_shards too large": "shard_id: 1\ntotal_shards: 2000\nstate_path: x\n",
		"shard > total":          "shard_id: 5\ntotal_shards: 3\nstate_path: x\n",
		"missing state_path":     "shard_id: 1\ntotal_shards: 1\n",
		"watermark too high":     "shard_id: 1\ntotal_shards: 1\nstate_path: x\nsegment_refill_watermark: 1.5\n",
		"watermark zero":         "shard_id: 1\ntotal_shards: 1\nstate_path: x\nsegment_refill_watermark: 0\n", // zero triggers default, no error
		"listen_port out of range": "shard_id: 1\ntotal_shards: 1\nstate_path: x\nlisten_port: 70000\n",
	}
	// The "watermark zero" case actually triggers the default — remove from negative cases.
	delete(cases, "watermark zero")

	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeYAML(t, yaml)
			if _, err := Load(p); err == nil {
				t.Errorf("%s: expected error, got nil", name)
			}
		})
	}
}

func TestLoad_missingFile(t *testing.T) {
	if _, err := Load("/nonexistent/path/file.yaml"); err == nil {
		t.Error("expected error on missing file")
	}
}

func TestLoad_malformedYAML(t *testing.T) {
	p := writeYAML(t, "shard_id: not-a-number\n")
	if _, err := Load(p); err == nil {
		t.Error("expected parse error on bad YAML")
	}
}
