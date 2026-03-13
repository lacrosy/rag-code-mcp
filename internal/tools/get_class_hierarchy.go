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

// GetClassHierarchyTool shows the inheritance chain for a class: parent, interfaces, siblings.
type GetClassHierarchyTool struct {
	memory           memory.LongTermMemory
	workspaceManager *workspace.Manager
}

func NewGetClassHierarchyTool(mem memory.LongTermMemory) *GetClassHierarchyTool {
	return &GetClassHierarchyTool{memory: mem}
}

func (t *GetClassHierarchyTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *GetClassHierarchyTool) Name() string { return "get_class_hierarchy" }

func (t *GetClassHierarchyTool) Description() string {
	return "Show inheritance hierarchy for a class: parent class (extends), interfaces (implements), " +
		"and sibling classes that implement the same interfaces. " +
		"Use to understand class relationships and find alternative implementations."
}

func (t *GetClassHierarchyTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	className, ok := params["class_name"].(string)
	if !ok || strings.TrimSpace(className) == "" {
		return "", fmt.Errorf("class_name parameter is required")
	}

	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for get_class_hierarchy")
	}

	searchMemory, workspacePath, _, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	// 1. Find the target class
	type ExactSearcher interface {
		SearchByNameAndType(ctx context.Context, name string, types []string) ([]memory.Document, error)
	}

	exactSearcher, ok := searchMemory.(ExactSearcher)
	if !ok {
		return "", fmt.Errorf("storage backend does not support exact search")
	}

	classResults, err := exactSearcher.SearchByNameAndType(ctx, className, []string{"class", "interface", "trait"})
	if err != nil {
		return "", fmt.Errorf("failed to find class: %w", err)
	}

	if len(classResults) == 0 {
		return fmt.Sprintf("Class '%s' not found in index.", className), nil
	}

	// Parse the target class chunk
	var targetChunk codetypes.CodeChunk
	if err := json.Unmarshal([]byte(classResults[0].Content), &targetChunk); err != nil {
		return "", fmt.Errorf("failed to parse class chunk: %w", err)
	}

	// Extract extends/implements from signature
	extends, implements := parseInheritanceFromSignature(targetChunk.Signature)

	// 2. Scroll all class-type chunks to find siblings, children, parents
	type AllScroller interface {
		ScrollAll(ctx context.Context, maxResults int) ([]memory.Document, error)
	}

	scroller, ok := searchMemory.(AllScroller)
	if !ok {
		return "", fmt.Errorf("storage backend does not support scroll operations")
	}

	allDocs, err := scroller.ScrollAll(ctx, 5000)
	if err != nil {
		return "", fmt.Errorf("scroll failed: %w", err)
	}

	// Parse all class chunks
	type classInfo struct {
		Name       string   `json:"name"`
		Type       string   `json:"type"`
		Package    string   `json:"package,omitempty"`
		FilePath   string   `json:"file_path,omitempty"`
		Extends    string   `json:"extends,omitempty"`
		Implements []string `json:"implements,omitempty"`
	}

	var children []classInfo
	var siblings []classInfo // classes implementing same interfaces
	var parentInfo *classInfo

	for _, doc := range allDocs {
		var chunk codetypes.CodeChunk
		if err := json.Unmarshal([]byte(doc.Content), &chunk); err != nil {
			continue
		}
		if chunk.Type != "class" && chunk.Type != "interface" && chunk.Type != "trait" {
			continue
		}
		if chunk.Name == className {
			continue
		}

		chExtends, chImplements := parseInheritanceFromSignature(chunk.Signature)

		ci := classInfo{
			Name:       chunk.Name,
			Type:       chunk.Type,
			Package:    chunk.Package,
			FilePath:   chunk.FilePath,
			Extends:    chExtends,
			Implements: chImplements,
		}

		// Is this a child (extends our class)?
		if chExtends == className || strings.HasSuffix(chExtends, "\\"+className) {
			children = append(children, ci)
		}

		// Is this the parent?
		if extends != "" && (chunk.Name == extends || strings.HasSuffix(chunk.Package+"\\"+chunk.Name, extends)) {
			parentInfo = &ci
		}

		// Is this a sibling (implements same interface)?
		if len(implements) > 0 && len(chImplements) > 0 {
			for _, iface := range implements {
				for _, chIface := range chImplements {
					if iface == chIface || shortName(iface) == shortName(chIface) {
						siblings = append(siblings, ci)
						goto nextDoc
					}
				}
			}
		}
	nextDoc:
	}

	// Build result
	result := map[string]interface{}{
		"class":      className,
		"type":       targetChunk.Type,
		"package":    targetChunk.Package,
		"file_path":  targetChunk.FilePath,
		"extends":    extends,
		"implements": implements,
	}
	if workspacePath != "" {
		result["workspace"] = workspacePath
	}
	if parentInfo != nil {
		result["parent"] = parentInfo
	}
	if len(children) > 0 {
		result["children"] = children
	}
	if len(siblings) > 0 {
		result["siblings"] = siblings
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

func parseInheritanceFromSignature(sig string) (extends string, implements []string) {
	if sig == "" {
		return "", nil
	}
	lower := strings.ToLower(sig)

	if idx := strings.Index(lower, " extends "); idx >= 0 {
		rest := sig[idx+len(" extends "):]
		if implIdx := strings.Index(strings.ToLower(rest), " implements "); implIdx >= 0 {
			extends = strings.TrimSpace(rest[:implIdx])
			implStr := rest[implIdx+len(" implements "):]
			for _, impl := range strings.Split(implStr, ",") {
				impl = strings.TrimSpace(impl)
				if impl != "" {
					implements = append(implements, impl)
				}
			}
		} else {
			extends = strings.TrimSpace(rest)
		}
	} else if idx := strings.Index(lower, " implements "); idx >= 0 {
		implStr := sig[idx+len(" implements "):]
		for _, impl := range strings.Split(implStr, ",") {
			impl = strings.TrimSpace(impl)
			if impl != "" {
				implements = append(implements, impl)
			}
		}
	}
	return
}

func shortName(fqn string) string {
	if idx := strings.LastIndex(fqn, "\\"); idx >= 0 {
		return fqn[idx+1:]
	}
	return fqn
}
