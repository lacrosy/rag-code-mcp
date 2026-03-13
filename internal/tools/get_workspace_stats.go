package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// GetWorkspaceStatsTool provides an overview of the indexed workspace.
type GetWorkspaceStatsTool struct {
	memory           memory.LongTermMemory
	workspaceManager *workspace.Manager
}

func NewGetWorkspaceStatsTool(mem memory.LongTermMemory) *GetWorkspaceStatsTool {
	return &GetWorkspaceStatsTool{memory: mem}
}

func (t *GetWorkspaceStatsTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *GetWorkspaceStatsTool) Name() string { return "get_workspace_stats" }

func (t *GetWorkspaceStatsTool) Description() string {
	return "Get overview of indexed workspace: total chunks, distribution by type (class/method/function), " +
		"available metadata keys with unique value counts, and top values for each custom metadata field. " +
		"Use as first step to understand what's in the index before searching."
}

func (t *GetWorkspaceStatsTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for get_workspace_stats")
	}

	searchMemory, workspacePath, collectionName, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	type AllScroller interface {
		ScrollAll(ctx context.Context, maxResults int) ([]memory.Document, error)
	}

	scroller, ok := searchMemory.(AllScroller)
	if !ok {
		return "", fmt.Errorf("storage backend does not support scroll operations")
	}

	docs, err := scroller.ScrollAll(ctx, 10000)
	if err != nil {
		return "", fmt.Errorf("scroll failed: %w", err)
	}

	if len(docs) == 0 {
		return fmt.Sprintf("Workspace '%s' has no indexed code chunks (collection: %s).", workspacePath, collectionName), nil
	}

	// Aggregate stats
	typeCounts := make(map[string]int)
	langCounts := make(map[string]int)
	// Track custom metadata keys (skip built-in ones)
	builtinKeys := map[string]bool{
		"content": true, "file": true, "package": true, "name": true,
		"type": true, "signature": true, "start_line": true, "end_line": true,
		"source": true, "basename": true, "chunk_type": true, "score": true,
	}
	customKeyValues := make(map[string]map[string]int) // key -> value -> count

	for _, doc := range docs {
		if t, ok := doc.Metadata["type"]; ok {
			typeCounts[fmt.Sprintf("%v", t)]++
		}
		if l, ok := doc.Metadata["language"]; ok {
			s := fmt.Sprintf("%v", l)
			if s != "" {
				langCounts[s]++
			}
		}

		for k, v := range doc.Metadata {
			if builtinKeys[k] {
				continue
			}
			s := fmt.Sprintf("%v", v)
			if s == "" || s == "0" || s == "<nil>" {
				continue
			}
			if customKeyValues[k] == nil {
				customKeyValues[k] = make(map[string]int)
			}
			customKeyValues[k][s]++
		}
	}

	// Build result
	type metaKeyInfo struct {
		Key          string   `json:"key"`
		UniqueValues int      `json:"unique_values"`
		TopValues    []string `json:"top_values"`
	}

	metaKeys := make([]metaKeyInfo, 0, len(customKeyValues))
	for k, vals := range customKeyValues {
		// Sort values by count, take top 10
		type kv struct {
			k string
			v int
		}
		sorted := make([]kv, 0, len(vals))
		for vk, vc := range vals {
			sorted = append(sorted, kv{vk, vc})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })

		topN := 10
		if len(sorted) < topN {
			topN = len(sorted)
		}
		top := make([]string, topN)
		for i := 0; i < topN; i++ {
			top[i] = fmt.Sprintf("%s (%d)", sorted[i].k, sorted[i].v)
		}

		metaKeys = append(metaKeys, metaKeyInfo{
			Key:          k,
			UniqueValues: len(vals),
			TopValues:    top,
		})
	}
	sort.Slice(metaKeys, func(i, j int) bool { return metaKeys[i].Key < metaKeys[j].Key })

	result := map[string]interface{}{
		"workspace":       workspacePath,
		"collection":      collectionName,
		"total_chunks":    len(docs),
		"by_type":         typeCounts,
		"by_language":     langCounts,
		"metadata_fields": metaKeys,
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}
