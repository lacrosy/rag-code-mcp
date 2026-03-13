package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// ListMetadataValuesTool lists all unique values for a given metadata key.
// Enables discovery: before filtering, LLM needs to know what values are available.
type ListMetadataValuesTool struct {
	memory           memory.LongTermMemory
	workspaceManager *workspace.Manager
}

func NewListMetadataValuesTool(mem memory.LongTermMemory) *ListMetadataValuesTool {
	return &ListMetadataValuesTool{memory: mem}
}

func (t *ListMetadataValuesTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *ListMetadataValuesTool) Name() string { return "list_metadata_values" }

func (t *ListMetadataValuesTool) Description() string {
	return "List all unique values for a metadata field across indexed code. " +
		"Use to discover available filter values before using search_by_metadata. " +
		"Example: key='pspi_role' returns ['payment_flow', 'status_mapper', ...]. " +
		"Also works for built-in fields like 'type', 'package', 'name'."
}

func (t *ListMetadataValuesTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	key, ok := params["key"].(string)
	if !ok || strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("key parameter is required (e.g., 'pspi_role', 'type', 'package')")
	}

	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for list_metadata_values")
	}

	// Optional pre-filter
	metadataFilter := extractMetadataFilter(params)

	searchMemory, workspacePath, _, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	// Scroll all points (or filtered)
	type AllScroller interface {
		ScrollAll(ctx context.Context, maxResults int) ([]memory.Document, error)
	}
	type FilteredScroller interface {
		ScrollAllWithFilter(ctx context.Context, filters map[string]string, maxResults int) ([]memory.Document, error)
	}

	var docs []memory.Document
	if len(metadataFilter) > 0 {
		if fs, ok := searchMemory.(FilteredScroller); ok {
			docs, err = fs.ScrollAllWithFilter(ctx, metadataFilter, 5000)
		}
	} else {
		if s, ok := searchMemory.(AllScroller); ok {
			docs, err = s.ScrollAll(ctx, 5000)
		}
	}
	if err != nil {
		return "", fmt.Errorf("scroll failed: %w", err)
	}

	// Collect unique values
	valueCounts := make(map[string]int)
	for _, doc := range docs {
		if val, ok := doc.Metadata[key]; ok {
			s := fmt.Sprintf("%v", val)
			if s != "" {
				valueCounts[s]++
			}
		}
	}

	if len(valueCounts) == 0 {
		if workspacePath != "" {
			return fmt.Sprintf("No values found for metadata key '%s' in workspace '%s'. The field may not exist in indexed data.", key, workspacePath), nil
		}
		return fmt.Sprintf("No values found for metadata key '%s'.", key), nil
	}

	// Sort by count descending
	type kv struct {
		Key   string `json:"value"`
		Count int    `json:"count"`
	}
	sorted := make([]kv, 0, len(valueCounts))
	for k, v := range valueCounts {
		sorted = append(sorted, kv{Key: k, Count: v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Count > sorted[j].Count })

	result := map[string]interface{}{
		"key":           key,
		"unique_values": len(sorted),
		"total_docs":    len(docs),
		"values":        sorted,
	}
	if workspacePath != "" {
		result["workspace"] = workspacePath
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}
