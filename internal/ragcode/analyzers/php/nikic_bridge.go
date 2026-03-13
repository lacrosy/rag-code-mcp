package php

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
)

// BridgeAnalyzer implements codetypes.PathAnalyzer using nikic/php-parser via PHP CLI bridge.
type BridgeAnalyzer struct{}

// NewBridgeAnalyzer creates a new PHP bridge analyzer.
func NewBridgeAnalyzer() *BridgeAnalyzer {
	return &BridgeAnalyzer{}
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
	bridgePath, err := findBridgePath()
	if err != nil {
		return nil, err
	}

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

	// Run PHP bridge in batch mode
	symbols, err := runBridge(bridgePath, phpFiles)
	if err != nil {
		return nil, fmt.Errorf("PHP bridge failed: %w", err)
	}

	return convertBridgeSymbolsToChunks(symbols), nil
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

// runBridge executes the PHP bridge script with a batch of files via stdin.
func runBridge(bridgePath string, files []string) ([]bridgeSymbol, error) {
	// Check if php is available
	phpBin, err := exec.LookPath("php")
	if err != nil {
		return nil, fmt.Errorf("PHP CLI not found in PATH. Install PHP 8.1+ to use the PHP analyzer: %w", err)
	}

	cmd := exec.Command(phpBin, bridgePath, "--batch")
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
