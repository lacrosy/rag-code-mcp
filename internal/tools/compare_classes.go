package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// CompareClassesTool compares two classes side by side.
type CompareClassesTool struct {
	memory           memory.LongTermMemory
	workspaceManager *workspace.Manager
}

func NewCompareClassesTool(mem memory.LongTermMemory) *CompareClassesTool {
	return &CompareClassesTool{memory: mem}
}

func (t *CompareClassesTool) SetWorkspaceManager(wm *workspace.Manager) {
	t.workspaceManager = wm
}

func (t *CompareClassesTool) Name() string { return "compare_classes" }

func (t *CompareClassesTool) Description() string {
	return "Compare two classes side by side: common methods, methods unique to each class, " +
		"signature differences, and metadata differences. " +
		"Use to understand how two implementations differ (e.g., two provider payment flows)."
}

func (t *CompareClassesTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	classA, _ := params["class_a"].(string)
	classB, _ := params["class_b"].(string)
	if classA == "" || classB == "" {
		return "", fmt.Errorf("both class_a and class_b parameters are required")
	}

	filePath := extractFilePathFromParams(params)
	if filePath == "" {
		return "", fmt.Errorf("file_path parameter is required for compare_classes")
	}

	searchMemory, workspacePath, _, err := resolveWorkspaceMemory(ctx, t.workspaceManager, t.memory, params)
	if err != nil {
		return "", err
	}
	if searchMemory == nil {
		return "", fmt.Errorf("no long-term memory configured")
	}

	// Scroll all to find both classes and their methods
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

	// Collect class info and methods for both classes
	type methodInfo struct {
		Name      string `json:"name"`
		Signature string `json:"signature"`
		StartLine int    `json:"start_line"`
	}

	type classData struct {
		Found     bool
		Chunk     codetypes.CodeChunk
		Metadata  map[string]interface{}
		Methods   map[string]methodInfo // name -> info
		FilePath  string
	}

	a := classData{Methods: make(map[string]methodInfo)}
	b := classData{Methods: make(map[string]methodInfo)}

	for _, doc := range allDocs {
		var chunk codetypes.CodeChunk
		if err := json.Unmarshal([]byte(doc.Content), &chunk); err != nil {
			continue
		}

		if chunk.Type == "class" || chunk.Type == "interface" || chunk.Type == "trait" {
			if chunk.Name == classA {
				a.Found = true
				a.Chunk = chunk
				a.Metadata = doc.Metadata
				a.FilePath = chunk.FilePath
			} else if chunk.Name == classB {
				b.Found = true
				b.Chunk = chunk
				b.Metadata = doc.Metadata
				b.FilePath = chunk.FilePath
			}
		}

		if chunk.Type == "method" {
			if chunk.FilePath != "" {
				if a.FilePath != "" && chunk.FilePath == a.FilePath {
					a.Methods[chunk.Name] = methodInfo{
						Name:      chunk.Name,
						Signature: chunk.Signature,
						StartLine: chunk.StartLine,
					}
				}
				if b.FilePath != "" && chunk.FilePath == b.FilePath {
					b.Methods[chunk.Name] = methodInfo{
						Name:      chunk.Name,
						Signature: chunk.Signature,
						StartLine: chunk.StartLine,
					}
				}
			}
		}
	}

	if !a.Found {
		return fmt.Sprintf("Class '%s' not found in index.", classA), nil
	}
	if !b.Found {
		return fmt.Sprintf("Class '%s' not found in index.", classB), nil
	}

	// Compare methods
	var commonMethods []map[string]interface{}
	var onlyA []methodInfo
	var onlyB []methodInfo

	allMethodNames := make(map[string]bool)
	for name := range a.Methods {
		allMethodNames[name] = true
	}
	for name := range b.Methods {
		allMethodNames[name] = true
	}

	sortedNames := make([]string, 0, len(allMethodNames))
	for name := range allMethodNames {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	for _, name := range sortedNames {
		mA, inA := a.Methods[name]
		mB, inB := b.Methods[name]

		if inA && inB {
			entry := map[string]interface{}{
				"name":   name,
				"sig_a":  mA.Signature,
				"sig_b":  mB.Signature,
				"same":   mA.Signature == mB.Signature,
			}
			commonMethods = append(commonMethods, entry)
		} else if inA {
			onlyA = append(onlyA, mA)
		} else {
			onlyB = append(onlyB, mB)
		}
	}

	// Compare metadata (custom keys only)
	builtinKeys := map[string]bool{
		"content": true, "file": true, "package": true, "name": true,
		"type": true, "signature": true, "start_line": true, "end_line": true,
		"source": true, "basename": true, "chunk_type": true, "score": true,
		"language": true,
	}

	metaDiffs := make(map[string]map[string]string)
	for k, v := range a.Metadata {
		if builtinKeys[k] {
			continue
		}
		vs := fmt.Sprintf("%v", v)
		if vs == "" || vs == "<nil>" {
			continue
		}
		if metaDiffs[k] == nil {
			metaDiffs[k] = make(map[string]string)
		}
		metaDiffs[k]["a"] = vs
	}
	for k, v := range b.Metadata {
		if builtinKeys[k] {
			continue
		}
		vs := fmt.Sprintf("%v", v)
		if vs == "" || vs == "<nil>" {
			continue
		}
		if metaDiffs[k] == nil {
			metaDiffs[k] = make(map[string]string)
		}
		metaDiffs[k]["b"] = vs
	}

	result := map[string]interface{}{
		"class_a": map[string]interface{}{
			"name":      classA,
			"package":   a.Chunk.Package,
			"file_path": a.Chunk.FilePath,
			"signature": a.Chunk.Signature,
			"methods":   len(a.Methods),
		},
		"class_b": map[string]interface{}{
			"name":      classB,
			"package":   b.Chunk.Package,
			"file_path": b.Chunk.FilePath,
			"signature": b.Chunk.Signature,
			"methods":   len(b.Methods),
		},
		"common_methods":  commonMethods,
		"only_in_a":       onlyA,
		"only_in_b":       onlyB,
		"metadata_comparison": metaDiffs,
	}
	if workspacePath != "" {
		result["workspace"] = workspacePath
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

// matchClassFile checks if a method chunk belongs to a class by checking file path
// and metadata class_name field.
func matchClassFile(doc memory.Document, classFilePath string) bool {
	if classFilePath == "" {
		return false
	}
	if f, ok := doc.Metadata["file"]; ok {
		return fmt.Sprintf("%v", f) == classFilePath
	}
	return false
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}
