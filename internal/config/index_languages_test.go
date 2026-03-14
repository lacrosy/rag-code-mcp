package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesIndexLanguages(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.yaml")

	yaml := []byte(`
llm:
  provider: ollama
  ollama_model: phi3:medium
  ollama_embed: mxbai-embed-large
workspace:
  index_languages:
    - php
    - html
`)
	if err := os.WriteFile(path, yaml, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	langs := cfg.Workspace.IndexLanguages
	if len(langs) != 2 {
		t.Fatalf("expected 2 index_languages, got %d: %v", len(langs), langs)
	}
	if langs[0] != "php" || langs[1] != "html" {
		t.Errorf("expected [php html], got %v", langs)
	}
}

func TestLoadEmptyIndexLanguages(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.yaml")

	yaml := []byte(`
llm:
  provider: ollama
  ollama_model: phi3:medium
workspace:
  enabled: true
`)
	if err := os.WriteFile(path, yaml, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Workspace.IndexLanguages) != 0 {
		t.Errorf("expected empty IndexLanguages, got %v", cfg.Workspace.IndexLanguages)
	}
}

func TestLoadSingleIndexLanguage(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.yaml")

	yaml := []byte(`
llm:
  provider: ollama
  ollama_model: phi3:medium
workspace:
  index_languages:
    - php
`)
	if err := os.WriteFile(path, yaml, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	langs := cfg.Workspace.IndexLanguages
	if len(langs) != 1 || langs[0] != "php" {
		t.Errorf("expected [php], got %v", langs)
	}
}
