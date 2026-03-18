package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig_IsValid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
}

func TestValidate_NoLinks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Links = nil
	cfg.Download.Interface = "" // prevent migration
	cfg.migrateLinks()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one link") {
		t.Errorf("expected 'at least one link' error, got: %v", err)
	}
}

func TestValidate_EmptyInterface(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links[0].Download.Interface = ""
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "interface must be set") {
		t.Errorf("expected interface error, got: %v", err)
	}
}

func TestValidate_MinRateZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links[0].Download.MinRateKbps = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "min_rate_kbps must be positive") {
		t.Errorf("expected min_rate error, got: %v", err)
	}
}

func TestValidate_BaseRateBelowMin(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links[0].Download.BaseRateKbps = 1
	cfg.Links[0].Download.MinRateKbps = 100
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "base_rate_kbps must be >= min_rate_kbps") {
		t.Errorf("expected base_rate error, got: %v", err)
	}
}

func TestValidate_MaxRateBelowBase(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links[0].Download.MaxRateKbps = 1
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "max_rate_kbps must be >= base_rate_kbps") {
		t.Errorf("expected max_rate error, got: %v", err)
	}
}

func TestValidate_NoReflectors(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.Links[0].Reflectors = nil
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reflector") {
		t.Errorf("expected reflector error, got: %v", err)
	}
}

func TestValidate_PingerCountZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.PingerCount = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "pinger_count") {
		t.Errorf("expected pinger_count error, got: %v", err)
	}
}

func TestValidate_BufferbloatDetectionWindowZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.BufferbloatDetectionWindow = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "bufferbloat_detection_window") {
		t.Errorf("expected bufferbloat_detection_window error, got: %v", err)
	}
}

func TestValidate_BufferbloatDetectionThrExceedsWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.BufferbloatDetectionThr = cfg.BufferbloatDetectionWindow + 1
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "bufferbloat_detection_thr") {
		t.Errorf("expected bufferbloat_detection_thr error, got: %v", err)
	}
}

func TestValidate_ReflectorMisbehavingWindowZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.ReflectorMisbehavingDetectionWindow = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reflector_misbehaving_detection_window") {
		t.Errorf("expected reflector_misbehaving_detection_window error, got: %v", err)
	}
}

func TestValidate_ReflectorMisbehavingThrExceedsWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.ReflectorMisbehavingDetectionThr = cfg.ReflectorMisbehavingDetectionWindow + 1
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reflector_misbehaving_detection_thr") {
		t.Errorf("expected reflector_misbehaving_detection_thr error, got: %v", err)
	}
}

func TestValidate_ReflectorMisbehavingThrZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.ReflectorMisbehavingDetectionThr = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reflector_misbehaving_detection_thr") {
		t.Errorf("expected reflector_misbehaving_detection_thr error, got: %v", err)
	}
}

func TestValidate_MonitorIntervalZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.MonitorIntervalMs = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "monitor_interval_ms") {
		t.Errorf("expected monitor_interval_ms error, got: %v", err)
	}
}

func TestValidate_PingIntervalZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.PingIntervalMs = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ping_interval_ms") {
		t.Errorf("expected ping_interval_ms error, got: %v", err)
	}
}

func TestValidate_ReflectorResponseDeadlineZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.ReflectorResponseDeadlineS = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "reflector_response_deadline_s") {
		t.Errorf("expected reflector_response_deadline_s error, got: %v", err)
	}
}

func TestMigrateLinks_LegacyFormat(t *testing.T) {
	cfg := DefaultConfig()
	// DefaultConfig has Download/Upload set but no Links
	cfg.Links = nil
	cfg.migrateLinks()

	if len(cfg.Links) != 1 {
		t.Fatalf("expected 1 link after migration, got %d", len(cfg.Links))
	}
	if cfg.Links[0].Name != "default" {
		t.Errorf("expected link name 'default', got %q", cfg.Links[0].Name)
	}
	if cfg.Links[0].Download.Interface != cfg.Download.Interface {
		t.Error("download interface not migrated")
	}
	if len(cfg.Links[0].Reflectors) != len(cfg.Reflectors) {
		t.Error("reflectors not migrated")
	}
}

func TestMigrateLinks_MultiLinkInheritsReflectors(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Links = []LinkConfig{
		{
			Name: "link0",
			Download: DirectionConfig{
				Interface:    "eth0",
				MinRateKbps:  1000,
				BaseRateKbps: 10000,
				MaxRateKbps:  50000,
			},
			Upload: DirectionConfig{
				Interface:    "eth1",
				MinRateKbps:  1000,
				BaseRateKbps: 10000,
				MaxRateKbps:  50000,
			},
			// No reflectors — should inherit from top-level
		},
	}
	cfg.migrateLinks()

	if len(cfg.Links[0].Reflectors) != len(cfg.Reflectors) {
		t.Errorf("expected inherited reflectors (len=%d), got %d",
			len(cfg.Reflectors), len(cfg.Links[0].Reflectors))
	}
}

func TestMigrateLinks_AutoNameLinks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Links = []LinkConfig{
		{
			Download: DirectionConfig{Interface: "eth0", MinRateKbps: 1000, BaseRateKbps: 10000, MaxRateKbps: 50000},
			Upload:   DirectionConfig{Interface: "eth1", MinRateKbps: 1000, BaseRateKbps: 10000, MaxRateKbps: 50000},
		},
		{
			Download: DirectionConfig{Interface: "eth2", MinRateKbps: 1000, BaseRateKbps: 10000, MaxRateKbps: 50000},
			Upload:   DirectionConfig{Interface: "eth3", MinRateKbps: 1000, BaseRateKbps: 10000, MaxRateKbps: 50000},
		},
	}
	cfg.migrateLinks()

	if cfg.Links[0].Name != "link0" || cfg.Links[1].Name != "link1" {
		t.Errorf("expected auto-names link0/link1, got %q/%q", cfg.Links[0].Name, cfg.Links[1].Name)
	}
}

func TestLoadConfig_ValidFile(t *testing.T) {
	content := `
links:
  - name: wan
    download:
      interface: ifb-wan
      min_rate_kbps: 5000
      base_rate_kbps: 20000
      max_rate_kbps: 80000
    upload:
      interface: wan
      min_rate_kbps: 5000
      base_rate_kbps: 20000
      max_rate_kbps: 35000
    reflectors:
      - 1.1.1.1
      - 8.8.8.8
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(cfg.Links) != 1 || cfg.Links[0].Name != "wan" {
		t.Errorf("unexpected links: %+v", cfg.Links)
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// --- Float tuning parameter validation tests ---

func TestValidate_HighLoadThrOutOfRange(t *testing.T) {
	for _, val := range []float64{-0.1, 1.1} {
		cfg := DefaultConfig()
		cfg.migrateLinks()
		cfg.HighLoadThr = val
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected error for high_load_thr=%v", val)
		}
	}
}

func TestValidate_AlphaBaselineIncreaseOutOfRange(t *testing.T) {
	for _, val := range []float64{0, -0.1, 1.1} {
		cfg := DefaultConfig()
		cfg.migrateLinks()
		cfg.AlphaBaselineIncrease = val
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected error for alpha_baseline_increase=%v", val)
		}
	}
}

func TestValidate_AlphaBaselineDecreaseOutOfRange(t *testing.T) {
	for _, val := range []float64{0, -0.1, 1.1} {
		cfg := DefaultConfig()
		cfg.migrateLinks()
		cfg.AlphaBaselineDecrease = val
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected error for alpha_baseline_decrease=%v", val)
		}
	}
}

func TestValidate_AlphaDeltaEWMAOutOfRange(t *testing.T) {
	for _, val := range []float64{0, 1.1} {
		cfg := DefaultConfig()
		cfg.migrateLinks()
		cfg.AlphaDeltaEWMA = val
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected error for alpha_delta_ewma=%v", val)
		}
	}
}

func TestValidate_ShaperRateAdjustDownOrdering(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	// max > min is invalid (max should be the more aggressive reduction)
	cfg.ShaperRateMaxAdjustDownBufferbloat = 0.99
	cfg.ShaperRateMinAdjustDownBufferbloat = 0.75
	if err := cfg.Validate(); err == nil {
		t.Error("expected error when max_adjust_down > min_adjust_down")
	}
}

func TestValidate_ShaperRateAdjustUpOrdering(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.ShaperRateMinAdjustUpLoadHigh = 1.05
	cfg.ShaperRateMaxAdjustUpLoadHigh = 1.01 // less than min
	if err := cfg.Validate(); err == nil {
		t.Error("expected error when max_adjust_up < min_adjust_up")
	}
}

func TestValidate_ShaperRateAdjustDownLoadLowOutOfRange(t *testing.T) {
	for _, val := range []float64{0, 1.1} {
		cfg := DefaultConfig()
		cfg.migrateLinks()
		cfg.ShaperRateAdjustDownLoadLow = val
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected error for shaper_rate_adjust_down_load_low=%v", val)
		}
	}
}

func TestValidate_ShaperRateAdjustUpLoadLowTooLow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.ShaperRateAdjustUpLoadLow = 0.9
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for shaper_rate_adjust_up_load_low < 1")
	}
}

func TestValidate_FloatParamsAtBoundaries(t *testing.T) {
	// All at valid boundaries — should pass
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.HighLoadThr = 0.0
	cfg.AlphaBaselineIncrease = 1.0
	cfg.AlphaBaselineDecrease = 1.0
	cfg.AlphaDeltaEWMA = 1.0
	cfg.ShaperRateMinAdjustDownBufferbloat = 1.0
	cfg.ShaperRateMaxAdjustDownBufferbloat = 1.0
	cfg.ShaperRateMinAdjustUpLoadHigh = 1.0
	cfg.ShaperRateMaxAdjustUpLoadHigh = 1.0
	cfg.ShaperRateAdjustDownLoadLow = 1.0
	cfg.ShaperRateAdjustUpLoadLow = 1.0
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid boundary values should pass: %v", err)
	}
}
