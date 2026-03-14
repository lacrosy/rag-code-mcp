package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/doITmagic/rag-code-mcp/internal/config"
)

// TestScanWorkspace_IndexIncludeResolvesFromRoot verifies that scanWorkspace
// resolves IndexInclude directories relative to info.Root (the workspace root),
// not relative to each subdirectory. This was the root cause of the empty index bug.
func TestScanWorkspace_IndexIncludeResolvesFromRoot(t *testing.T) {
	// Create project structure:
	//   wsRoot/
	//     src/App.php
	//     config/services.php
	//     tests/AppTest.php
	//     vendor/lib.php  (should be excluded)
	tmpDir := t.TempDir()

	dirs := []string{"src", "config", "tests", "vendor"}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(tmpDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	files := map[string]string{
		"src/App.php":        "<?php class App {}",
		"config/services.php": "<?php return [];",
		"tests/AppTest.php":  "<?php class AppTest {}",
		"vendor/lib.php":     "<?php class Lib {}",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{
			IndexInclude:   []string{"src", "config", "tests"},
			IndexExclude:   []string{"vendor"},
			IndexLanguages: []string{"php"},
		},
	}
	m := &Manager{config: cfg}

	info := &Info{Root: tmpDir}
	scan, err := m.scanWorkspace(info)
	if err != nil {
		t.Fatalf("scanWorkspace failed: %v", err)
	}

	phpFiles := scan.LanguageFiles["php"]
	if len(phpFiles) != 3 {
		t.Errorf("expected 3 PHP files (src, config, tests), got %d", len(phpFiles))
		for _, f := range phpFiles {
			t.Logf("  found: %s", f)
		}
	}

	// Verify vendor file is NOT included (it's not in IndexInclude)
	for _, f := range phpFiles {
		if filepath.Base(filepath.Dir(f)) == "vendor" {
			t.Errorf("vendor file should not be included: %s", f)
		}
	}
}

// TestScanWorkspace_SubdirRootBug reproduces the original bug where info.Root
// was set to a subdirectory (e.g., "pspi/src") and scanWorkspace tried to
// resolve IndexInclude relative to it (creating "pspi/src/src" which doesn't exist).
func TestScanWorkspace_SubdirRootBug(t *testing.T) {
	tmpDir := t.TempDir()

	// Create src/App.php
	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "App.php"), []byte("<?php"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{
			IndexInclude:   []string{"src"},
			IndexLanguages: []string{"php"},
		},
	}
	m := &Manager{config: cfg}

	// BUG scenario: info.Root = tmpDir/src (subdirectory, not workspace root)
	// scanWorkspace will try to find tmpDir/src/src — doesn't exist — finds nothing
	infoBad := &Info{Root: srcDir}
	scanBad, err := m.scanWorkspace(infoBad)
	if err != nil {
		t.Fatalf("scanWorkspace failed: %v", err)
	}
	badCount := len(scanBad.LanguageFiles["php"])

	// FIXED: info.Root = tmpDir (workspace root)
	// scanWorkspace resolves tmpDir/src — exists — finds files
	infoGood := &Info{Root: tmpDir}
	scanGood, err := m.scanWorkspace(infoGood)
	if err != nil {
		t.Fatalf("scanWorkspace failed: %v", err)
	}
	goodCount := len(scanGood.LanguageFiles["php"])

	if badCount != 0 {
		t.Errorf("expected 0 files with subdir root (bug scenario), got %d", badCount)
	}
	if goodCount != 1 {
		t.Errorf("expected 1 file with correct workspace root, got %d", goodCount)
	}
}

// TestScanWorkspace_NoIndexInclude verifies that when IndexInclude is empty,
// scanWorkspace scans the entire info.Root directory.
func TestScanWorkspace_NoIndexInclude(t *testing.T) {
	tmpDir := t.TempDir()

	files := []string{"App.php", "sub/Handler.php"}
	for _, name := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<?php"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{
			IndexLanguages: []string{"php"},
		},
	}
	m := &Manager{config: cfg}

	info := &Info{Root: tmpDir}
	scan, err := m.scanWorkspace(info)
	if err != nil {
		t.Fatalf("scanWorkspace failed: %v", err)
	}

	if got := len(scan.LanguageFiles["php"]); got != 2 {
		t.Errorf("expected 2 PHP files (full scan), got %d", got)
	}
}

// TestScanWorkspace_MarkdownAlwaysCollected verifies that .md files are collected
// regardless of the IndexLanguages filter.
func TestScanWorkspace_MarkdownAlwaysCollected(t *testing.T) {
	tmpDir := t.TempDir()

	files := map[string]string{
		"README.md": "# Hello",
		"docs/guide.md": "# Guide",
		"src/App.php": "<?php",
		"main.go": "package main",
	}
	for name, content := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Only PHP language enabled
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{
			IndexLanguages: []string{"php"},
		},
	}
	m := &Manager{config: cfg}

	info := &Info{Root: tmpDir}
	scan, err := m.scanWorkspace(info)
	if err != nil {
		t.Fatal(err)
	}

	// Go files should NOT be collected
	if len(scan.LanguageFiles["go"]) > 0 {
		t.Error("go files should not be collected when only php is enabled")
	}

	// PHP files SHOULD be collected
	if len(scan.LanguageFiles["php"]) != 1 {
		t.Errorf("expected 1 PHP file, got %d", len(scan.LanguageFiles["php"]))
	}

	// Markdown files SHOULD always be collected
	if len(scan.DocFiles) != 2 {
		t.Errorf("expected 2 markdown files regardless of language filter, got %d", len(scan.DocFiles))
	}
}
