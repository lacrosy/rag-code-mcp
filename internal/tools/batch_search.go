package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/llm"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// BatchSearchTool performs multiple semantic searches in a single call.
type BatchSearchTool struct {
	memory           memory.LongTermMemory
	embedder         llm.Provider
	workspaceManager *workspace.Manager
}

func NewBatchSearchTool(mem memory.LongTermMemory, embedder llm.Provider) *BatchSearchTool {
	return &BatchSearchTool{memory: mem, embedder: embedder}
}

func (t *BatchSearchTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *BatchSearchTool) Name() string { return "batch_search" }

func (t *BatchSearchTool) Description() string {
	return "Execute multiple semantic code searches in a single call. " +
		"Each query runs independently and returns its own results. " +
		"Use to reduce round-trips when you need to search for several related things. " +
		"Each query can have its own metadata_filter."
}

type batchQuery struct {
	Query          string            `json:"query"`
	MetadataFilter map[string]string `json:"metadata_filter,omitempty"`
	Limit          int               `json:"limit,omitempty"`
}

type batchResult struct {
	Query   string                 `json:"query"`
	Results []map[string]interface{} `json:"results"`
	Error   string                 `json:"error,omitempty"`
}

func (t *BatchSearchTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for batch_search")
	}

	// Parse queries
	rawQueries, ok := params["queries"]
	if !ok {
		return "", fmt.Errorf("queries parameter is required (array of {query, metadata_filter?, limit?})")
	}

	var queries []batchQuery
	switch v := rawQueries.(type) {
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				q := batchQuery{Limit: 3}
				if s, ok := m["query"].(string); ok {
					q.Query = s
				}
				if l, ok := m["limit"].(float64); ok && l > 0 {
					q.Limit = int(l)
				}
				if mf, ok := m["metadata_filter"].(map[string]interface{}); ok {
					q.MetadataFilter = make(map[string]string)
					for k, val := range mf {
						if s, ok := val.(string); ok {
							q.MetadataFilter[k] = s
						}
					}
				}
				if q.Query != "" {
					queries = append(queries, q)
				}
			}
		}
	default:
		return "", fmt.Errorf("queries must be an array of objects")
	}

	if len(queries) == 0 {
		return "", fmt.Errorf("at least one query is required")
	}
	if len(queries) > 10 {
		return "", fmt.Errorf("maximum 10 queries per batch")
	}

	searchMemory, workspacePath, _, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	type FilteredSearcher interface {
		SearchCodeOnlyWithFilter(ctx context.Context, query []float64, limit int, filters map[string]string) ([]memory.Document, error)
	}
	type CodeSearcher interface {
		SearchCodeOnly(ctx context.Context, query []float64, limit int) ([]memory.Document, error)
	}

	var results []batchResult
	for _, q := range queries {
		br := batchResult{Query: q.Query}

		embedding, err := t.embedder.Embed(ctx, q.Query)
		if err != nil {
			br.Error = fmt.Sprintf("embedding failed: %v", err)
			results = append(results, br)
			continue
		}

		var docs []memory.Document
		var searchErr error

		if len(q.MetadataFilter) > 0 {
			if fs, ok := searchMemory.(FilteredSearcher); ok {
				docs, searchErr = fs.SearchCodeOnlyWithFilter(ctx, embedding, q.Limit, q.MetadataFilter)
			} else if cs, ok := searchMemory.(CodeSearcher); ok {
				docs, searchErr = cs.SearchCodeOnly(ctx, embedding, q.Limit)
			} else {
				docs, searchErr = searchMemory.Search(ctx, embedding, q.Limit)
			}
		} else if cs, ok := searchMemory.(CodeSearcher); ok {
			docs, searchErr = cs.SearchCodeOnly(ctx, embedding, q.Limit)
		} else {
			docs, searchErr = searchMemory.Search(ctx, embedding, q.Limit)
		}

		if searchErr != nil {
			br.Error = fmt.Sprintf("search failed: %v", searchErr)
			results = append(results, br)
			continue
		}

		descriptors := buildSymbolDescriptorsFromDocs(docs)
		for _, desc := range descriptors {
			entry := map[string]interface{}{
				"name":      desc.Name,
				"kind":      desc.Kind,
				"package":   desc.Package,
				"signature": desc.Signature,
			}
			if desc.Location.FilePath != "" {
				entry["file_path"] = desc.Location.FilePath
				entry["start_line"] = desc.Location.StartLine
			}
			if desc.Metadata != nil {
				// Include custom metadata (skip builtin)
				for k, v := range desc.Metadata {
					if strings.HasPrefix(k, "pspi_") || strings.HasPrefix(k, "symfony_") {
						entry[k] = v
					}
				}
			}
			br.Results = append(br.Results, entry)
		}

		results = append(results, br)
	}

	output := map[string]interface{}{
		"batch_results": results,
		"total_queries": len(queries),
	}
	if workspacePath != "" {
		output["workspace"] = workspacePath
	}

	data, _ := json.MarshalIndent(output, "", "  ")
	return string(data), nil
}
