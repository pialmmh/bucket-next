package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/telcobright/bucket-next/internal/names"
)

// Config is the full configuration loaded from a single YAML file at startup.
type Config struct {
	ShardID     int `yaml:"shard_id"`
	TotalShards int `yaml:"total_shards"`
	ListenPort  int `yaml:"listen_port"`

	// StateDir is this shard's state directory: shard.json and tenants/<tenant>.json.
	StateDir string `yaml:"state_dir"`
	// StatePath is the pre-tenant key (one state file per shard). It is refused, naming
	// state_dir instead.
	StatePath string `yaml:"state_path"`

	// Tenants are the tenants this installation serves (tenant-master's tenant ids). Every
	// /api/ call names one; a call for any other is refused. A call never creates a tenant.
	Tenants []string `yaml:"tenants"`
	// LegacyTenant switches the pre-tenant roads (/api/next-id/{entity} and the rest, no
	// tenant in the path) on, acting on this tenant, and names the owner of a pre-tenant
	// state.json's counters. Empty: those roads are refused, in words.
	LegacyTenant string `yaml:"legacy_tenant"`

	// ListenAddress is the IP the HTTP server binds. Empty (the default) means every
	// interface. The API has no authentication, so a dev box should say 127.0.0.1 and a
	// server its private IP.
	ListenAddress string `yaml:"listen_address"`

	SegmentSize            int     `yaml:"segment_size"`
	SegmentRefillWatermark float64 `yaml:"segment_refill_watermark"`
	ClockDriftToleranceMs  int64   `yaml:"clock_drift_tolerance_ms"`

	SnowflakeEpochMs int64 `yaml:"snowflake_epoch_ms"`

	// UUIDKey is the hex AES key (32, 48 or 64 hex chars) that scrambles uuid* IDs. It must
	// be identical on every shard and must never change once IDs are issued. Empty means a
	// built-in key: IDs stay unique but are predictable to anyone with the source.
	UUIDKey string `yaml:"uuid_key"`
}

// Load reads a YAML config file, applies defaults, and validates.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.ListenPort == 0 {
		c.ListenPort = 7001
	}
	if c.SegmentSize == 0 {
		c.SegmentSize = 1000
	}
	if c.SegmentRefillWatermark == 0 {
		c.SegmentRefillWatermark = 0.5
	}
	if c.SnowflakeEpochMs == 0 {
		c.SnowflakeEpochMs = 1704067200000
	}
}

func (c *Config) validate() error {
	if c.ShardID < 1 || c.ShardID > 1024 {
		return fmt.Errorf("shard_id must be in [1, 1024], got %d", c.ShardID)
	}
	if c.TotalShards < 1 || c.TotalShards > 1024 {
		return fmt.Errorf("total_shards must be in [1, 1024], got %d", c.TotalShards)
	}
	if c.ShardID > c.TotalShards {
		return fmt.Errorf("shard_id (%d) cannot exceed total_shards (%d)", c.ShardID, c.TotalShards)
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("listen_port must be in [1, 65535], got %d", c.ListenPort)
	}
	if c.ListenAddress != "" && net.ParseIP(c.ListenAddress) == nil {
		return fmt.Errorf("listen_address must be an IP address such as 127.0.0.1 or the host's private IP "+
			"(empty = all interfaces), got %q", c.ListenAddress)
	}
	if c.StatePath != "" {
		return fmt.Errorf("state_path is replaced by state_dir: the shard's directory, which holds shard.json and "+
			"tenants/<tenant>.json (a state.json found in it is carried over into legacy_tenant at the first start) — "+
			"e.g. state_dir: %s", filepath.Dir(c.StatePath))
	}
	if c.StateDir == "" {
		return fmt.Errorf("state_dir is required")
	}
	if len(c.Tenants) == 0 {
		return fmt.Errorf("tenants is required: the tenants this installation serves, e.g. tenants: [btcl]")
	}
	seen := map[string]bool{}
	for _, t := range c.Tenants {
		if !names.ValidTenant(t) {
			return fmt.Errorf("tenants: %q breaks the tenant rule %s (tenant-master's tenant id: lowercase letters "+
				"and digits, a letter first, 2-16 characters)", t, names.TenantRule)
		}
		if seen[t] {
			return fmt.Errorf("tenants: %q is listed twice", t)
		}
		seen[t] = true
	}
	if c.LegacyTenant != "" && !seen[c.LegacyTenant] {
		return fmt.Errorf("legacy_tenant %q is not one of tenants %v", c.LegacyTenant, c.Tenants)
	}
	if c.SegmentSize < 1 {
		return fmt.Errorf("segment_size must be >= 1, got %d", c.SegmentSize)
	}
	if c.SegmentRefillWatermark <= 0 || c.SegmentRefillWatermark >= 1 {
		return fmt.Errorf("segment_refill_watermark must be in (0, 1), got %f", c.SegmentRefillWatermark)
	}
	if c.UUIDKey != "" {
		if _, err := c.UUIDKeyBytes(); err != nil {
			return err
		}
	}
	return nil
}

// ListenAddr is the address the HTTP server binds: listen_address:listen_port, or every
// interface when listen_address is empty.
func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.ListenPort))
}

// UUIDKeyBytes decodes uuid_key. It returns nil when no key is set.
func (c *Config) UUIDKeyBytes() ([]byte, error) {
	if c.UUIDKey == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(strings.TrimSpace(c.UUIDKey))
	if err != nil {
		return nil, fmt.Errorf("uuid_key must be hex: %w", err)
	}
	if n := len(b); n != 16 && n != 24 && n != 32 {
		return nil, fmt.Errorf("uuid_key must be 32, 48 or 64 hex chars (16/24/32 bytes), got %d bytes", n)
	}
	return b, nil
}
