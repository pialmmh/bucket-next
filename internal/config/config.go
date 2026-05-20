package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the full configuration loaded from a single YAML file at startup.
type Config struct {
	ShardID     int    `yaml:"shard_id"`
	TotalShards int    `yaml:"total_shards"`
	ListenPort  int    `yaml:"listen_port"`
	StatePath   string `yaml:"state_path"`

	SegmentSize            int     `yaml:"segment_size"`
	SegmentRefillWatermark float64 `yaml:"segment_refill_watermark"`
	ClockDriftToleranceMs  int64   `yaml:"clock_drift_tolerance_ms"`

	SnowflakeEpochMs int64 `yaml:"snowflake_epoch_ms"`
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
		c.SegmentRefillWatermark = 0.9
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
	if c.StatePath == "" {
		return fmt.Errorf("state_path is required")
	}
	if c.SegmentSize < 1 {
		return fmt.Errorf("segment_size must be >= 1, got %d", c.SegmentSize)
	}
	if c.SegmentRefillWatermark <= 0 || c.SegmentRefillWatermark >= 1 {
		return fmt.Errorf("segment_refill_watermark must be in (0, 1), got %f", c.SegmentRefillWatermark)
	}
	return nil
}
