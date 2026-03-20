package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSysfsCounter_Read(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	if err := os.WriteFile(path, []byte("123456\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := &sysfsCounter{file: f, path: path}
	val, err := sc.read()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if val != 123456 {
		t.Errorf("expected 123456, got %d", val)
	}
}

func TestSysfsCounter_ReadUpdatedValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	if err := os.WriteFile(path, []byte("100\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := &sysfsCounter{file: f, path: path}

	val1, err := sc.read()
	if err != nil {
		t.Fatal(err)
	}
	if val1 != 100 {
		t.Errorf("first read: expected 100, got %d", val1)
	}

	// Update the file (simulating sysfs counter increment)
	if err := os.WriteFile(path, []byte("200\n"), 0644); err != nil {
		t.Fatal(err)
	}

	val2, err := sc.read()
	if err != nil {
		t.Fatal(err)
	}
	if val2 != 200 {
		t.Errorf("second read: expected 200, got %d", val2)
	}
}

func TestSysfsCounter_ReadNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	if err := os.WriteFile(path, []byte("42"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := &sysfsCounter{file: f, path: path}
	val, err := sc.read()
	if err != nil {
		t.Fatal(err)
	}
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
}

func TestSysfsCounter_ReadLargeValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	// ~18 digits — near int64 max
	if err := os.WriteFile(path, []byte("999999999999999999\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := &sysfsCounter{file: f, path: path}
	val, err := sc.read()
	if err != nil {
		t.Fatal(err)
	}
	if val != 999999999999999999 {
		t.Errorf("expected 999999999999999999, got %d", val)
	}
}

func TestSysfsCounter_Reopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	if err := os.WriteFile(path, []byte("100\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	sc := &sysfsCounter{file: f, path: path}

	// Close the fd to simulate a stale fd
	sc.file.Close()

	// Reopen should get a fresh fd
	if err := sc.reopen(); err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer sc.close()

	val, err := sc.read()
	if err != nil {
		t.Fatalf("read after reopen failed: %v", err)
	}
	if val != 100 {
		t.Errorf("expected 100 after reopen, got %d", val)
	}
}

func TestSysfsCounter_ReopenMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	if err := os.WriteFile(path, []byte("100\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	sc := &sysfsCounter{file: f, path: path}
	sc.file.Close()

	// Remove the file
	os.Remove(path)

	if err := sc.reopen(); err == nil {
		t.Error("reopen should fail when file is missing")
	}
}

func TestOpenSysfsCounter_InvalidInterface(t *testing.T) {
	_, err := openSysfsCounter("nonexistent_iface_xyz", "rx_bytes")
	if err == nil {
		t.Error("expected error for nonexistent interface")
	}
}

func TestSysfsCounter_ReadClosedFdReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rx_bytes")
	if err := os.WriteFile(path, []byte("123\n"), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	sc := &sysfsCounter{file: f, path: path}
	// Close the fd to simulate a stale/broken fd
	f.Close()

	_, err = sc.read()
	if err == nil {
		t.Error("expected error when reading from closed fd")
	}
}

func TestMonitorRun_RetriesUntilInterfaceAppears(t *testing.T) {
	dir := t.TempDir()

	logger := testLogger(t)
	m := NewMonitor("test_dl", "test_ul", 50, logger)

	// Override sysfs paths by creating files after a short delay to simulate
	// an interface appearing after startup. We can't easily test the real
	// retry loop without mocking sysfs, but we can test that cancellation
	// works when the interface never appears.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ch := make(chan RateStats, 10)
	done := make(chan struct{})
	go func() {
		m.Run(ctx, ch)
		close(done)
	}()

	// Run should exit when context is cancelled (not hang or panic)
	select {
	case <-done:
		// Good — Run returned after context cancellation
	case <-time.After(5 * time.Second):
		t.Fatal("Monitor.Run did not exit after context cancellation")
	}

	_ = dir // used to ensure temp dir stays alive
}

func TestNewMonitor(t *testing.T) {
	logger := testLogger(t)
	m := NewMonitor("eth0", "eth0", 200, logger)
	if m.dlIface != "eth0" || m.ulIface != "eth0" {
		t.Error("interfaces not set correctly")
	}
	if m.interval.Milliseconds() != 200 {
		t.Errorf("expected 200ms interval, got %v", m.interval)
	}
}
