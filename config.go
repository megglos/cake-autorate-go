// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
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
}

// LinkConfig holds the per-link configuration for a single WAN interface pair.
type LinkConfig struct {
	Name              string          `yaml:"name"`
	Download          DirectionConfig `yaml:"download"`
	Upload            DirectionConfig `yaml:"upload"`
	Reflectors        []string        `yaml:"reflectors"`
	PingSourceAddr    string          `yaml:"ping_source_addr"`
	PingInterfaceName string          `yaml:"ping_interface"`

	// The next five keys and the keys after Mode override the top-level key of the same name for
	// this link. A key that the link does not set (nil) uses the top-level
	// value. Use ForLink to get the values that apply to a link.
	PingerCount     *int    `yaml:"pinger_count"`
	PingIntervalMs  *int    `yaml:"ping_interval_ms"`
	PingMethod      *string `yaml:"ping_method"`
	LearnCrossDelay *bool   `yaml:"learn_cross_delay"`

	// Mode overrides the top-level mode for this link. The next three
	// keys override the top-level keys of the same name. They apply only
	// in the mode custom (see LinkMode).
	Mode                 *string `yaml:"mode"`
	RefractoryOnDecrease *bool   `yaml:"refractory_on_decrease"`
	IncreaseToDelayThr   *bool   `yaml:"increase_to_delay_thr"`
	AutoMaxRate          *bool   `yaml:"auto_max_rate"`
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
	RefractoryOnDecrease bool `yaml:"refractory_on_decrease"`
	// RepeatDecreaseOnRise allows a second decrease in the refractory
	// period only when the delay is higher than at the last decrease. After
	// a decrease, the queue empties only slowly, and the detection window
	// keeps the delayed pings. Without this option, each of these pings
	// decreases the rate again. RefractoryOnDecrease has priority.
	RepeatDecreaseOnRise    bool `yaml:"repeat_decrease_on_rise"`
	DecayRefractoryPeriodMs int  `yaml:"decay_refractory_period_ms"`
	// StallRecoveryMs sets the rate back after a short capacity stall: if
	// the delay is low again in this time after the first decrease of a
	// bufferbloat episode, the rate goes to
	// ShaperRateMaxAdjustDownBufferbloat times the rate before the episode.
	// Only with one-way delays (icmp-timestamp). 0 disables it.
	StallRecoveryMs int `yaml:"stall_recovery_ms"`
	// LearnCrossDelay makes Core learn the upload delay that download
	// activity causes, and ignore it. Only with one-way delays.
	LearnCrossDelay bool `yaml:"learn_cross_delay"`
	// IncreaseToDelayThr changes the increase at high load (as in the
	// original): the increase is at its maximum while the delay is at or
	// below avg_owd_delta_max_adjust_up_thr_ms, and decreases linearly to
	// zero at the delay threshold (owd_delta_delay_thr_ms). If false, the
	// increase decreases from its maximum at a delay of 0 to zero at
	// avg_owd_delta_max_adjust_up_thr_ms. Use it on a link that adds a
	// delay under load that the shaper cannot remove (for example, the
	// scheduler of a 5G cell). Then the rate can increase also with this
	// delay.
	IncreaseToDelayThr bool `yaml:"increase_to_delay_thr"`
	// AutoMaxRate lets the maximum rate increase above max_rate_kbps. If
	// the rate stays at the maximum for 2 s at high load without
	// bufferbloat, and the achieved rate is at least 90 % of it, the
	// configured maximum is not a limit of the link: the maximum increases
	// by 10 %, up to AutoMaxRateLimitFactor times max_rate_kbps. While the
	// load is high and the rate stays below 80 % of the maximum, the maximum
	// decreases slowly (time constant 100 s), not below max_rate_kbps. Use
	// it on a link with a capacity that changes much, for example a 5G cell
	// at night.
	AutoMaxRate            bool    `yaml:"auto_max_rate"`
	AutoMaxRateLimitFactor float64 `yaml:"auto_max_rate_limit_factor"`
	// Mode is the link mode after the start (latency, balanced, bandwidth
	// or custom, see LinkMode). A link can override it. The control command
	// "mode" changes it while the program runs. Without the key, LoadConfig
	// sets it (see defaultMode).
	Mode string `yaml:"mode"`

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

	// RecordDir is the directory of the recordings (see docs/recording.md).
	// At each start, the program writes a new file in it. Empty: no
	// recording. The flag --record overrides it.
	RecordDir string `yaml:"record_dir"`
	// RecordMaxMB stops a recording in RecordDir when the file gets to
	// this size in MB. 0: no limit.
	RecordMaxMB int `yaml:"record_max_mb"`

	// StatusFile is the path of the status file. The program writes the
	// status of all links to it each second. Empty: no status file.
	StatusFile string `yaml:"status_file"`

	// StatusHistory keeps the samples of the last 15 minutes of each link
	// in memory, for the control command "history" (graphs of a user
	// interface). It needs a status file. Off: no memory for the history.
	StatusHistory bool `yaml:"status_history"`

	// ControlSocket is the path of the Unix socket for the commands pause
	// and resume. Empty: no control socket.
	ControlSocket string `yaml:"control_socket"`

	// unknownKeys are the keys in the config file that the program does not
	// know (for example, a typing error). LoadConfig sets them.
	unknownKeys []string
	// reflectorsDefault is true if the config sets no top-level reflectors
	// and LoadConfig used the default list for the top-level ping_method.
	reflectorsDefault bool
}

// ForLink returns a copy of the config in which the link overrides of
// pinger_count, ping_interval_ms, ping_method, learn_cross_delay, mode,
// refractory_on_decrease, increase_to_delay_thr and auto_max_rate
// apply. The controller of a link uses this copy.
func (c *Config) ForLink(l *LinkConfig) *Config {
	cp := *c
	if l.PingerCount != nil {
		cp.PingerCount = *l.PingerCount
	}
	if l.PingIntervalMs != nil {
		cp.PingIntervalMs = *l.PingIntervalMs
	}
	if l.PingMethod != nil {
		cp.PingMethod = *l.PingMethod
	}
	if l.LearnCrossDelay != nil {
		cp.LearnCrossDelay = *l.LearnCrossDelay
	}
	if l.Mode != nil {
		cp.Mode = *l.Mode
	}
	if l.RefractoryOnDecrease != nil {
		cp.RefractoryOnDecrease = *l.RefractoryOnDecrease
	}
	if l.IncreaseToDelayThr != nil {
		cp.IncreaseToDelayThr = *l.IncreaseToDelayThr
	}
	if l.AutoMaxRate != nil {
		cp.AutoMaxRate = *l.AutoMaxRate
	}
	return &cp
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
	"lon-gb-ping.vultr.com",
	"waw-pl-ping.vultr.com",
	"ping.online.net",
}

// accessTimestampReflectors are in the network of the access provider. The
// calibration probe tests them. 100.64.0.1 is the carrier NAT gateway of
// Starlink at the point of presence: its delay is mostly the delay of the
// satellite link. It is one host, thus the proposal uses it only together
// with other reflectors.
var accessTimestampReflectors = []string{
	"100.64.0.1",
}

// worldTimestampReflectors reply to ICMP timestamp requests in other
// regions. They are not in the default list: the program selects the
// active reflectors at random, and a far reflector has a long RTT. The
// calibration probe tests them, and its proposal has the reflectors with
// the lowest jitter.
var worldTimestampReflectors = []string{
	"sto-se-ping.vultr.com",
	"mad-es-ping.vultr.com",
	"nj-us-ping.vultr.com",
	"il-us-ping.vultr.com",
	"tx-us-ping.vultr.com",
	"wa-us-ping.vultr.com",
	"ash-speed.hetzner.com",
	"hil-speed.hetzner.com",
	"sgp-ping.vultr.com",
	"sin-speed.hetzner.com",
	"hnd-jp-ping.vultr.com",
	"syd-au-ping.vultr.com",
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

		MonitorIntervalMs:             200,
		BufferbloatRefractoryPeriodMs: 300,
		DecayRefractoryPeriodMs:       1000,
		StallRecoveryMs:               500,
		AutoMaxRateLimitFactor:        3,

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

		RecordMaxMB: 100,
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
		cfg.reflectorsDefault = true
	}

	// Migrate legacy single-link config to links list
	cfg.migrateLinks()
	cfg.setDefaultModes()

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
				// The default list depends on the ping method of the link.
				if c.reflectorsDefault && c.Links[i].PingMethod != nil {
					c.Links[i].Reflectors = defaultReflectors(*c.Links[i].PingMethod)
				}
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
	if c.StatusHistory && c.StatusFile == "" {
		w = append(w, "status_history needs status_file: there is no history")
	}
	for _, link := range c.Links {
		if c.ForLink(&link).PingMethod == PingMethodTimestamp {
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

// controlNameRe matches the link names that the control socket can address.
// The longest command with such a name is much shorter than maxControlLine.
var controlNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// maxControlSocketPath is the maximum length of control_socket in bytes.
// Linux accepts a Unix socket path of not more than 107 bytes. The program
// creates the socket first in a temporary directory next to the path (see
// listenPrivate). That path can be 15 bytes longer than the parent
// directory plus the socket name. This limit keeps all such paths short
// enough.
const maxControlSocketPath = 90

// validateIfname checks that name is a valid Linux network interface name.
func validateIfname(name string) error {
	if !ifnameRe.MatchString(name) {
		return fmt.Errorf("invalid interface name %q: must be 1-15 chars of [a-zA-Z0-9._-], starting with alphanumeric", name)
	}
	return nil
}

// Validate checks that the configuration values are sensible.
func (c *Config) Validate() error {
	if len(c.Links) == 0 {
		return fmt.Errorf("at least one link must be configured")
	}

	if c.RecordDir != "" && !filepath.IsAbs(c.RecordDir) {
		return fmt.Errorf("record_dir %q must be an absolute path", c.RecordDir)
	}
	if c.RecordMaxMB < 0 {
		return fmt.Errorf("record_max_mb must not be negative")
	}

	if len(c.ControlSocket) > maxControlSocketPath {
		return fmt.Errorf("control_socket %q is longer than %d bytes", c.ControlSocket, maxControlSocketPath)
	}

	seen := make(map[string]bool)
	for _, link := range c.Links {
		if link.Name == "" {
			return fmt.Errorf("each link must have a name")
		}
		if seen[link.Name] {
			return fmt.Errorf("link name %q is used more than one time", link.Name)
		}
		seen[link.Name] = true
		if c.ControlSocket != "" && !controlNameRe.MatchString(link.Name) {
			return fmt.Errorf("link name %q: with control_socket, use 1 to 64 characters of [A-Za-z0-9_.-]", link.Name)
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
		}
		if len(link.Reflectors) == 0 {
			return fmt.Errorf("link %q: at least one reflector must be configured", link.Name)
		}
		lc := c.ForLink(&link)
		if lc.PingerCount <= 0 {
			return fmt.Errorf("link %q: pinger_count must be positive", link.Name)
		}
		if lc.PingIntervalMs <= 0 {
			return fmt.Errorf("link %q: ping_interval_ms must be positive", link.Name)
		}
		if link.Mode != nil {
			if _, err := parseLinkMode(*link.Mode); err != nil {
				return fmt.Errorf("link %q: mode: %w", link.Name, err)
			}
		}
		if lc.PingMethod != PingMethodEcho && lc.PingMethod != PingMethodTimestamp {
			return fmt.Errorf("link %q: ping_method must be %q or %q", link.Name, PingMethodEcho, PingMethodTimestamp)
		}
		if lc.PingMethod == PingMethodTimestamp && !slices.ContainsFunc(link.Reflectors, func(r string) bool { return !noTimestampReply(r) }) {
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
	if c.StallRecoveryMs < 0 {
		return fmt.Errorf("stall_recovery_ms must be 0 (off) or positive")
	}
	if !(c.AutoMaxRateLimitFactor >= 1) || math.IsInf(c.AutoMaxRateLimitFactor, 1) {
		return fmt.Errorf("auto_max_rate_limit_factor must be a finite number, 1 or more")
	}
	if c.Mode != "" {
		if _, err := parseLinkMode(c.Mode); err != nil {
			return fmt.Errorf("mode: %w", err)
		}
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
