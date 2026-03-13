package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/llm"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

type callGraphFunc struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Package   string `json:"package,omitempty"`
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	Code      string `json:"-"`
}

// GetCallGraphTool shows what a function calls and what calls it (1-2 levels).
type GetCallGraphTool struct {
	memory           memory.LongTermMemory
	embedder         llm.Provider
	workspaceManager *workspace.Manager
}

func NewGetCallGraphTool(mem memory.LongTermMemory, embedder llm.Provider) *GetCallGraphTool {
	return &GetCallGraphTool{memory: mem, embedder: embedder}
}

func (t *GetCallGraphTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *GetCallGraphTool) Name() string { return "get_call_graph" }

func (t *GetCallGraphTool) Description() string {
	return "Show call relationships for a function/method: what it calls (callees) and what calls it (callers). " +
		"Analyzes code content to find function references. " +
		"Use to understand dependencies before refactoring or to trace execution flow."
}

func (t *GetCallGraphTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	symbolName, ok := params["symbol_name"].(string)
	if !ok || strings.TrimSpace(symbolName) == "" {
		return "", fmt.Errorf("symbol_name parameter is required")
	}

	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for get_call_graph")
	}

	depth := 1
	if d, ok := params["depth"].(float64); ok && d > 0 {
		depth = int(d)
		if depth > 2 {
			depth = 2
		}
	}

	searchMemory, workspacePath, _, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	// Scroll all to analyze code
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

	// Parse all function/method chunks
	allFuncs := make(map[string]callGraphFunc)
	var targetFunc *callGraphFunc

	for _, doc := range allDocs {
		var chunk codetypes.CodeChunk
		if err := json.Unmarshal([]byte(doc.Content), &chunk); err != nil {
			continue
		}
		if chunk.Type != "function" && chunk.Type != "method" {
			continue
		}

		fi := callGraphFunc{
			Name:      chunk.Name,
			Type:      chunk.Type,
			Package:   chunk.Package,
			FilePath:  chunk.FilePath,
			StartLine: chunk.StartLine,
			Code:      chunk.Code,
		}
		allFuncs[chunk.Name] = fi

		if chunk.Name == symbolName {
			targetFunc = &fi
		}
	}

	if targetFunc == nil {
		return fmt.Sprintf("Function/method '%s' not found in index.", symbolName), nil
	}

	// Find callees: functions referenced in the target's code
	callees := findReferencedFunctions(targetFunc.Code, allFuncs, symbolName)

	// Find callers: functions whose code contains the target symbol
	var callers []callGraphFunc
	for _, fi := range allFuncs {
		if fi.Name == symbolName {
			continue
		}
		if fi.Code != "" && strings.Contains(fi.Code, symbolName) {
			callers = append(callers, fi)
		}
	}

	// Level 2 if requested
	var level2Callees map[string][]string
	var level2Callers map[string][]string

	if depth >= 2 {
		level2Callees = make(map[string][]string)
		for _, callee := range callees {
			if fi, ok := allFuncs[callee.Name]; ok {
				subCallees := findReferencedFunctions(fi.Code, allFuncs, callee.Name)
				names := make([]string, len(subCallees))
				for i, sc := range subCallees {
					names[i] = sc.Name
				}
				if len(names) > 0 {
					level2Callees[callee.Name] = names
				}
			}
		}

		level2Callers = make(map[string][]string)
		for _, caller := range callers {
			var subCallers []string
			for _, fi := range allFuncs {
				if fi.Name == caller.Name || fi.Name == symbolName {
					continue
				}
				if fi.Code != "" && strings.Contains(fi.Code, caller.Name) {
					subCallers = append(subCallers, fi.Name)
				}
			}
			if len(subCallers) > 0 {
				level2Callers[caller.Name] = subCallers
			}
		}
	}

	// Build result
	result := map[string]interface{}{
		"symbol":    symbolName,
		"type":      targetFunc.Type,
		"package":   targetFunc.Package,
		"file_path": targetFunc.FilePath,
		"callees":   callees,
		"callers":   callers,
	}
	if workspacePath != "" {
		result["workspace"] = workspacePath
	}
	if depth >= 2 {
		if len(level2Callees) > 0 {
			result["level2_callees"] = level2Callees
		}
		if len(level2Callers) > 0 {
			result["level2_callers"] = level2Callers
		}
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

func findReferencedFunctions(code string, allFuncs map[string]callGraphFunc, selfName string) []callGraphFunc {
	var callees []callGraphFunc
	seen := make(map[string]bool)
	for name, fi := range allFuncs {
		if name == selfName || seen[name] {
			continue
		}
		// Check if the function name appears in the code as a call
		// Look for name( or ->name( or ::name( patterns
		if code != "" && (strings.Contains(code, name+"(") ||
			strings.Contains(code, "->"+name+"(") ||
			strings.Contains(code, "::"+name+"(") ||
			strings.Contains(code, "."+name+"(")) {
			callees = append(callees, fi)
			seen[name] = true
		}
	}
	return callees
}
