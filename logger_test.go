package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLogger_StderrOnly(t *testing.T) {
	l, err := NewLogger(false, "", 0)
	if err != nil {
		t.Fatalf("NewLogger stderr-only failed: %v", err)
	}
	defer l.Close()

	if l.file != nil {
		t.Error("expected no file when path is empty")
	}
}

func TestNewLogger_WithFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	l, err := NewLogger(false, path, 0)
	if err != nil {
		t.Fatalf("NewLogger with file failed: %v", err)
	}
	defer l.Close()

	if l.file == nil {
		t.Fatal("expected file to be opened")
	}

	l.Infof("hello %s", "world")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Errorf("log file should contain 'hello world', got: %s", string(data))
	}
	if !strings.Contains(string(data), "[INFO]") {
		t.Errorf("log file should contain [INFO] prefix, got: %s", string(data))
	}
}

func TestLogger_DebugSuppressed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	l, err := NewLogger(false, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Debugf("should not appear")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "should not appear") {
		t.Error("debug message should be suppressed when debug=false")
	}
}

func TestLogger_DebugEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	l, err := NewLogger(true, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Debugf("visible debug %d", 42)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "visible debug 42") {
		t.Errorf("debug message should appear when debug=true, got: %s", string(data))
	}
	if !strings.Contains(string(data), "[DEBUG]") {
		t.Errorf("should have [DEBUG] prefix, got: %s", string(data))
	}
}

func TestLogger_Errorf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	l, err := NewLogger(false, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Errorf("something went %s", "wrong")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[ERROR]") {
		t.Errorf("should have [ERROR] prefix, got: %s", string(data))
	}
	if !strings.Contains(string(data), "something went wrong") {
		t.Errorf("should contain error message, got: %s", string(data))
	}
}

func TestLogger_Rotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	// maxSizeKB = 1 → rotates at 1024 bytes
	l, err := NewLogger(false, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Write enough to exceed 1KB
	for i := 0; i < 50; i++ {
		l.Infof("log line number %d with some padding to fill up space quickly", i)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// After rotation, file should be smaller than 1KB + one rotation message
	if info.Size() > 2048 {
		t.Errorf("log file should have been rotated (size=%d, expected <2048)", info.Size())
	}
}

func TestLogger_NoRotationWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	// maxSizeKB = 0 → no rotation
	l, err := NewLogger(false, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 0; i < 50; i++ {
		l.Infof("log line number %d with some padding to fill up space quickly", i)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Without rotation, file grows freely
	if info.Size() < 1024 {
		t.Errorf("log file should be >1KB without rotation, got %d", info.Size())
	}
}

func TestNewLogger_InvalidPath(t *testing.T) {
	_, err := NewLogger(false, "/nonexistent/dir/test.log", 0)
	if err == nil {
		t.Error("expected error for invalid log file path")
	}
}

func TestLogger_CloseIdempotent(t *testing.T) {
	l, err := NewLogger(false, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	l.Close() // should not panic
}
