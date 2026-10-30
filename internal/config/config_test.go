package config

import (
	"os"
	"path/filepath"
	"strings"
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
	p := writeYAML(t, "shard_id: 1\ntotal_shards: 1\nstate_dir: ./s\ntenants: [btcl]\n")
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
	if c.SegmentRefillWatermark != 0.5 {
		t.Errorf("default watermark=%v, want 0.5", c.SegmentRefillWatermark)
	}
	if c.SnowflakeEpochMs != 1704067200000 {
		t.Errorf("default epoch=%d, want 1704067200000", c.SnowflakeEpochMs)
	}
}

func TestLoad_explicitValuesOverrideDefaults(t *testing.T) {
	p := writeYAML(t, `
shard_id: 3
total_shards: 5
state_dir: /var/lib/bn/shard-3
tenants: [btcl, acme]
legacy_tenant: btcl
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
		"shard_id zero":            "shard_id: 0\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\n",
		"shard_id too large":       "shard_id: 2000\ntotal_shards: 2000\nstate_dir: x\ntenants: [btcl]\n",
		"total_shards too large":   "shard_id: 1\ntotal_shards: 2000\nstate_dir: x\ntenants: [btcl]\n",
		"shard > total":            "shard_id: 5\ntotal_shards: 3\nstate_dir: x\ntenants: [btcl]\n",
		"missing state_dir":        "shard_id: 1\ntotal_shards: 1\ntenants: [btcl]\n",
		"missing tenants":          "shard_id: 1\ntotal_shards: 1\nstate_dir: x\n",
		"empty tenants":            "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: []\n",
		"tenant in capitals":       "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [BTCL]\n",
		"tenant with underscore":   "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [bt_cl]\n",
		"tenant twice":             "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl, btcl]\n",
		"legacy not a tenant":      "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nlegacy_tenant: acme\n",
		"tenants as a scalar":      "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: btcl\n",
		"watermark too high":       "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nsegment_refill_watermark: 1.5\n",
		"watermark zero":           "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nsegment_refill_watermark: 0\n", // zero triggers default, no error
		"listen_port out of range": "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nlisten_port: 70000\n",
		"uuid_key not hex":         "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nuuid_key: zz\n",
		"listen_address hostname":  "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nlisten_address: localhost\n",
		"listen_address bad IP":    "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nlisten_address: 10.0.0.300\n",
		"uuid_key wrong length":    "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nuuid_key: 00112233\n",
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

func TestLoad_uuidKey(t *testing.T) {
	p := writeYAML(t, "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\nuuid_key: 00112233445566778899aabbccddeeff\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := c.UUIDKeyBytes(); len(b) != 16 {
		t.Errorf("key bytes=%d, want 16", len(b))
	}
	p = writeYAML(t, "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\n")
	c, _ = Load(p)
	if b, err := c.UUIDKeyBytes(); b != nil || err != nil {
		t.Errorf("no key set: got %v %v", b, err)
	}
}

func TestLoad_listenAddress(t *testing.T) {
	cases := map[string]string{
		"":             ":7001",
		"127.0.0.1":    "127.0.0.1:7001",
		"10.10.199.11": "10.10.199.11:7001",
		"::1":          "[::1]:7001",
	}
	for addr, want := range cases {
		yaml := "shard_id: 1\ntotal_shards: 1\nstate_dir: x\ntenants: [btcl]\n"
		if addr != "" {
			yaml += "listen_address: \"" + addr + "\"\n"
		}
		c, err := Load(writeYAML(t, yaml))
		if err != nil {
			t.Fatalf("%q: %v", addr, err)
		}
		if got := c.ListenAddr(); got != want {
			t.Errorf("listen_address %q: ListenAddr()=%q, want %q", addr, got, want)
		}
	}
}

func TestLoad_tenantKeys(t *testing.T) {
	c, err := Load(writeYAML(t, "shard_id: 1\ntotal_shards: 2\nstate_dir: /var/lib/bucket-next/shard-1\ntenants: [btcl, acme]\nlegacy_tenant: btcl\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tenants) != 2 || c.Tenants[0] != "btcl" || c.Tenants[1] != "acme" || c.LegacyTenant != "btcl" {
		t.Errorf("tenants=%v legacy=%q", c.Tenants, c.LegacyTenant)
	}
	if c.StateDir != "/var/lib/bucket-next/shard-1" {
		t.Errorf("state_dir=%q", c.StateDir)
	}
}

func TestLoad_statePathIsRefusedNamingStateDir(t *testing.T) {
	_, err := Load(writeYAML(t, "shard_id: 1\ntotal_shards: 2\nstate_path: /var/lib/bucket-next/shard-1/state.json\ntenants: [btcl]\n"))
	if err == nil || !strings.Contains(err.Error(), "state_dir: /var/lib/bucket-next/shard-1") {
		t.Fatalf("want the state_dir pointer, got %v", err)
	}
}
