package tools

import (
	"context"
	"fmt"

	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// resolveWorkspaceMemory detects workspace from params and returns the appropriate memory backend.
// Returns (memory, workspacePath, collectionName, error).
func resolveWorkspaceMemory(ctx context.Context, wm *workspace.Manager, fallback memory.LongTermMemory, params map[string]interface{}) (memory.LongTermMemory, string, string, error) {
	filePath := extractFilePathFromParams(params)

	if wm != nil {
		workspaceInfo, err := wm.DetectWorkspace(params)
		if err == nil && workspaceInfo != nil {
			language := inferLanguageFromPath(filePath)
			if language == "" && len(workspaceInfo.Languages) > 0 {
				language = workspaceInfo.Languages[0]
			}
			if language == "" {
				language = workspaceInfo.ProjectType
			}

			collectionName := workspaceInfo.CollectionNameForLanguage(language)
			mem, err := wm.GetMemoryForWorkspaceLanguage(ctx, workspaceInfo, language)
			if err == nil && mem != nil {
				indexKey := workspaceInfo.ID + "-" + language
				if wm.IsIndexing(indexKey) {
					return nil, "", "", fmt.Errorf("workspace '%s' language '%s' is currently being indexed, try again later", workspaceInfo.Root, language)
				}

				if msg, err := CheckCollectionStatus(ctx, mem, collectionName, workspaceInfo.Root); err != nil || msg != "" {
					if err != nil {
						return nil, "", "", err
					}
					return nil, "", "", fmt.Errorf("%s", msg)
				}

				return mem, workspaceInfo.Root, collectionName, nil
			}
		}
	}

	return fallback, "", "", nil
}

// CheckCollectionStatus verifies if a collection exists and has data.
// Returns an error message if the collection is missing or empty, nil otherwise.
func CheckCollectionStatus(ctx context.Context, mem memory.LongTermMemory, collectionName, workspacePath string) (string, error) {
	// Type assertion to check if this memory supports collection existence checking
	type CollectionChecker interface {
		CollectionExists(ctx context.Context, name string) (bool, error)
	}

	if checker, ok := mem.(CollectionChecker); ok {
		exists, err := checker.CollectionExists(ctx, collectionName)
		if err != nil {
			return "", fmt.Errorf("failed to check collection status: %w", err)
		}

		if !exists {
			// Collection doesn't exist - tell AI to index first
			return fmt.Sprintf("❌ Workspace '%s' is not indexed yet.\n\n"+
				"To enable this operation, please call the 'index_workspace' tool first with:\n"+
				"{\n"+
				"  \"file_path\": \"%s\"\n"+
				"}\n\n"+
				"Details:\n"+
				"- Workspace: %s\n"+
				"- Collection: %s (not created yet)\n",
				workspacePath,
				workspacePath,
				workspacePath,
				collectionName), nil
		}
	}

	return "", nil
}

// CheckSearchResults verifies if search returned any results.
// Returns an error message if no results found, nil otherwise.
func CheckSearchResults(resultCount int, collectionName, workspacePath string) (string, error) {
	if resultCount == 0 {
		// Collection exists but is empty - tell AI to index
		return fmt.Sprintf("❌ Workspace '%s' appears to be empty or not fully indexed.\n\n"+
			"To enable this operation, please call the 'index_workspace' tool with:\n"+
			"{\n"+
			"  \"file_path\": \"%s\"\n"+
			"}\n\n"+
			"Details:\n"+
			"- Workspace: %s\n"+
			"- Collection: %s (exists but may be empty)\n",
			workspacePath,
			workspacePath,
			workspacePath,
			collectionName), nil
	}

	return "", nil
}
