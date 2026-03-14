package workspace

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/doITmagic/rag-code-mcp/internal/config"
)

func newManagerWithLanguages(langs []string) *Manager {
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{
			IndexLanguages: langs,
		},
	}
	return &Manager{config: cfg}
}

func TestIsLanguageEnabled_EmptyConfig(t *testing.T) {
	m := newManagerWithLanguages(nil)
	for _, lang := range []string{"go", "php", "python", "html"} {
		if !m.isLanguageEnabled(lang) {
			t.Errorf("expected %q enabled when IndexLanguages is empty", lang)
		}
	}
}

func TestIsLanguageEnabled_FilteredConfig(t *testing.T) {
	m := newManagerWithLanguages([]string{"php"})

	if !m.isLanguageEnabled("php") {
		t.Error("expected php enabled")
	}
	if m.isLanguageEnabled("go") {
		t.Error("expected go disabled")
	}
	if m.isLanguageEnabled("python") {
		t.Error("expected python disabled")
	}
}

func TestIsLanguageEnabled_CaseInsensitive(t *testing.T) {
	m := newManagerWithLanguages([]string{"PHP", "Go"})

	if !m.isLanguageEnabled("php") {
		t.Error("expected php (lowercase) to match PHP config")
	}
	if !m.isLanguageEnabled("Go") {
		t.Error("expected Go to match Go config")
	}
	if !m.isLanguageEnabled("go") {
		t.Error("expected go (lowercase) to match Go config")
	}
}

func TestIsLanguageEnabled_MultipleLanguages(t *testing.T) {
	m := newManagerWithLanguages([]string{"php", "html"})

	if !m.isLanguageEnabled("php") {
		t.Error("expected php enabled")
	}
	if !m.isLanguageEnabled("html") {
		t.Error("expected html enabled")
	}
	if m.isLanguageEnabled("go") {
		t.Error("expected go disabled")
	}
	if m.isLanguageEnabled("python") {
		t.Error("expected python disabled")
	}
}

func TestGetIndexLanguages_Default(t *testing.T) {
	m := newManagerWithLanguages(nil)
	langs := m.GetIndexLanguages()

	if len(langs) == 0 {
		t.Fatal("expected non-empty default languages")
	}

	// Should contain all default languages
	langSet := make(map[string]bool)
	for _, l := range langs {
		langSet[l] = true
	}

	for _, expected := range []string{"go", "php", "python", "html"} {
		if !langSet[expected] {
			t.Errorf("expected %q in default languages, got %v", expected, langs)
		}
	}

	// Should not have duplicates (html maps from both .html and .htm)
	seen := make(map[string]bool)
	for _, l := range langs {
		if seen[l] {
			t.Errorf("duplicate language %q in GetIndexLanguages result", l)
		}
		seen[l] = true
	}
}

func TestGetIndexLanguages_Configured(t *testing.T) {
	m := newManagerWithLanguages([]string{"php", "python"})
	langs := m.GetIndexLanguages()

	if len(langs) != 2 {
		t.Fatalf("expected 2 languages, got %d: %v", len(langs), langs)
	}

	sort.Strings(langs)
	if langs[0] != "php" || langs[1] != "python" {
		t.Errorf("expected [php python], got %v", langs)
	}
}

func TestDefaultExtToLanguage_Mapping(t *testing.T) {
	tests := []struct {
		ext  string
		lang string
	}{
		{".go", "go"},
		{".php", "php"},
		{".py", "python"},
		{".html", "html"},
		{".htm", "html"},
	}

	for _, tt := range tests {
		lang, ok := defaultExtToLanguage[tt.ext]
		if !ok {
			t.Errorf("extension %q not found in defaultExtToLanguage", tt.ext)
			continue
		}
		if lang != tt.lang {
			t.Errorf("extension %q: expected language %q, got %q", tt.ext, tt.lang, lang)
		}
	}
}

func TestDefaultExtToLanguage_UnknownExtension(t *testing.T) {
	unknowns := []string{".rb", ".java", ".rs", ".ts", ".css", ".json"}
	for _, ext := range unknowns {
		if _, ok := defaultExtToLanguage[ext]; ok {
			t.Errorf("unexpected extension %q found in defaultExtToLanguage", ext)
		}
	}
}

// TestWalkDir_LanguageFilter creates a temp directory with mixed files
// and verifies that language filtering works correctly.
func TestWalkDir_LanguageFilter(t *testing.T) {
	tmpDir := t.TempDir()

	// Create files of different types
	files := map[string]string{
		"main.go":          "package main",
		"app.php":          "<?php echo 'hello';",
		"script.py":        "print('hello')",
		"index.html":       "<html></html>",
		"readme.md":        "# Readme",
		"style.css":        "body {}",
		"sub/handler.php":  "<?php class Handler {}",
		"sub/utils.go":     "package sub",
		"sub/template.htm": "<div></div>",
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

	tests := []struct {
		name          string
		indexLangs    []string
		wantLangs     []string // languages that should have files
		dontWantLangs []string // languages that should NOT have files
		wantDocs      bool     // should .md files always be collected
	}{
		{
			name:          "only php",
			indexLangs:    []string{"php"},
			wantLangs:     []string{"php"},
			dontWantLangs: []string{"go", "python", "html"},
			wantDocs:      true,
		},
		{
			name:          "php and html",
			indexLangs:    []string{"php", "html"},
			wantLangs:     []string{"php", "html"},
			dontWantLangs: []string{"go", "python"},
			wantDocs:      true,
		},
		{
			name:          "all languages (empty config)",
			indexLangs:    nil,
			wantLangs:     []string{"go", "php", "python", "html"},
			dontWantLangs: nil,
			wantDocs:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newManagerWithLanguages(tt.indexLangs)

			scan := &workspaceScan{
				LanguageDirs:  make(map[string][]string),
				LanguageFiles: make(map[string][]string),
				DocFiles:      make([]string, 0),
			}
			dirCache := make(map[string]map[string]struct{})

			err := m.walkDir(tmpDir, scan, dirCache, make(map[string]struct{}), nil, tmpDir)
			if err != nil {
				t.Fatalf("walkDir failed: %v", err)
			}

			for _, lang := range tt.wantLangs {
				if len(scan.LanguageFiles[lang]) == 0 {
					t.Errorf("expected files for language %q, got none", lang)
				}
			}

			for _, lang := range tt.dontWantLangs {
				if len(scan.LanguageFiles[lang]) > 0 {
					t.Errorf("expected no files for language %q, got %d", lang, len(scan.LanguageFiles[lang]))
				}
			}

			if tt.wantDocs && len(scan.DocFiles) == 0 {
				t.Error("expected .md files to be collected regardless of language filter")
			}
		})
	}
}

// TestWalkDir_PHPFileCount verifies correct file counting with PHP-only filter.
func TestWalkDir_PHPFileCount(t *testing.T) {
	tmpDir := t.TempDir()

	phpFiles := []string{"a.php", "b.php", "sub/c.php"}
	otherFiles := []string{"main.go", "script.py", "index.html"}

	for _, name := range append(phpFiles, otherFiles...) {
		path := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	m := newManagerWithLanguages([]string{"php"})
	scan := &workspaceScan{
		LanguageDirs:  make(map[string][]string),
		LanguageFiles: make(map[string][]string),
		DocFiles:      make([]string, 0),
	}
	dirCache := make(map[string]map[string]struct{})

	if err := m.walkDir(tmpDir, scan, dirCache, make(map[string]struct{}), nil, tmpDir); err != nil {
		t.Fatal(err)
	}

	if got := len(scan.LanguageFiles["php"]); got != 3 {
		t.Errorf("expected 3 PHP files, got %d", got)
	}

	// TotalFiles should count only PHP files (filtered languages)
	if scan.TotalFiles != 3 {
		t.Errorf("expected TotalFiles=3 (only PHP), got %d", scan.TotalFiles)
	}
}

// TestWalkDir_ExcludePatternsWithLanguageFilter tests that exclude patterns
// still work alongside language filtering.
func TestWalkDir_ExcludePatternsWithLanguageFilter(t *testing.T) {
	tmpDir := t.TempDir()

	files := []string{"app.php", "app.generated.php", "sub/handler.php"}
	for _, name := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<?php"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	m := newManagerWithLanguages([]string{"php"})
	scan := &workspaceScan{
		LanguageDirs:  make(map[string][]string),
		LanguageFiles: make(map[string][]string),
		DocFiles:      make([]string, 0),
	}
	dirCache := make(map[string]map[string]struct{})

	excludePatterns := []string{"*.generated.php"}
	if err := m.walkDir(tmpDir, scan, dirCache, make(map[string]struct{}), excludePatterns, tmpDir); err != nil {
		t.Fatal(err)
	}

	// Should have 2 PHP files (app.generated.php excluded)
	if got := len(scan.LanguageFiles["php"]); got != 2 {
		t.Errorf("expected 2 PHP files (1 excluded), got %d", got)
		for _, f := range scan.LanguageFiles["php"] {
			t.Logf("  found: %s", f)
		}
	}

	// Verify the excluded file is not present
	for _, f := range scan.LanguageFiles["php"] {
		if strings.Contains(f, "generated") {
			t.Errorf("excluded file should not be present: %s", f)
		}
	}
}
