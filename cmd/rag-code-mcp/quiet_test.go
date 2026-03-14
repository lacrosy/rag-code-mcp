package main

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

func TestSimpleLogger_QuietSuppressesStderr(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	l := &simpleLogger{quiet: true}
	l.Info("test info %s", "message")
	l.Error("test error %s", "message")
	l.Warn("test warn %s", "message")

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if output != "" {
		t.Errorf("expected no stderr output in quiet mode, got: %q", output)
	}
}

func TestSimpleLogger_NonQuietWritesStderr(t *testing.T) {
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	l := &simpleLogger{quiet: false}
	l.Info("hello %s", "world")

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if output == "" {
		t.Error("expected stderr output in non-quiet mode, got nothing")
	}
	if !bytes.Contains(buf.Bytes(), []byte("[INFO]")) {
		t.Errorf("expected [INFO] prefix, got: %q", output)
	}
}

func TestSimpleLogger_QuietWritesToLogFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "quiet-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	l := &simpleLogger{
		logFile: tmpFile,
		quiet:   true,
	}

	l.Info("file message %d", 42)
	l.Error("file error %s", "oops")
	tmpFile.Sync()

	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(content, []byte("[INFO] file message 42")) {
		t.Errorf("log file missing INFO message, got: %q", string(content))
	}
	if !bytes.Contains(content, []byte("[ERROR] file error oops")) {
		t.Errorf("log file missing ERROR message, got: %q", string(content))
	}
}

func TestStderrf_QuietMode(t *testing.T) {
	// Save and restore global logger
	oldLogger := logger
	defer func() { logger = oldLogger }()

	tmpFile, err := os.CreateTemp("", "stderrf-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	logger = &simpleLogger{
		logFile: tmpFile,
		quiet:   true,
	}

	// Capture stderr
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	stderrf("test message %d\n", 123)

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)

	// Stderr should be empty in quiet mode
	if buf.String() != "" {
		t.Errorf("expected no stderr in quiet mode, got: %q", buf.String())
	}

	// Log file should have the message
	tmpFile.Sync()
	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("test message 123")) {
		t.Errorf("log file missing stderrf message, got: %q", string(content))
	}
}

func TestStderrf_NonQuietMode(t *testing.T) {
	oldLogger := logger
	defer func() { logger = oldLogger }()

	logger = &simpleLogger{quiet: false}

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	stderrf("visible message\n")

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)

	if !bytes.Contains(buf.Bytes(), []byte("visible message")) {
		t.Errorf("expected stderr output in non-quiet mode, got: %q", buf.String())
	}
}

func TestSimpleLogger_ShouldLog(t *testing.T) {
	l := &simpleLogger{}

	tests := []struct {
		envLevel  string
		msgLevel  string
		shouldLog bool
	}{
		{"info", "info", true},
		{"info", "warn", true},
		{"info", "error", true},
		{"info", "debug", false},
		{"debug", "debug", true},
		{"debug", "info", true},
		{"warn", "info", false},
		{"warn", "warn", true},
		{"error", "warn", false},
		{"error", "error", true},
		{"", "info", true},  // default level is info
		{"", "debug", false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.envLevel, tt.msgLevel), func(t *testing.T) {
			if tt.envLevel != "" {
				t.Setenv("MCP_LOG_LEVEL", tt.envLevel)
			} else {
				t.Setenv("MCP_LOG_LEVEL", "")
			}
			got := l.shouldLog(tt.msgLevel)
			if got != tt.shouldLog {
				t.Errorf("shouldLog(%q) with MCP_LOG_LEVEL=%q: got %v, want %v",
					tt.msgLevel, tt.envLevel, got, tt.shouldLog)
			}
		})
	}
}
