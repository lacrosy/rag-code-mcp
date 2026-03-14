package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads and parses the configuration file
func Load(path string) (*Config, error) {
	// Read configuration file
	data, err := os.ReadFile(path)
	if err != nil {
		// Return default config if file doesn't exist
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Auto-migrate file-based config BEFORE env overrides to see if we should save
	if migrateEmbeddingModel(&cfg) {
		log.Printf("🔄 Auto-migrating configuration file '%s' to stable embedding models...", path)
		if err := Save(path, &cfg); err != nil {
			log.Printf("⚠️  Failed to persist migrated configuration to '%s': %v", path, err)
		} else {
			log.Printf("✅ Configuration file successfully updated.")
		}
	}

	// Apply environment variable overrides
	applyEnvOverrides(&cfg)

	// Auto-migrate again after env overrides (in memory) to ensure any deprecated models
	// from environment variables are also updated
	migrateEmbeddingModel(&cfg)

	// Validate configuration
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Store the config file directory for resolving relative paths
	absPath, err := filepath.Abs(path)
	if err == nil {
		cfg.ConfigDir = filepath.Dir(absPath)
	}

	return &cfg, nil
}

// Save writes the configuration to a file
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		LLM: LLMConfig{
			Provider:         "ollama",
			OllamaBaseURL:    "http://localhost:11434",
			OllamaModel:      "phi3:medium",
			OllamaEmbed:      "mxbai-embed-large",
			LlamafileBaseURL: "http://localhost:8080",
			Temperature:      0.7,
			MaxTokens:        2048,
			Timeout:          60 * time.Second,
			MaxRetries:       3,
			// Legacy fields for backward compatibility
			BaseURL:    "http://localhost:11434",
			Model:      "phi3:medium",
			EmbedModel: "mxbai-embed-large",
		},
		Memory: MemoryConfig{
			ShortTermSize:  10,
			EnableLongTerm: false,
		},
		Storage: StorageConfig{
			VectorDB: VectorDBConfig{
				Provider:   "qdrant",
				URL:        "http://localhost:6333",
				Collection: "do-ai",
			},
			Redis: RedisConfig{
				Enabled: false,
				URL:     "localhost:6379",
				DB:      0,
			},
			SQLite: SQLiteConfig{
				Path: "./data/do-ai.db",
			},
		},
		Server: ServerConfig{
			Host:            "0.0.0.0",
			Port:            8080,
			EnableWebSocket: true,
		},
		Logging: LoggingConfig{
			Level:     "info",
			Format:    "text",
			Output:    "stdout",
			MaxSizeMB: 10,
		},
		RagCode: RagCodeConfig{
			Enabled:        false,
			IndexOnStartup: false,
			Paths:          []string{"./internal", "./cmd"},
			Collection:     "do-ai-code",
			Model:          "",
			Include:        []string{"**/*.go"},
			Exclude:        []string{"**/*_test.go", "vendor/**", ".git/**", "testdata/**"},
		},
		Docs: DocsConfig{
			Collection: "do-ai-docs",
			ReadmePath: "./README.md",
			DocsPaths:  []string{"./docs"},
		},
		APIDocs: APIDocsConfig{
			Collection: "do-ai-api-docs",
		},
		Workspace: WorkspaceConfig{
			Enabled:            true,
			AutoIndex:          true,
			MaxWorkspaces:      10,
			DetectionMarkers:   []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", "pom.xml"},
			ExcludePatterns:    []string{"node_modules", ".git", "vendor", "target", "build", "dist", ".venv"},
			CollectionPrefix:   "ragcode",
			IndexInclude:       []string{}, // Empty means use global rag_code.include
			IndexExclude:       []string{}, // Empty means use global rag_code.exclude
			AutoCreateIDERules: true,
		},
	}
}

// applyEnvOverrides applies environment variable overrides to the configuration
func applyEnvOverrides(cfg *Config) {
	if apiKey := os.Getenv("OPENAI_API_KEY"); apiKey != "" {
		cfg.LLM.APIKey = apiKey
	}
	if baseURL := os.Getenv("LLM_BASE_URL"); baseURL != "" {
		cfg.LLM.BaseURL = baseURL
	}

	// Docs configuration overrides
	if docsColl := os.Getenv("DOCS_COLLECTION"); docsColl != "" {
		cfg.Docs.Collection = docsColl
	}
	if readmePath := os.Getenv("DOCS_README_PATH"); readmePath != "" {
		cfg.Docs.ReadmePath = readmePath
	}
	if docsPaths := os.Getenv("DOCS_PATHS"); docsPaths != "" {
		parts := strings.Split(docsPaths, ",")
		cfg.Docs.DocsPaths = cfg.Docs.DocsPaths[:0]
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.Docs.DocsPaths = append(cfg.Docs.DocsPaths, p)
			}
		}
	}

	if apiColl := os.Getenv("API_DOCS_COLLECTION"); apiColl != "" {
		cfg.APIDocs.Collection = apiColl
	}

	// LLM configuration overrides
	if provider := os.Getenv("LLM_PROVIDER"); provider != "" {
		cfg.LLM.Provider = provider
	}
	if baseURL := os.Getenv("OLLAMA_BASE_URL"); baseURL != "" {
		cfg.LLM.OllamaBaseURL = baseURL
	}
	if model := os.Getenv("OLLAMA_MODEL"); model != "" {
		cfg.LLM.OllamaModel = model
	}
	if embed := os.Getenv("OLLAMA_EMBED"); embed != "" {
		cfg.LLM.OllamaEmbed = embed
	}

	// Vector DB (Qdrant) configuration overrides
	if url := os.Getenv("QDRANT_URL"); url != "" {
		cfg.Storage.VectorDB.URL = url
	}
	if apiKey := os.Getenv("QDRANT_API_KEY"); apiKey != "" {
		cfg.Storage.VectorDB.APIKey = apiKey
	}
	if coll := os.Getenv("QDRANT_COLLECTION"); coll != "" {
		cfg.Storage.VectorDB.Collection = coll
	}

	// RagCode configuration overrides
	if codeColl := os.Getenv("CODE_RAG_COLLECTION"); codeColl != "" {
		cfg.RagCode.Collection = codeColl
	}
	if codeModel := os.Getenv("CODE_RAG_MODEL"); codeModel != "" {
		cfg.RagCode.Model = codeModel
	}
	if enabled := os.Getenv("CODE_RAG_ENABLED"); enabled != "" {
		if v, err := strconv.ParseBool(enabled); err == nil {
			cfg.RagCode.Enabled = v
		}
	}
	if indexOnStartup := os.Getenv("CODE_RAG_INDEX_ON_STARTUP"); indexOnStartup != "" {
		if v, err := strconv.ParseBool(indexOnStartup); err == nil {
			cfg.RagCode.IndexOnStartup = v
		}
	}

	// Workspace configuration overrides
	if wsEnabled := os.Getenv("WORKSPACE_ENABLED"); wsEnabled != "" {
		if v, err := strconv.ParseBool(wsEnabled); err == nil {
			cfg.Workspace.Enabled = v
		}
	}
	if wsAutoIndex := os.Getenv("WORKSPACE_AUTO_INDEX"); wsAutoIndex != "" {
		if v, err := strconv.ParseBool(wsAutoIndex); err == nil {
			cfg.Workspace.AutoIndex = v
		}
	}
	if wsMax := os.Getenv("WORKSPACE_MAX_WORKSPACES"); wsMax != "" {
		if v, err := strconv.Atoi(wsMax); err == nil {
			cfg.Workspace.MaxWorkspaces = v
		}
	}
	if wsPrefix := os.Getenv("WORKSPACE_COLLECTION_PREFIX"); wsPrefix != "" {
		cfg.Workspace.CollectionPrefix = wsPrefix
	}
	if wsIDERules := os.Getenv("WORKSPACE_AUTO_CREATE_IDE_RULES"); wsIDERules != "" {
		if v, err := strconv.ParseBool(wsIDERules); err == nil {
			cfg.Workspace.AutoCreateIDERules = v
		}
	}
}

// migrateEmbeddingModel automatically migrates from old unstable embedding model.
// NOTE: nomic-embed-text is no longer considered deprecated — it is a valid choice
// with 8K context and 768 dimensions. Auto-migration is disabled.
func migrateEmbeddingModel(cfg *Config) bool {
	// No auto-migration needed currently.
	// Users choose their embedding model explicitly in config.yaml.
	return false
}

// ResolveWorkspaceRoot returns the absolute path of the workspace root.
// If workspace_root is set in config, resolves it relative to ConfigDir.
// Returns empty string if workspace_root is not configured.
func (cfg *Config) ResolveWorkspaceRoot() string {
	wsRoot := cfg.Workspace.WorkspaceRoot
	if wsRoot == "" {
		return ""
	}
	if filepath.IsAbs(wsRoot) {
		return filepath.Clean(wsRoot)
	}
	if cfg.ConfigDir == "" {
		// Fallback: resolve relative to CWD
		abs, err := filepath.Abs(wsRoot)
		if err != nil {
			return wsRoot
		}
		return abs
	}
	return filepath.Clean(filepath.Join(cfg.ConfigDir, wsRoot))
}

// FindConfigFile looks for config.yaml in standard locations:
// 1. Explicit path (if provided and not empty)
// 2. Next to the running binary (config.yaml must be in the same directory)
// 3. Current working directory
// Returns the path to the first config file found, or empty string.
func FindConfigFile(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
	}

	// Look next to the binary
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Look in CWD
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}

	return explicit // fallback to whatever was passed
}

// validate checks if the configuration is valid
func validate(cfg *Config) error {
	// Default to ollama if provider is not set
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = "ollama"
	}

	// Only ollama is supported in this version of the project
	if cfg.LLM.Provider != "ollama" {
		return fmt.Errorf("llm.provider must be 'ollama'")
	}

	// Validate ollama model configuration
	if cfg.LLM.OllamaModel == "" && cfg.LLM.Model == "" {
		return fmt.Errorf("llm.ollama_model (or legacy llm.model) is required for ollama provider")
	}

	// Ensure log max size
	if cfg.Logging.MaxSizeMB <= 0 {
		cfg.Logging.MaxSizeMB = 10
	}

	return nil
}
