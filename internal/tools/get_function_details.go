package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/llm"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/ragcode/analyzers/php"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// GetFunctionDetailsTool returns complete details about a function or method
type GetFunctionDetailsTool struct {
	longTermMemory   memory.LongTermMemory
	embedder         llm.Provider
	workspaceManager *workspace.Manager
}

// NewGetFunctionDetailsTool creates a new function details tool
func NewGetFunctionDetailsTool(ltm memory.LongTermMemory, embedder llm.Provider) *GetFunctionDetailsTool {
	return &GetFunctionDetailsTool{
		longTermMemory: ltm,
		embedder:       embedder,
	}
}

// SetWorkspaceManager sets the workspace manager for workspace-aware searching
func (t *GetFunctionDetailsTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *GetFunctionDetailsTool) Name() string {
	return "get_function_details"
}

func (t *GetFunctionDetailsTool) Description() string {
	return "Get COMPLETE function/method source code - returns full implementation with signature, parameters, return types, and body. Use when you know the exact function name. Returns the entire function ready to read or modify. Works for Go, PHP, Python."
}

func (t *GetFunctionDetailsTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	functionName, ok := args["function_name"].(string)
	if !ok || functionName == "" {
		return "", fmt.Errorf("function_name is required")
	}

	// Optional package filter
	packagePath := ""
	if pkg, ok := args["package"].(string); ok {
		packagePath = pkg
	}

	// Optional output format: markdown (default) or json
	outputFormat := "markdown"
	if of, ok := args["output_format"].(string); ok && of != "" {
		outputFormat = strings.ToLower(of)
	}

	// file_path is required for workspace detection
	filePath := extractFilePathFromParams(args)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for get_function_details. Please provide a file path from your workspace")
	}

	// Try workspace detection if workspace manager is available
	var searchMemory memory.LongTermMemory
	var workspacePath string
	var collectionName string

	if t.workspaceManager != nil {
		workspaceInfo, err := t.workspaceManager.DetectWorkspace(args)
		if err == nil && workspaceInfo != nil {
			workspacePath = workspaceInfo.Root

			// Detect language from file path or use first detected language
			language := inferLanguageFromPath(filePath)
			if language == "" && len(workspaceInfo.Languages) > 0 {
				language = workspaceInfo.Languages[0]
			}
			if language == "" {
				language = workspaceInfo.ProjectType
			}

			collectionName = workspaceInfo.CollectionNameForLanguage(language)
			mem, err := t.workspaceManager.GetMemoryForWorkspaceLanguage(ctx, workspaceInfo, language)
			if err == nil && mem != nil {
				// Check if indexing is in progress
				indexKey := workspaceInfo.ID + "-" + language
				if t.workspaceManager.IsIndexing(indexKey) {
					return fmt.Sprintf("⏳ Workspace '%s' language '%s' is currently being indexed in the background.\n"+
						"Please try again in a few moments.\n"+
						"Workspace: %s\n"+
						"Language: %s\n"+
						"Collection: %s",
						workspaceInfo.Root, language, workspaceInfo.Root, language, collectionName), nil
				}

				// Check if collection exists before proceeding
				if msg, err := CheckCollectionStatus(ctx, mem, collectionName, workspacePath); err != nil || msg != "" {
					if err != nil {
						return "", err
					}
					return msg, nil
				}

				searchMemory = mem
			}
		}
	}

	// Use workspace-specific memory or fall back to default
	if searchMemory == nil {
		searchMemory = t.longTermMemory
	}

	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	// Search for the function in the vector database
	query := fmt.Sprintf("function %s definition", functionName)
	if packagePath != "" {
		query = fmt.Sprintf("function %s in package %s", functionName, packagePath)
	}

	// Generate query embedding
	queryEmbedding, err := t.embedder.Embed(ctx, query)
	if err != nil {
		return "", fmt.Errorf("failed to generate query embedding: %w", err)
	}

	// First, try exact name+type search (faster and more accurate)
	type ExactSearcher interface {
		SearchByNameAndType(ctx context.Context, name string, types []string) ([]memory.Document, error)
	}

	functionKinds := []string{"function", "method"}

	var results []memory.Document
	if exactSearcher, ok := searchMemory.(ExactSearcher); ok {
		results, err = exactSearcher.SearchByNameAndType(ctx, functionName, functionKinds)
		if err == nil && len(results) > 0 {
			// Found exact match, use it directly
			goto processResults
		}
	}

	// Fallback to semantic search if exact search didn't find anything
	{
		type CodeSearcher interface {
			SearchCodeOnly(ctx context.Context, query []float64, limit int) ([]memory.Document, error)
		}

		if codeSearcher, ok := searchMemory.(CodeSearcher); ok {
			results, err = codeSearcher.SearchCodeOnly(ctx, queryEmbedding, 50)
		} else {
			results, err = searchMemory.Search(ctx, queryEmbedding, 50)
		}
		if err != nil {
			return "", fmt.Errorf("search failed: %w", err)
		}
	}

processResults:

	if len(results) == 0 {
		// Check if this is a workspace search with empty collection
		if workspacePath != "" && collectionName != "" {
			if msg, err := CheckSearchResults(0, collectionName, workspacePath); err != nil || msg != "" {
				if err != nil {
					return "", err
				}
				return msg, nil
			}
			return fmt.Sprintf("Function '%s' not found in workspace '%s'", functionName, workspacePath), nil
		}
		return fmt.Sprintf("Function '%s' not found", functionName), nil
	}

	// Find exact match
	var bestMatch *memory.Document
	for _, result := range results {
		var chunk codetypes.CodeChunk
		if err := json.Unmarshal([]byte(result.Content), &chunk); err != nil {
			continue
		}

		// Check if this is a function chunk
		if chunk.Type != "function" && chunk.Type != "method" {
			continue
		}

		// Check name match
		if chunk.Name != functionName {
			continue
		}

		// Check package match if specified
		if packagePath != "" && !strings.Contains(chunk.Package, packagePath) {
			continue
		}

		bestMatch = &result
		break
	}

	if bestMatch == nil {
		return fmt.Sprintf("Function '%s' not found (searched %d chunks)", functionName, len(results)), nil
	}

	var chunk codetypes.CodeChunk
	if err := json.Unmarshal([]byte(bestMatch.Content), &chunk); err != nil {
		return "", fmt.Errorf("failed to parse chunk: %w", err)
	}

	// Read actual code body from file
	codeBody := chunk.Code
	if codeBody == "" && chunk.FilePath != "" && chunk.StartLine > 0 && chunk.EndLine > 0 {
		body, err := readFileLines(chunk.FilePath, chunk.StartLine, chunk.EndLine)
		if err == nil {
			codeBody = body
		}
	}

	// PHP: use PHP analyzer directly on the source file to build a rich function/method view
	if chunk.Language == "php" {
		return t.buildPHPFunctionResponse(&chunk, codeBody, outputFormat)
	}

	// Default (Go and others): optional JSON output, otherwise keep existing
	// markdown behaviour using the CodeChunk data.
	if strings.ToLower(outputFormat) == "json" {
		// Go: enrich descriptor using metadata from CodeChunk (receiver,
		// parameters, returns), so AI are-aware of full function shape.
		var desc codetypes.FunctionDescriptor
		if chunk.Language == "go" {
			desc = buildGoFunctionDescriptor(&chunk, codeBody)
		} else {
			desc = codetypes.FunctionDescriptor{
				Language:    chunk.Language,
				Kind:        chunk.Type,
				Name:        chunk.Name,
				Namespace:   chunk.Package,
				Signature:   chunk.Signature,
				Description: chunk.Docstring,
				Location: codetypes.SymbolLocation{
					FilePath:  chunk.FilePath,
					StartLine: chunk.StartLine,
					EndLine:   chunk.EndLine,
				},
				Code: codeBody,
			}
		}
		data, err := json.MarshalIndent(desc, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal Go function descriptor: %w", err)
		}
		return string(data), nil
	}

	// Markdown behaviour (Go and others)
	var response strings.Builder
	response.WriteString(fmt.Sprintf("# %s\n\n", chunk.Name))
	response.WriteString(fmt.Sprintf("**Type:** %s\n", chunk.Type))
	response.WriteString(fmt.Sprintf("**Package:** %s\n", chunk.Package))
	response.WriteString(fmt.Sprintf("**Signature:** `%s`\n\n", chunk.Signature))

	if chunk.Docstring != "" {
		response.WriteString(fmt.Sprintf("**Description:**\n%s\n\n", chunk.Docstring))
	}

	response.WriteString(fmt.Sprintf("**Location:** `%s:%d-%d`\n\n", chunk.FilePath, chunk.StartLine, chunk.EndLine))

	if codeBody != "" {
		response.WriteString("**Code:**\n```go\n")
		response.WriteString(codeBody)
		response.WriteString("\n```\n")
	}

	return response.String(), nil
}

// buildGoFunctionDescriptor constructs a richer FunctionDescriptor for Go
// functions/methods using CodeChunk metadata produced by the Go analyzer
// (receiver, parameters, returns).
func buildGoFunctionDescriptor(chunk *codetypes.CodeChunk, codeBody string) codetypes.FunctionDescriptor {
	fd := codetypes.FunctionDescriptor{
		Language:    chunk.Language,
		Kind:        chunk.Type,
		Name:        chunk.Name,
		Namespace:   chunk.Package,
		Signature:   chunk.Signature,
		Description: chunk.Docstring,
		Location: codetypes.SymbolLocation{
			FilePath:  chunk.FilePath,
			StartLine: chunk.StartLine,
			EndLine:   chunk.EndLine,
		},
		Code: codeBody,
	}

	// Go-specific enrichment based on analyzer metadata
	if strings.ToLower(chunk.Language) != "go" {
		return fd
	}

	// Visibility: exported vs unexported symbol
	if chunk.Name != "" {
		first, _ := utf8DecodeRuneInString(chunk.Name)
		if unicode.IsUpper(first) {
			fd.Visibility = "exported"
		}
	}

	if chunk.Metadata != nil {
		// Receiver / method kind
		if recv, ok := chunk.Metadata["receiver"].(string); ok && recv != "" {
			fd.Receiver = recv
			fd.Kind = "method"
		}

		// Parameters
		if rawParams, ok := chunk.Metadata["params"]; ok {
			switch v := rawParams.(type) {
			case []codetypes.ParamInfo:
				for _, p := range v {
					fd.Parameters = append(fd.Parameters, codetypes.ParamDescriptor{
						Name: p.Name,
						Type: p.Type,
					})
				}
			case []interface{}:
				for _, item := range v {
					if m, ok := item.(map[string]interface{}); ok {
						name, _ := m["name"].(string)
						typ, _ := m["type"].(string)
						fd.Parameters = append(fd.Parameters, codetypes.ParamDescriptor{
							Name: name,
							Type: typ,
						})
					}
				}
			}
		}

		// Returns
		if rawReturns, ok := chunk.Metadata["returns"]; ok {
			switch v := rawReturns.(type) {
			case []codetypes.ReturnInfo:
				for _, r := range v {
					fd.Returns = append(fd.Returns, codetypes.ReturnDescriptor{
						Type:        r.Type,
						Description: r.Description,
						SourceHint:  "type_hint",
					})
				}
			case []interface{}:
				for _, item := range v {
					if m, ok := item.(map[string]interface{}); ok {
						typ, _ := m["type"].(string)
						desc, _ := m["description"].(string)
						fd.Returns = append(fd.Returns, codetypes.ReturnDescriptor{
							Type:        typ,
							Description: desc,
							SourceHint:  "type_hint",
						})
					}
				}
			}
		}
	}

	return fd
}

// utf8DecodeRuneInString is a tiny helper so we don't import the entire utf8
// package interface here.
func utf8DecodeRuneInString(s string) (rune, int) {
	if s == "" {
		return 0, 0
	}
	r := []rune(s)
	return r[0], len(string(r[0]))
}

// buildPHPFunctionResponse builds a rich view for a PHP function or method
// using the PHP BridgeAnalyzer to re-parse the source file and extract
// detailed parameter/return/visibility information from the resulting CodeChunks.
//
// outputFormat can be "markdown" (default) or "json". The JSON form returns a
// codetypes.FunctionDescriptor encoded as JSON.
func (t *GetFunctionDetailsTool) buildPHPFunctionResponse(chunk *codetypes.CodeChunk, codeBody, outputFormat string) (string, error) {
	format := strings.ToLower(outputFormat)
	if format == "" {
		format = "markdown"
	}

	// Helper to build a FunctionDescriptor from a matching CodeChunk produced
	// by the BridgeAnalyzer. The matched chunk carries metadata with
	// parameters, returns, visibility, etc.
	buildDescriptor := func(matched *codetypes.CodeChunk, className, namespace string) codetypes.FunctionDescriptor {
		fd := codetypes.FunctionDescriptor{
			Language:  chunk.Language,
			Kind:      chunk.Type,
			Name:      chunk.Name,
			Namespace: namespace,
			Receiver:  className,
			Location: codetypes.SymbolLocation{
				FilePath:  chunk.FilePath,
				StartLine: chunk.StartLine,
				EndLine:   chunk.EndLine,
			},
			Code: codeBody,
		}

		sig := chunk.Signature
		if sig == "" {
			sig = fmt.Sprintf("function %s()", chunk.Name)
		}
		fd.Description = chunk.Docstring

		if matched != nil {
			if matched.Signature != "" {
				sig = matched.Signature
			}
			if matched.Docstring != "" {
				fd.Description = matched.Docstring
			}

			// Extract structured metadata from the bridge chunk
			if matched.Metadata != nil {
				if vis, ok := matched.Metadata["visibility"].(string); ok {
					fd.Visibility = vis
				}
				if isStatic, ok := matched.Metadata["is_static"].(bool); ok {
					fd.IsStatic = isStatic
				}
				if isAbstract, ok := matched.Metadata["is_abstract"].(bool); ok {
					fd.IsAbstract = isAbstract
				}
				if isFinal, ok := matched.Metadata["is_final"].(bool); ok {
					fd.IsFinal = isFinal
				}

				// Parameters
				if rawParams, ok := matched.Metadata["parameters"]; ok {
					if params, ok := rawParams.([]interface{}); ok {
						for _, item := range params {
							if m, ok := item.(map[string]interface{}); ok {
								name, _ := m["name"].(string)
								typ, _ := m["type"].(string)
								if typ == "" {
									typ = "mixed"
								}
								fd.Parameters = append(fd.Parameters, codetypes.ParamDescriptor{
									Name: name,
									Type: typ,
								})
							}
						}
					}
				}

				// Return type
				if retType, ok := matched.Metadata["return_type"].(string); ok && retType != "" {
					fd.Returns = append(fd.Returns, codetypes.ReturnDescriptor{
						Type:       retType,
						SourceHint: "type_hint",
					})
				}
			}
		}

		fd.Signature = sig
		return fd
	}

	// Fallback descriptor from the chunk only (no analyzer data).
	fallbackDescriptor := func() codetypes.FunctionDescriptor {
		return buildDescriptor(nil, "", chunk.Package)
	}

	// If we don't have a file path we cannot re-parse; use the chunk as-is.
	if chunk.FilePath == "" {
		if format == "json" {
			desc := fallbackDescriptor()
			data, err := json.MarshalIndent(desc, "", "  ")
			if err != nil {
				return "", fmt.Errorf("failed to marshal PHP function descriptor: %w", err)
			}
			return string(data), nil
		}
	}

	// Re-analyze the source file via the PHP bridge to get richer metadata.
	analyzer := php.NewBridgeAnalyzer()
	bridgeChunks, err := analyzer.AnalyzePaths([]string{chunk.FilePath})
	if err != nil || len(bridgeChunks) == 0 {
		// Degrade gracefully to a simple representation
		if format == "json" {
			desc := fallbackDescriptor()
			data, err2 := json.MarshalIndent(desc, "", "  ")
			if err2 != nil {
				return "", fmt.Errorf("failed to marshal PHP function descriptor: %w", err2)
			}
			return string(data), nil
		}

		var response strings.Builder
		response.WriteString(fmt.Sprintf("# %s\n\n", chunk.Name))
		response.WriteString(fmt.Sprintf("**Type:** %s\n", chunk.Type))
		response.WriteString(fmt.Sprintf("**Namespace:** %s\n", chunk.Package))
		response.WriteString(fmt.Sprintf("**Location:** `%s:%d-%d`\n\n", chunk.FilePath, chunk.StartLine, chunk.EndLine))
		if chunk.Signature != "" {
			response.WriteString(fmt.Sprintf("**Signature:** `%s`\n\n", chunk.Signature))
		}
		if codeBody != "" {
			response.WriteString("**Code:**\n```php\n")
			response.WriteString(codeBody)
			response.WriteString("\n```\n")
		}
		return response.String(), nil
	}

	// Find the matching chunk from the bridge output.
	var matched *codetypes.CodeChunk
	var className string
	var namespace string

	for i := range bridgeChunks {
		bc := &bridgeChunks[i]
		// Must match type (function/method) and name.
		if bc.Type != chunk.Type || bc.Name != chunk.Name {
			continue
		}
		// If we have line numbers, verify they match.
		if chunk.StartLine > 0 && bc.StartLine > 0 && chunk.StartLine != bc.StartLine {
			continue
		}
		// Package/namespace match if specified.
		if chunk.Package != "" && bc.Package != "" && bc.Package != chunk.Package {
			continue
		}
		matched = bc
		namespace = bc.Package
		// For methods, try to extract class name from metadata.
		if bc.Metadata != nil {
			if cn, ok := bc.Metadata["class_name"].(string); ok {
				className = cn
			}
		}
		break
	}

	// JSON output
	if format == "json" {
		desc := buildDescriptor(matched, className, namespace)
		data, err := json.MarshalIndent(desc, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal PHP function descriptor: %w", err)
		}
		return string(data), nil
	}

	// Markdown output
	var response strings.Builder

	kind := chunk.Type
	if kind != "method" {
		kind = "function"
	}

	if namespace == "" {
		namespace = chunk.Package
	}

	// Header
	response.WriteString(fmt.Sprintf("# %s\n\n", chunk.Name))
	response.WriteString(fmt.Sprintf("**Kind:** %s\n", kind))
	response.WriteString(fmt.Sprintf("**Namespace:** %s\n", namespace))
	if className != "" {
		response.WriteString(fmt.Sprintf("**Class:** %s\n", className))
	}

	// Build descriptor to reuse the structured info for markdown rendering.
	desc := buildDescriptor(matched, className, namespace)

	// Signature
	response.WriteString(fmt.Sprintf("**Signature:** `%s`\n\n", desc.Signature))

	// Description
	if desc.Description != "" {
		response.WriteString(fmt.Sprintf("**Description:**\n%s\n\n", desc.Description))
	}

	// Location
	response.WriteString(fmt.Sprintf("**Location:** `%s:%d-%d`\n\n", chunk.FilePath, chunk.StartLine, chunk.EndLine))

	// Parameters
	if len(desc.Parameters) > 0 {
		response.WriteString("**Parameters:**\n")
		for _, p := range desc.Parameters {
			response.WriteString(fmt.Sprintf("- `$%s`: %s\n", p.Name, p.Type))
		}
		response.WriteString("\n")
	}

	// Returns
	if len(desc.Returns) > 0 {
		response.WriteString("**Returns:**\n")
		for _, r := range desc.Returns {
			typeStr := r.Type
			if typeStr == "" {
				typeStr = "mixed"
			}
			if r.Description != "" {
				response.WriteString(fmt.Sprintf("- `%s` - %s\n", typeStr, r.Description))
			} else {
				response.WriteString(fmt.Sprintf("- `%s`\n", typeStr))
			}
		}
		response.WriteString("\n")
	}

	// Code snippet
	if codeBody != "" {
		response.WriteString("**Code:**\n```php\n")
		response.WriteString(codeBody)
		response.WriteString("\n```\n")
	}

	return response.String(), nil
}
