// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
)

// LogLevel represents the severity of a log message.
type LogLevel int

const (
	LogDebug LogLevel = iota
	LogInfo
	LogError
)

// Logger provides leveled logging to file and stderr.
type Logger struct {
	mu       sync.Mutex
	debugOn  atomic.Bool // lock-free check for debug fast path
	file     *os.File
	logger   *log.Logger
	maxBytes int64
}

// NewLogger creates a new Logger. If filePath is non-empty, logs are also
// written to that file. maxSizeKB controls log file rotation (0 = no limit).
func NewLogger(debug bool, filePath string, maxSizeKB int) (*Logger, error) {
	return newLogger(debug, filePath, maxSizeKB, os.Stderr)
}

// NewDiscardLogger creates a logger that discards all output.
// Useful for tests that don't want log noise on stderr.
func NewDiscardLogger() *Logger {
	l, _ := newLogger(false, "", 0, io.Discard)
	return l
}

func newLogger(debug bool, filePath string, maxSizeKB int, stderr io.Writer) (*Logger, error) {
	l := &Logger{
		maxBytes: int64(maxSizeKB) * 1024,
	}
	l.debugOn.Store(debug)

	writers := []io.Writer{stderr}

	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("opening log file: %w", err)
		}
		l.file = f
		writers = append(writers, f)
	}

	l.logger = log.New(io.MultiWriter(writers...), "", log.LstdFlags|log.Lmicroseconds)
	return l, nil
}

// Close closes the log file if open.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
	}
}

// Debugf logs a debug message (only if debug mode is enabled).
// Uses an atomic check to avoid mutex contention when debug is disabled.
func (l *Logger) Debugf(format string, args ...interface{}) {
	if !l.debugOn.Load() {
		return
	}
	l.logf("DEBUG", format, args...)
}

// Infof logs an informational message.
func (l *Logger) Infof(format string, args ...interface{}) {
	l.logf("INFO", format, args...)
}

// Errorf logs an error message.
func (l *Logger) Errorf(format string, args ...interface{}) {
	l.logf("ERROR", format, args...)
}

func (l *Logger) logf(level, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	msg := fmt.Sprintf(format, args...)
	l.logger.Printf("[%s] %s", level, msg)

	l.maybeRotate()
}

func (l *Logger) maybeRotate() {
	if l.file == nil || l.maxBytes <= 0 {
		return
	}
	info, err := l.file.Stat()
	if err != nil {
		return
	}
	if info.Size() < l.maxBytes {
		return
	}

	// Rotate: truncate the file
	if err := l.file.Truncate(0); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] log rotation truncate failed: %v\n", err)
		return
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] log rotation seek failed: %v\n", err)
		return
	}
	l.logger.Printf("[INFO] log file rotated (exceeded %d KB)", l.maxBytes/1024)
}
