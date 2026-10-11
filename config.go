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
	"reflect"
	"regexp"
	"slices"
	"strings"

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
	Interface                  string  `yaml:"interface"`
	Adjust                     bool    `yaml:"adjust"`
	MinRateKbps                int     `yaml:"min_rate_kbps"`
	BaseRateKbps               int     `yaml:"base_rate_kbps"`
	MaxRateKbps                int     `yaml:"max_rate_kbps"`
	OWDDeltaDelayThrMs         float64 `yaml:"owd_delta_delay_thr_ms"`
	AvgOWDDeltaMaxAdjUpThrMs   float64 `yaml:"avg_owd_delta_max_adjust_up_thr_ms"`
	AvgOWDDeltaMaxAdjDownThrMs float64 `yaml:"avg_owd_delta_max_adjust_down_thr_ms"`

	// LatencyTargetP95Ms is the target for the p95 of the delay delta at
	// high load (0: off). Core then changes the three delay thresholds
	// slowly, so that the p95 is near the target.
	LatencyTargetP95Ms float64 `yaml:"latency_target_p95_ms"`
	// LatencyTargetMinThrMs is the minimum delay threshold for the latency
	// target (0: 10 ms). Set it above the delay that no shaper can remove
	// (on 5G, the upload grant delay: see the calibrate command).
	LatencyTargetMinThrMs float64 `yaml:"latency_target_min_thr_ms"`
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
	Download          DirectionConfig `yaml:"download"`
	Upload            DirectionConfig `yaml:"upload"`
	Reflectors        []string        `yaml:"reflectors"`
	PingSourceAddr    string          `yaml:"ping_source_addr"`
	PingInterfaceName string          `yaml:"ping_interface"`

	// Shared pinger settings
	PingerCount    int `yaml:"pinger_count"`
	PingIntervalMs int `yaml:"ping_interval_ms"`
	// PingMethod is PingMethodEcho or PingMethodTimestamp.
	PingMethod string `yaml:"ping_method"`

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
	MonitorIntervalMs             int `yaml:"monitor_interval_ms"`
	BufferbloatRefractoryPeriodMs int `yaml:"bufferbloat_refractory_period_ms"`
	// RefractoryOnDecrease applies the bufferbloat refractory period also
	// to decreases: one decrease in each period (as in the original). If
	// false, each ping with bufferbloat decreases the rate.
	RefractoryOnDecrease    bool `yaml:"refractory_on_decrease"`
	DecayRefractoryPeriodMs int  `yaml:"decay_refractory_period_ms"`
	// LearnCrossDelay makes Core learn the upload delay that download
	// activity causes, and ignore it. Only with one-way delays.
	LearnCrossDelay bool `yaml:"learn_cross_delay"`

	// LatencyTargetWindowS is the time window for the p95 of the latency
	// target (latency_target_p95_ms). After each window, Core changes the
	// thresholds one time.
	LatencyTargetWindowS float64 `yaml:"latency_target_window_s"`

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

	// unknownKeys are the keys in the config file that the program does not
	// know (for example, a typing error). LoadConfig sets them.
	unknownKeys []string
}

// Ping methods.
const (
	// PingMethodEcho uses ICMP echo. It measures only the RTT. Core uses
	// RTT/2 as the delay of each direction.
	PingMethodEcho = "icmp"
	// PingMethodTimestamp uses ICMP timestamps. It measures the delay of
	// each direction. Thus, a queue in one direction does not decrease the
	// rate of the other direction. Not all reflectors reply.
	PingMethodTimestamp = "icmp-timestamp"
)

// defaultTimestampReflectors are the reflectors for PingMethodTimestamp if
// the config does not set reflectors. Many public DNS servers (for example
// 1.1.1.1, 8.8.8.8, 9.9.9.9) do not reply to ICMP timestamp requests.
var defaultTimestampReflectors = []string{
	"94.140.14.14", // AdGuard DNS
	"94.140.15.15", // AdGuard DNS
	"fra-de-ping.vultr.com",
	"ams-nl-ping.vultr.com",
	"par-fr-ping.vultr.com",
	"fsn1-speed.hetzner.com",
	"nbg1-speed.hetzner.com",
	"hel1-speed.hetzner.com",
}

// defaultReflectors returns the default reflectors for a ping method. The
// usual public DNS reflectors do not reply to ICMP timestamp requests.
func defaultReflectors(method string) []string {
	if method == PingMethodTimestamp {
		return slices.Clone(defaultTimestampReflectors)
	}
	return DefaultConfig().Reflectors
}

// noTimestampReply reports whether a reflector is known to not reply to ICMP
// timestamp requests. These are the default reflectors for ICMP echo.
func noTimestampReply(reflector string) bool {
	return slices.Contains(DefaultConfig().Reflectors, reflector)
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
		PingMethod:     PingMethodEcho,

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

		LatencyTargetWindowS: 30,

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
	// The default reflectors depend on ping_method. Thus, set them after
	// the parse, and only if the config does not set reflectors.
	cfg.Reflectors = nil

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err == nil && len(root.Content) > 0 {
		cfg.unknownKeys = unknownKeys(root.Content[0], reflect.TypeOf(*cfg), "")
	}

	if len(cfg.Reflectors) == 0 {
		cfg.Reflectors = defaultReflectors(cfg.PingMethod)
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
			// The YAML parser creates each link from zero values. Thus, a
			// threshold that the link does not set is 0. Use the top-level
			// value (the default, or the legacy setting) instead.
			inheritThresholds(&c.Links[i].Download, &c.Download)
			inheritThresholds(&c.Links[i].Upload, &c.Upload)
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

// inheritThresholds sets each delay threshold of dc that is 0 to the value
// in from.
func inheritThresholds(dc, from *DirectionConfig) {
	if dc.OWDDeltaDelayThrMs == 0 {
		dc.OWDDeltaDelayThrMs = from.OWDDeltaDelayThrMs
	}
	if dc.AvgOWDDeltaMaxAdjUpThrMs == 0 {
		dc.AvgOWDDeltaMaxAdjUpThrMs = from.AvgOWDDeltaMaxAdjUpThrMs
	}
	if dc.AvgOWDDeltaMaxAdjDownThrMs == 0 {
		dc.AvgOWDDeltaMaxAdjDownThrMs = from.AvgOWDDeltaMaxAdjDownThrMs
	}
}

// unknownKeys returns a message for each key in the YAML mapping node that
// is not a yaml field of the struct type t. It examines nested structs and
// lists of structs. path is the location of node in the file.
func unknownKeys(node *yaml.Node, t reflect.Type, path string) []string {
	node = resolveAlias(node)
	if node.Kind != yaml.MappingNode || t.Kind() != reflect.Struct {
		return nil
	}
	fields := make(map[string]reflect.Type)
	for i := range t.NumField() {
		f := t.Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); name != "" && name != "-" {
			fields[name] = f.Type
		}
	}
	var msgs []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, resolveAlias(node.Content[i+1])
		if node.Content[i].Tag == "!!merge" {
			// YAML merge key: examine the keys of each merged mapping.
			merged := []*yaml.Node{value}
			if value.Kind == yaml.SequenceNode {
				merged = value.Content
			}
			for _, m := range merged {
				msgs = append(msgs, unknownKeys(m, t, path)...)
			}
			continue
		}
		ft, ok := fields[key]
		if !ok {
			msg := fmt.Sprintf("unknown config key %q (line %d) has no effect", path+key, node.Content[i].Line)
			if s := closestKey(key, fields); s != "" {
				msg += fmt.Sprintf(". Did you mean %q?", path+s)
			}
			msgs = append(msgs, msg)
			continue
		}
		switch {
		case ft.Kind() == reflect.Struct:
			msgs = append(msgs, unknownKeys(value, ft, path+key+".")...)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct && value.Kind == yaml.SequenceNode:
			for j, item := range value.Content {
				msgs = append(msgs, unknownKeys(item, ft.Elem(), fmt.Sprintf("%s%s[%d].", path, key, j))...)
			}
		}
	}
	return msgs
}

// resolveAlias returns the node that an alias node refers to.
func resolveAlias(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

// closestKey returns the known key that is nearest to key (edit distance at
// most 1/3 of the length), or "" if no key is near.
func closestKey(key string, known map[string]reflect.Type) string {
	best, bestDist := "", len(key)/3+1
	for k := range known {
		if d := editDistance(key, k); d < bestDist || (d == bestDist && best != "" && k < best) {
			best, bestDist = k, d
		}
	}
	return best
}

// editDistance returns the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Warnings returns the problems in the config that do not stop the program.
func (c *Config) Warnings() []string {
	w := slices.Clone(c.unknownKeys)
	if c.PingMethod == PingMethodTimestamp {
		for _, link := range c.Links {
			var bad []string
			for _, r := range link.Reflectors {
				if noTimestampReply(r) {
					bad = append(bad, r)
				}
			}
			if len(bad) > 0 {
				w = append(w, fmt.Sprintf("link %q: reflectors %v do not reply to ICMP timestamp requests. "+
					"Remove them from the config", link.Name, bad))
			}
		}
	}
	return w
}

// ifnameRe matches valid Linux interface names: alphanumerics, hyphens, dots,
// and underscores, up to IFNAMSIZ-1 (15) characters. This rejects path
// separators, whitespace, and other characters that could cause issues when
// interpolated into sysfs paths or tc arguments.
var ifnameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$`)

// validateIfname checks that name is a valid Linux network interface name.
func validateIfname(name string) error {
	if !ifnameRe.MatchString(name) {
		return fmt.Errorf("invalid interface name %q: must be 1-15 chars of [a-zA-Z0-9._-], starting with alphanumeric", name)
	}
	return nil
}

// Validate checks that the configuration values are sensible.
func (c *Config) Validate() error {
	if c.LatencyTargetWindowS <= 0 {
		return fmt.Errorf("latency_target_window_s must be positive")
	}
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
			if err := validateIfname(dc.dir.Interface); err != nil {
				return fmt.Errorf("%s: %w", dc.name, err)
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
			// With a threshold of 0, each ping shows bufferbloat, and the
			// shaper rate goes to min_rate_kbps.
			if dc.dir.OWDDeltaDelayThrMs <= 0 {
				return fmt.Errorf("%s owd_delta_delay_thr_ms must be positive", dc.name)
			}
			if dc.dir.AvgOWDDeltaMaxAdjUpThrMs <= 0 {
				return fmt.Errorf("%s avg_owd_delta_max_adjust_up_thr_ms must be positive", dc.name)
			}
			if dc.dir.AvgOWDDeltaMaxAdjDownThrMs <= 0 {
				return fmt.Errorf("%s avg_owd_delta_max_adjust_down_thr_ms must be positive", dc.name)
			}
			if dc.dir.LatencyTargetP95Ms < 0 || dc.dir.LatencyTargetMinThrMs < 0 {
				return fmt.Errorf("%s latency_target_p95_ms and latency_target_min_thr_ms must not be negative", dc.name)
			}
			if dc.dir.LatencyTargetMinThrMs > dc.dir.OWDDeltaDelayThrMs*latencyTargetMaxScale {
				return fmt.Errorf("%s latency_target_min_thr_ms must not be more than %g x owd_delta_delay_thr_ms", dc.name, latencyTargetMaxScale)
			}
		}
		if len(link.Reflectors) == 0 {
			return fmt.Errorf("link %q: at least one reflector must be configured", link.Name)
		}
		if c.PingMethod == PingMethodTimestamp && !slices.ContainsFunc(link.Reflectors, func(r string) bool { return !noTimestampReply(r) }) {
			return fmt.Errorf("link %q: no reflector replies to ICMP timestamp requests. "+
				"Remove the reflectors from the config to use the default list for icmp-timestamp", link.Name)
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
	if c.PingMethod != PingMethodEcho && c.PingMethod != PingMethodTimestamp {
		return fmt.Errorf("ping_method must be %q or %q", PingMethodEcho, PingMethodTimestamp)
	}
	if c.ReflectorResponseDeadlineS <= 0 {
		return fmt.Errorf("reflector_response_deadline_s must be positive")
	}

	// Validate float tuning parameters
	if c.HighLoadThr < 0 || c.HighLoadThr > 1 {
		return fmt.Errorf("high_load_thr must be between 0 and 1")
	}
	if c.AlphaBaselineIncrease <= 0 || c.AlphaBaselineIncrease > 1 {
		return fmt.Errorf("alpha_baseline_increase must be in (0, 1]")
	}
	if c.AlphaBaselineDecrease <= 0 || c.AlphaBaselineDecrease > 1 {
		return fmt.Errorf("alpha_baseline_decrease must be in (0, 1]")
	}
	if c.AlphaDeltaEWMA <= 0 || c.AlphaDeltaEWMA > 1 {
		return fmt.Errorf("alpha_delta_ewma must be in (0, 1]")
	}
	if c.ShaperRateMinAdjustDownBufferbloat <= 0 || c.ShaperRateMinAdjustDownBufferbloat > 1 {
		return fmt.Errorf("shaper_rate_min_adjust_down_bufferbloat must be in (0, 1]")
	}
	if c.ShaperRateMaxAdjustDownBufferbloat <= 0 || c.ShaperRateMaxAdjustDownBufferbloat > 1 {
		return fmt.Errorf("shaper_rate_max_adjust_down_bufferbloat must be in (0, 1]")
	}
	if c.ShaperRateMaxAdjustDownBufferbloat > c.ShaperRateMinAdjustDownBufferbloat {
		return fmt.Errorf("shaper_rate_max_adjust_down_bufferbloat must be <= shaper_rate_min_adjust_down_bufferbloat")
	}
	if c.ShaperRateMinAdjustUpLoadHigh < 1 {
		return fmt.Errorf("shaper_rate_min_adjust_up_load_high must be >= 1")
	}
	if c.ShaperRateMaxAdjustUpLoadHigh < c.ShaperRateMinAdjustUpLoadHigh {
		return fmt.Errorf("shaper_rate_max_adjust_up_load_high must be >= shaper_rate_min_adjust_up_load_high")
	}
	if c.ShaperRateAdjustDownLoadLow <= 0 || c.ShaperRateAdjustDownLoadLow > 1 {
		return fmt.Errorf("shaper_rate_adjust_down_load_low must be in (0, 1]")
	}
	if c.ShaperRateAdjustUpLoadLow < 1 {
		return fmt.Errorf("shaper_rate_adjust_up_load_low must be >= 1")
	}
	return nil
}
