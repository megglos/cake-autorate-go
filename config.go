// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Direction represents a traffic direction (download or upload).
type Direction string

const (
	DL Direction = "dl"
	UL Direction = "ul"
)

// Directions returns both traffic directions.
func Directions() [2]Direction { return [2]Direction{DL, UL} }

// DirectionConfig holds rate limits for one direction.
type DirectionConfig struct {
	Interface     string  `yaml:"interface"`
	Adjust        bool    `yaml:"adjust"`
	MinRateKbps   int     `yaml:"min_rate_kbps"`
	BaseRateKbps  int     `yaml:"base_rate_kbps"`
	MaxRateKbps   int     `yaml:"max_rate_kbps"`
	OWDDeltaDelayThrMs       float64 `yaml:"owd_delta_delay_thr_ms"`
	AvgOWDDeltaMaxAdjUpThrMs   float64 `yaml:"avg_owd_delta_max_adjust_up_thr_ms"`
	AvgOWDDeltaMaxAdjDownThrMs float64 `yaml:"avg_owd_delta_max_adjust_down_thr_ms"`
}

// LinkConfig holds the per-link configuration for a single WAN interface pair.
type LinkConfig struct {
	Name              string          `yaml:"name"`
	Download          DirectionConfig `yaml:"download"`
	Upload            DirectionConfig `yaml:"upload"`
	Reflectors        []string        `yaml:"reflectors"`
	PingSourceAddr    string          `yaml:"ping_source_addr"`
	PingInterfaceName string          `yaml:"ping_interface"`
}

// DirConfig returns the DirectionConfig for the given direction.
func (lc *LinkConfig) DirConfig(dir Direction) *DirectionConfig {
	if dir == DL {
		return &lc.Download
	}
	return &lc.Upload
}

// Config holds all configuration for cake-autorate.
type Config struct {
	// Multi-link configuration (preferred)
	Links []LinkConfig `yaml:"links"`

	// Legacy single-link fields (for backward compatibility)
	Download DirectionConfig `yaml:"download"`
	Upload   DirectionConfig `yaml:"upload"`
	Reflectors        []string `yaml:"reflectors"`
	PingSourceAddr    string   `yaml:"ping_source_addr"`
	PingInterfaceName string   `yaml:"ping_interface"`

	// Shared pinger settings
	PingerCount    int `yaml:"pinger_count"`
	PingIntervalMs int `yaml:"ping_interval_ms"`

	// Idle/sleep settings
	EnableSleepFunction     bool    `yaml:"enable_sleep_function"`
	ConnectionActiveThrKbps int     `yaml:"connection_active_thr_kbps"`
	SustainedIdleSleepThrS  float64 `yaml:"sustained_idle_sleep_thr_s"`

	// Bufferbloat detection
	BufferbloatDetectionWindow int `yaml:"bufferbloat_detection_window"`
	BufferbloatDetectionThr    int `yaml:"bufferbloat_detection_thr"`

	// EWMA parameters
	AlphaBaselineIncrease float64 `yaml:"alpha_baseline_increase"`
	AlphaBaselineDecrease float64 `yaml:"alpha_baseline_decrease"`
	AlphaDeltaEWMA        float64 `yaml:"alpha_delta_ewma"`

	// Rate adjustment factors
	ShaperRateMinAdjustDownBufferbloat float64 `yaml:"shaper_rate_min_adjust_down_bufferbloat"`
	ShaperRateMaxAdjustDownBufferbloat float64 `yaml:"shaper_rate_max_adjust_down_bufferbloat"`
	ShaperRateMinAdjustUpLoadHigh      float64 `yaml:"shaper_rate_min_adjust_up_load_high"`
	ShaperRateMaxAdjustUpLoadHigh      float64 `yaml:"shaper_rate_max_adjust_up_load_high"`
	ShaperRateAdjustDownLoadLow        float64 `yaml:"shaper_rate_adjust_down_load_low"`
	ShaperRateAdjustUpLoadLow          float64 `yaml:"shaper_rate_adjust_up_load_low"`

	// Load threshold
	HighLoadThr float64 `yaml:"high_load_thr"`

	// Timing
	MonitorIntervalMs          int `yaml:"monitor_interval_ms"`
	BufferbloatRefractoryPeriodMs int `yaml:"bufferbloat_refractory_period_ms"`
	DecayRefractoryPeriodMs      int `yaml:"decay_refractory_period_ms"`

	// Reflector health
	ReflectorResponseDeadlineS          float64 `yaml:"reflector_response_deadline_s"`
	ReflectorMisbehavingDetectionWindow int     `yaml:"reflector_misbehaving_detection_window"`
	ReflectorMisbehavingDetectionThr    int     `yaml:"reflector_misbehaving_detection_thr"`

	// Stall detection
	StallDetectionThr          int     `yaml:"stall_detection_thr"`
	ConnectionStallThrKbps     int     `yaml:"connection_stall_thr_kbps"`
	GlobalPingResponseTimeoutS float64 `yaml:"global_ping_response_timeout_s"`

	// Minimum shaper rate enforcement during idle/stall
	MinShaperRatesEnforcement bool `yaml:"min_shaper_rates_enforcement"`

	// Startup wait
	StartupWaitS float64 `yaml:"startup_wait_s"`

	// Logging
	LogToFile        bool   `yaml:"log_to_file"`
	LogFilePath      string `yaml:"log_file_path"`
	LogFileMaxSizeKB int    `yaml:"log_file_max_size_kb"`
	Debug            bool   `yaml:"debug"`
}

// DefaultConfig returns a configuration with sensible defaults matching
// the original cake-autorate project.
func DefaultConfig() *Config {
	return &Config{
		Download: DirectionConfig{
			Interface:                  "ifb-wan",
			Adjust:                     true,
			MinRateKbps:                5000,
			BaseRateKbps:               20000,
			MaxRateKbps:                80000,
			OWDDeltaDelayThrMs:         30.0,
			AvgOWDDeltaMaxAdjUpThrMs:   10.0,
			AvgOWDDeltaMaxAdjDownThrMs: 60.0,
		},
		Upload: DirectionConfig{
			Interface:                  "wan",
			Adjust:                     true,
			MinRateKbps:                5000,
			BaseRateKbps:               20000,
			MaxRateKbps:                35000,
			OWDDeltaDelayThrMs:         30.0,
			AvgOWDDeltaMaxAdjUpThrMs:   10.0,
			AvgOWDDeltaMaxAdjDownThrMs: 60.0,
		},
		Reflectors: []string{
			"1.1.1.1",
			"1.0.0.1",
			"8.8.8.8",
			"8.8.4.4",
			"9.9.9.9",
			"149.112.112.112",
			"208.67.222.222",
			"208.67.220.220",
		},
		PingerCount:    6,
		PingIntervalMs: 300,

		EnableSleepFunction:     true,
		ConnectionActiveThrKbps: 2000,
		SustainedIdleSleepThrS:  60.0,

		BufferbloatDetectionWindow: 6,
		BufferbloatDetectionThr:    3,

		AlphaBaselineIncrease: 0.001,
		AlphaBaselineDecrease: 0.9,
		AlphaDeltaEWMA:        0.095,

		ShaperRateMinAdjustDownBufferbloat: 0.99,
		ShaperRateMaxAdjustDownBufferbloat: 0.75,
		ShaperRateMinAdjustUpLoadHigh:      1.0,
		ShaperRateMaxAdjustUpLoadHigh:      1.04,
		ShaperRateAdjustDownLoadLow:        0.99,
		ShaperRateAdjustUpLoadLow:          1.01,

		HighLoadThr: 0.75,

		MonitorIntervalMs:             200,
		BufferbloatRefractoryPeriodMs: 300,
		DecayRefractoryPeriodMs:       1000,

		ReflectorResponseDeadlineS:          1.0,
		ReflectorMisbehavingDetectionWindow: 60,
		ReflectorMisbehavingDetectionThr:    3,

		StallDetectionThr:          5,
		ConnectionStallThrKbps:     10,
		GlobalPingResponseTimeoutS: 10.0,

		MinShaperRatesEnforcement: false,
		StartupWaitS:              0.0,

		LogToFile:        true,
		LogFilePath:      "/var/log/cake-autorate.log",
		LogFileMaxSizeKB: 2000,
		Debug:            false,
	}
}

// LoadConfig reads a YAML config file and merges with defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Migrate legacy single-link config to links list
	cfg.migrateLinks()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

// migrateLinks converts legacy top-level download/upload fields into a single
// link entry when the links list is empty. This preserves backward compatibility
// with existing single-link configs.
func (c *Config) migrateLinks() {
	if len(c.Links) > 0 {
		// For links that don't specify reflectors, inherit from top-level
		for i := range c.Links {
			if len(c.Links[i].Reflectors) == 0 {
				c.Links[i].Reflectors = c.Reflectors
			}
			if c.Links[i].Name == "" {
				c.Links[i].Name = fmt.Sprintf("link%d", i)
			}
		}
		return
	}

	// No links defined — migrate from legacy fields
	if c.Download.Interface != "" {
		c.Links = []LinkConfig{{
			Name:              "default",
			Download:          c.Download,
			Upload:            c.Upload,
			Reflectors:        c.Reflectors,
			PingSourceAddr:    c.PingSourceAddr,
			PingInterfaceName: c.PingInterfaceName,
		}}
	}
}

// Validate checks that the configuration values are sensible.
func (c *Config) Validate() error {
	if len(c.Links) == 0 {
		return fmt.Errorf("at least one link must be configured")
	}

	for _, link := range c.Links {
		if link.Name == "" {
			return fmt.Errorf("each link must have a name")
		}
		for _, dc := range []struct {
			name string
			dir  DirectionConfig
		}{
			{link.Name + ".download", link.Download},
			{link.Name + ".upload", link.Upload},
		} {
			if dc.dir.Interface == "" {
				return fmt.Errorf("%s interface must be set", dc.name)
			}
			if dc.dir.MinRateKbps <= 0 {
				return fmt.Errorf("%s min_rate_kbps must be positive", dc.name)
			}
			if dc.dir.BaseRateKbps < dc.dir.MinRateKbps {
				return fmt.Errorf("%s base_rate_kbps must be >= min_rate_kbps", dc.name)
			}
			if dc.dir.MaxRateKbps < dc.dir.BaseRateKbps {
				return fmt.Errorf("%s max_rate_kbps must be >= base_rate_kbps", dc.name)
			}
		}
		if len(link.Reflectors) == 0 {
			return fmt.Errorf("link %q: at least one reflector must be configured", link.Name)
		}
	}

	if c.PingerCount <= 0 {
		return fmt.Errorf("pinger_count must be positive")
	}
	if c.BufferbloatDetectionWindow <= 0 {
		return fmt.Errorf("bufferbloat_detection_window must be positive")
	}
	if c.BufferbloatDetectionThr <= 0 || c.BufferbloatDetectionThr > c.BufferbloatDetectionWindow {
		return fmt.Errorf("bufferbloat_detection_thr must be between 1 and bufferbloat_detection_window")
	}
	if c.ReflectorMisbehavingDetectionWindow <= 0 {
		return fmt.Errorf("reflector_misbehaving_detection_window must be positive")
	}
	if c.ReflectorMisbehavingDetectionThr <= 0 || c.ReflectorMisbehavingDetectionThr > c.ReflectorMisbehavingDetectionWindow {
		return fmt.Errorf("reflector_misbehaving_detection_thr must be between 1 and reflector_misbehaving_detection_window")
	}
	if c.MonitorIntervalMs <= 0 {
		return fmt.Errorf("monitor_interval_ms must be positive")
	}
	if c.PingIntervalMs <= 0 {
		return fmt.Errorf("ping_interval_ms must be positive")
	}
	if c.ReflectorResponseDeadlineS <= 0 {
		return fmt.Errorf("reflector_response_deadline_s must be positive")
	}
	return nil
}
