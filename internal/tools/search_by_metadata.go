package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// SearchByMetadataTool searches for code chunks by exact metadata field matches
// without vector similarity. Useful for finding all classes with a specific role,
// provider, component type, etc.
type SearchByMetadataTool struct {
	memory           memory.LongTermMemory
	workspaceManager *workspace.Manager
}

// NewSearchByMetadataTool creates a new metadata search tool.
func NewSearchByMetadataTool(mem memory.LongTermMemory) *SearchByMetadataTool {
	return &SearchByMetadataTool{
		memory: mem,
	}
}

// SetWorkspaceManager sets the workspace manager for workspace-aware searching.
func (t *SearchByMetadataTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

// Name returns the MCP tool name.
func (t *SearchByMetadataTool) Name() string { return "search_by_metadata" }

// Description provides a description for the tool.
func (t *SearchByMetadataTool) Description() string {
	return "Find code by exact metadata field values - no semantic search, just precise filtering. " +
		"Use when you know specific metadata keys (e.g., pspi_provider, pspi_role, pspi_component_type, type, package). " +
		"Returns all matching code chunks. Perfect for queries like 'all payment flow classes' or 'all classes of provider X'."
}

// Execute runs the metadata search.
func (t *SearchByMetadataTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	// Extract metadata_filter (required for this tool)
	metadataFilter := extractMetadataFilter(params)
	if len(metadataFilter) == 0 {
		return "", fmt.Errorf("metadata_filter parameter is required. Provide a JSON object with key-value pairs, e.g. {\"pspi_role\": \"payment_flow\"}")
	}

	limit := 50
	if v, ok := params["limit"].(float64); ok && v > 0 {
		limit = int(v)
	} else if v, ok := params["limit"].(int); ok && v > 0 {
		limit = v
	}

	outputFormat := "json"
	if of, ok := params["output_format"].(string); ok && of != "" {
		outputFormat = strings.ToLower(of)
	}

	// file_path is required for workspace detection
	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for search_by_metadata. Please provide a file path from your workspace")
	}

	// Workspace detection
	var searchMemory memory.LongTermMemory
	var workspacePath string
	var collectionName string

	if t.workspaceManager != nil {
		workspaceInfo, err := t.workspaceManager.DetectWorkspace(params)
		if err == nil && workspaceInfo != nil {
			workspacePath = workspaceInfo.Root

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
				indexKey := workspaceInfo.ID + "-" + language
				if t.workspaceManager.IsIndexing(indexKey) {
					return fmt.Sprintf("Workspace '%s' language '%s' is currently being indexed. Please try again later.",
						workspaceInfo.Root, language), nil
				}

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

	if searchMemory == nil {
		searchMemory = t.memory
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured for search_by_metadata")
	}

	// Type assertion for ScrollByMetadata
	type MetadataScroller interface {
		ScrollByMetadata(ctx context.Context, filters map[string]string, limit int) ([]memory.Document, error)
	}

	scroller, ok := searchMemory.(MetadataScroller)
	if !ok {
		return "", fmt.Errorf("the configured storage backend does not support metadata-only search")
	}

	docs, err := scroller.ScrollByMetadata(ctx, metadataFilter, limit)
	if err != nil {
		return "", fmt.Errorf("metadata search failed: %w", err)
	}

	if len(docs) == 0 {
		filterDesc := formatFilterDescription(metadataFilter)
		if workspacePath != "" {
			return fmt.Sprintf("No code found matching %s in workspace '%s' (collection: %s).",
				filterDesc, workspacePath, collectionName), nil
		}
		return fmt.Sprintf("No code found matching %s.", filterDesc), nil
	}

	if outputFormat == "markdown" {
		return formatMetadataResults(docs, metadataFilter, workspacePath), nil
	}

	descriptors := buildSymbolDescriptorsFromDocs(docs)
	data, err := json.MarshalIndent(descriptors, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal search_by_metadata results: %w", err)
	}
	return string(data), nil
}

func formatFilterDescription(filters map[string]string) string {
	parts := make([]string, 0, len(filters))
	for k, v := range filters {
		parts = append(parts, fmt.Sprintf("%s=%q", k, v))
	}
	return strings.Join(parts, ", ")
}

func formatMetadataResults(docs []memory.Document, filters map[string]string, workspacePath string) string {
	var sb strings.Builder
	filterDesc := formatFilterDescription(filters)
	if workspacePath != "" {
		sb.WriteString(fmt.Sprintf("Found %d result(s) matching %s in workspace '%s':\n\n", len(docs), filterDesc, workspacePath))
	} else {
		sb.WriteString(fmt.Sprintf("Found %d result(s) matching %s:\n\n", len(docs), filterDesc))
	}
	for i, doc := range docs {
		sb.WriteString(fmt.Sprintf("--- Result %d ---\n%s\n\n", i+1, doc.Content))
	}
	return sb.String()
}
