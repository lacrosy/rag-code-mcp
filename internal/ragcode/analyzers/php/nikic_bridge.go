package php

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
)

// BridgeAnalyzer implements codetypes.PathAnalyzer using nikic/php-parser.
// Default: HTTP mode via Docker container (RAGCODE_PHP_BRIDGE_URL, default http://localhost:9100).
// Fallback: CLI mode if RAGCODE_PHP_BRIDGE is explicitly set to a parse.php path.
type BridgeAnalyzer struct {
	httpURL       string
	client        *http.Client
	extractorsDir string
}

// NewBridgeAnalyzer creates a new PHP bridge analyzer.
// Default: Docker HTTP mode on localhost:9100.
// Override: set RAGCODE_PHP_BRIDGE=/path/to/parse.php for CLI mode.
func NewBridgeAnalyzer() *BridgeAnalyzer {
	return NewBridgeAnalyzerWithExtractors("")
}

// NewBridgeAnalyzerWithExtractors creates a PHP bridge analyzer with an optional
// custom extractors directory. The directory path is passed to the PHP bridge
// so it can load project-specific *Extractor.php plugins.
func NewBridgeAnalyzerWithExtractors(extractorsDir string) *BridgeAnalyzer {
	// CLI mode override
	if bridgePath := os.Getenv("RAGCODE_PHP_BRIDGE"); bridgePath != "" {
		return &BridgeAnalyzer{extractorsDir: extractorsDir}
	}
	// Default: Docker HTTP mode
	url := os.Getenv("RAGCODE_PHP_BRIDGE_URL")
	if url == "" {
		url = "http://localhost:9100"
	}
	return &BridgeAnalyzer{
		httpURL:       strings.TrimRight(url, "/"),
		client:        &http.Client{Timeout: 120 * time.Second},
		extractorsDir: extractorsDir,
	}
}

// bridgeSymbol represents a symbol returned by the PHP bridge JSON output.
type bridgeSymbol struct {
	Name          string                 `json:"name"`
	Type          string                 `json:"type"` // class, interface, trait, enum, function, method, property, constant
	Namespace     string                 `json:"namespace"`
	ClassName     string                 `json:"class_name,omitempty"`
	Signature     string                 `json:"signature"`
	FilePath      string                 `json:"file_path"`
	StartLine     int                    `json:"start_line"`
	EndLine       int                    `json:"end_line"`
	Code          string                 `json:"code,omitempty"`
	Docstring     string                 `json:"docstring,omitempty"`
	Extends       json.RawMessage        `json:"extends,omitempty"`     // string or []string
	Implements    []string               `json:"implements,omitempty"`
	Uses          []string               `json:"uses,omitempty"`
	BackedType    string                 `json:"backed_type,omitempty"` // enum
	Cases         []bridgeEnumCase       `json:"cases,omitempty"`       // enum
	Parameters    []bridgeParam          `json:"parameters,omitempty"`
	ReturnType    *string                `json:"return_type"`
	PropertyType  *string                `json:"property_type"`
	Visibility    string                 `json:"visibility,omitempty"`
	SetVisibility string                 `json:"set_visibility,omitempty"` // PHP 8.4
	ConstantType  *string                `json:"constant_type"`
	Value         string                 `json:"value,omitempty"`
	Modifiers     map[string]bool        `json:"modifiers,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

type bridgeEnumCase struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type bridgeParam struct {
	Name string  `json:"name"`
	Type *string `json:"type"`
}

// AnalyzePaths implements the PathAnalyzer interface.
func (ba *BridgeAnalyzer) AnalyzePaths(paths []string) ([]codetypes.CodeChunk, error) {
	// Collect all PHP files from paths
	var phpFiles []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot access %s: %v\n", p, err)
			continue
		}
		if info.IsDir() {
			files, err := collectPHPFiles(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: error walking %s: %v\n", p, err)
				continue
			}
			phpFiles = append(phpFiles, files...)
		} else if strings.HasSuffix(p, ".php") {
			phpFiles = append(phpFiles, p)
		}
	}

	if len(phpFiles) == 0 {
		return nil, nil
	}

	// Route to HTTP or CLI mode
	var symbols []bridgeSymbol
	var err error

	if ba.httpURL != "" {
		symbols, err = ba.runBridgeHTTP(phpFiles)
	} else {
		bridgePath, pathErr := findBridgePath()
		if pathErr != nil {
			return nil, pathErr
		}
		symbols, err = ba.runBridgeCLI(bridgePath, phpFiles)
	}

	if err != nil {
		return nil, fmt.Errorf("PHP bridge failed: %w", err)
	}

	return convertBridgeSymbolsToChunks(symbols), nil
}

// bridgeHTTPResponse is the JSON response from the PHP bridge HTTP server.
type bridgeHTTPResponse struct {
	Symbols []bridgeSymbol `json:"symbols"`
	Errors  []string       `json:"errors,omitempty"`
}

// runBridgeHTTP sends files to the PHP bridge HTTP server for parsing.
func (ba *BridgeAnalyzer) runBridgeHTTP(files []string) ([]bridgeSymbol, error) {
	// Process in batches of 100 to avoid huge requests
	const batchSize = 100
	var allSymbols []bridgeSymbol

	for i := 0; i < len(files); i += batchSize {
		end := i + batchSize
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]

		reqPayload := map[string]interface{}{"files": batch}
		if ba.extractorsDir != "" {
			reqPayload["extractors_dir"] = ba.extractorsDir
		}
		reqBody, err := json.Marshal(reqPayload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}

		resp, err := ba.client.Post(ba.httpURL+"/parse", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("HTTP request to PHP bridge failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("PHP bridge HTTP returned %d: %s", resp.StatusCode, truncate(string(body), 500))
		}

		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		var result bridgeHTTPResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("failed to parse PHP bridge HTTP response: %w\noutput (first 500 bytes): %s",
				err, truncate(string(body), 500))
		}

		// Log errors from bridge
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "PHP bridge warning: %s\n", e)
		}

		allSymbols = append(allSymbols, result.Symbols...)
	}

	return allSymbols, nil
}

// findBridgePath locates the parse.php bridge script.
func findBridgePath() (string, error) {
	// 1. RAGCODE_PHP_BRIDGE env
	if envPath := os.Getenv("RAGCODE_PHP_BRIDGE"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath, nil
		}
	}

	// 2. Relative to executable
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidates := []string{
			filepath.Join(exeDir, "php-bridge", "parse.php"),
			filepath.Join(exeDir, "..", "php-bridge", "parse.php"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}

	// 3. Relative to CWD
	if cwd, err := os.Getwd(); err == nil {
		p := filepath.Join(cwd, "php-bridge", "parse.php")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("PHP bridge not found. Set RAGCODE_PHP_BRIDGE env or ensure php-bridge/parse.php exists next to the binary")
}

// collectPHPFiles walks a directory and collects all .php files.
// It does NOT apply its own skip-dirs — the caller (workspace manager) is responsible for filtering.
func collectPHPFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			// Minimal skip: only skip .git and vendor to avoid obvious non-PHP dirs.
			// The main filtering should happen at the workspace manager level.
			if base == ".git" || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".php") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// runBridgeCLI executes the PHP bridge script with a batch of files via stdin.
func (ba *BridgeAnalyzer) runBridgeCLI(bridgePath string, files []string) ([]bridgeSymbol, error) {
	// Check if php is available
	phpBin, err := exec.LookPath("php")
	if err != nil {
		return nil, fmt.Errorf("PHP CLI not found in PATH. Install PHP 8.1+ to use the PHP analyzer: %w", err)
	}

	args := []string{bridgePath, "--batch"}
	if ba.extractorsDir != "" {
		args = append(args, "--extractors-dir", ba.extractorsDir)
	}
	cmd := exec.Command(phpBin, args...)
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n"))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("PHP bridge exited with error: %w\nstderr: %s", err, stderr.String())
	}

	// Log warnings from stderr
	if stderrStr := stderr.String(); stderrStr != "" {
		fmt.Fprint(os.Stderr, stderrStr)
	}

	var symbols []bridgeSymbol
	if err := json.Unmarshal(stdout.Bytes(), &symbols); err != nil {
		return nil, fmt.Errorf("failed to parse PHP bridge JSON output: %w\noutput (first 500 bytes): %s",
			err, truncate(stdout.String(), 500))
	}

	return symbols, nil
}

// convertBridgeSymbolsToChunks converts PHP bridge symbols to CodeChunks.
func convertBridgeSymbolsToChunks(symbols []bridgeSymbol) []codetypes.CodeChunk {
	var chunks []codetypes.CodeChunk

	for _, sym := range symbols {
		chunk := codetypes.CodeChunk{
			Name:      sym.Name,
			Type:      sym.Type,
			Language:  "php",
			Package:   sym.Namespace,
			Signature: sym.Signature,
			FilePath:  sym.FilePath,
			StartLine: sym.StartLine,
			EndLine:   sym.EndLine,
			Code:      sym.Code,
			Docstring: sym.Docstring,
		}

		// Copy metadata from bridge
		if len(sym.Metadata) > 0 {
			chunk.Metadata = make(map[string]any, len(sym.Metadata))
			for k, v := range sym.Metadata {
				chunk.Metadata[k] = v
			}
		}

		chunks = append(chunks, chunk)
	}

	return chunks
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
