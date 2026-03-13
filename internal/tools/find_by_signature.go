package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// FindBySignatureTool finds functions/methods by parameter or return types.
type FindBySignatureTool struct {
	memory           memory.LongTermMemory
	workspaceManager *workspace.Manager
}

func NewFindBySignatureTool(mem memory.LongTermMemory) *FindBySignatureTool {
	return &FindBySignatureTool{memory: mem}
}

func (t *FindBySignatureTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *FindBySignatureTool) Name() string { return "find_by_signature" }

func (t *FindBySignatureTool) Description() string {
	return "Find functions and methods by their parameter types, return types, or signature patterns. " +
		"Use to answer questions like 'what methods return PaymentResponse?' or " +
		"'what functions accept ProviderRequestInterface?'. Searches signatures stored in the index."
}

func (t *FindBySignatureTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	returnType, _ := params["return_type"].(string)
	paramType, _ := params["param_type"].(string)
	pattern, _ := params["pattern"].(string)

	if returnType == "" && paramType == "" && pattern == "" {
		return "", fmt.Errorf("at least one of return_type, param_type, or pattern is required")
	}

	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for find_by_signature")
	}

	limit := 50
	if v, ok := params["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	searchMemory, workspacePath, _, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	// Scroll through all method/function chunks
	type AllScroller interface {
		ScrollAll(ctx context.Context, maxResults int) ([]memory.Document, error)
	}

	scroller, ok := searchMemory.(AllScroller)
	if !ok {
		return "", fmt.Errorf("storage backend does not support scroll operations")
	}

	allDocs, err := scroller.ScrollAll(ctx, 10000)
	if err != nil {
		return "", fmt.Errorf("scroll failed: %w", err)
	}

	// Filter by signature content
	type match struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Package   string `json:"package,omitempty"`
		Signature string `json:"signature"`
		FilePath  string `json:"file_path"`
		StartLine int    `json:"start_line"`
	}

	var matches []match
	for _, doc := range allDocs {
		var chunk codetypes.CodeChunk
		if err := json.Unmarshal([]byte(doc.Content), &chunk); err != nil {
			continue
		}
		if chunk.Type != "function" && chunk.Type != "method" {
			continue
		}
		if chunk.Signature == "" {
			continue
		}

		sig := chunk.Signature
		sigLower := strings.ToLower(sig)

		matched := true
		if returnType != "" {
			// Check return type in signature (after ":" or "returns")
			rtLower := strings.ToLower(returnType)
			if !strings.Contains(sigLower, rtLower) {
				// Also check metadata return_type
				if rt, ok := doc.Metadata["return_type"]; ok {
					if !strings.Contains(strings.ToLower(fmt.Sprintf("%v", rt)), rtLower) {
						matched = false
					}
				} else {
					matched = false
				}
			}
		}
		if paramType != "" && matched {
			ptLower := strings.ToLower(paramType)
			if !strings.Contains(sigLower, ptLower) {
				matched = false
			}
		}
		if pattern != "" && matched {
			patLower := strings.ToLower(pattern)
			if !strings.Contains(sigLower, patLower) {
				matched = false
			}
		}

		if matched {
			matches = append(matches, match{
				Name:      chunk.Name,
				Type:      chunk.Type,
				Package:   chunk.Package,
				Signature: sig,
				FilePath:  chunk.FilePath,
				StartLine: chunk.StartLine,
			})
			if len(matches) >= limit {
				break
			}
		}
	}

	if len(matches) == 0 {
		desc := buildSignatureSearchDesc(returnType, paramType, pattern)
		if workspacePath != "" {
			return fmt.Sprintf("No functions/methods found matching %s in workspace '%s'.", desc, workspacePath), nil
		}
		return fmt.Sprintf("No functions/methods found matching %s.", desc), nil
	}

	result := map[string]interface{}{
		"total":   len(matches),
		"matches": matches,
	}
	if workspacePath != "" {
		result["workspace"] = workspacePath
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

func buildSignatureSearchDesc(returnType, paramType, pattern string) string {
	var parts []string
	if returnType != "" {
		parts = append(parts, fmt.Sprintf("return_type=%q", returnType))
	}
	if paramType != "" {
		parts = append(parts, fmt.Sprintf("param_type=%q", paramType))
	}
	if pattern != "" {
		parts = append(parts, fmt.Sprintf("pattern=%q", pattern))
	}
	return strings.Join(parts, ", ")
}
