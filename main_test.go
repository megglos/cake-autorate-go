package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReportConfigCheck(t *testing.T) {
	cfg := DefaultConfig()
	cfg.migrateLinks()
	cfg.unknownKeys = []string{"unknown config key \"x\" (line 1) has no effect"}

	var out, errOut bytes.Buffer
	if code := reportConfigCheck(&out, &errOut, cfg, false); code != 0 {
		t.Errorf("a warning must not fail without strict, got code %d", code)
	}
	if !strings.Contains(errOut.String(), "warning: unknown config key") || !strings.Contains(out.String(), "1 link(s)") {
		t.Errorf("unexpected output: %q %q", out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := reportConfigCheck(&out, &errOut, cfg, true); code != 1 {
		t.Errorf("a warning must fail with strict, got code %d", code)
	}
	if out.Len() != 0 {
		t.Errorf("no success message expected with strict warnings: %q", out.String())
	}

	cfg.unknownKeys = nil
	if code := reportConfigCheck(&out, &errOut, cfg, true); code != 0 {
		t.Errorf("a config without warnings must pass with strict, got code %d", code)
	}
}
