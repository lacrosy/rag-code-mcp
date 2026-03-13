package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
)

// mockScrollableMemory extends InMemoryLongTermMemory with scroll capabilities
// needed by the new tools (ScrollAll, ScrollAllWithFilter, ScrollByMetadata,
// SearchCodeOnlyWithFilter, SearchByNameAndType).
type mockScrollableMemory struct {
	docs map[string]memory.Document
}

func newMockScrollableMemory() *mockScrollableMemory {
	return &mockScrollableMemory{docs: make(map[string]memory.Document)}
}

func (m *mockScrollableMemory) Store(ctx context.Context, doc memory.Document) error {
	m.docs[doc.ID] = doc
	return nil
}

func (m *mockScrollableMemory) Search(ctx context.Context, query []float64, limit int) ([]memory.Document, error) {
	return m.allDocs(limit), nil
}

func (m *mockScrollableMemory) SearchDocsOnly(ctx context.Context, query []float64, limit int) ([]memory.Document, error) {
	return nil, nil
}

func (m *mockScrollableMemory) SearchCodeOnly(ctx context.Context, query []float64, limit int) ([]memory.Document, error) {
	return m.allDocs(limit), nil
}

func (m *mockScrollableMemory) SearchCodeOnlyWithFilter(ctx context.Context, query []float64, limit int, filters map[string]string) ([]memory.Document, error) {
	return m.filterDocs(filters, limit), nil
}

func (m *mockScrollableMemory) SearchByNameAndType(ctx context.Context, name string, types []string) ([]memory.Document, error) {
	var result []memory.Document
	for _, doc := range m.docs {
		n, _ := doc.Metadata["name"].(string)
		t, _ := doc.Metadata["type"].(string)
		if n == name {
			for _, tt := range types {
				if t == tt {
					result = append(result, doc)
					break
				}
			}
		}
	}
	return result, nil
}

func (m *mockScrollableMemory) ScrollAll(ctx context.Context, maxResults int) ([]memory.Document, error) {
	return m.allDocs(maxResults), nil
}

func (m *mockScrollableMemory) ScrollAllWithFilter(ctx context.Context, filters map[string]string, maxResults int) ([]memory.Document, error) {
	return m.filterDocs(filters, maxResults), nil
}

func (m *mockScrollableMemory) ScrollByMetadata(ctx context.Context, filters map[string]string, limit int) ([]memory.Document, error) {
	return m.filterDocs(filters, limit), nil
}

func (m *mockScrollableMemory) Delete(ctx context.Context, id string) error {
	delete(m.docs, id)
	return nil
}

func (m *mockScrollableMemory) DeleteByMetadata(ctx context.Context, key, value string) error {
	return nil
}

func (m *mockScrollableMemory) Clear(ctx context.Context) error {
	m.docs = make(map[string]memory.Document)
	return nil
}

func (m *mockScrollableMemory) allDocs(limit int) []memory.Document {
	var result []memory.Document
	for _, doc := range m.docs {
		result = append(result, doc)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

func (m *mockScrollableMemory) filterDocs(filters map[string]string, limit int) []memory.Document {
	var result []memory.Document
	for _, doc := range m.docs {
		match := true
		for k, v := range filters {
			docVal, ok := doc.Metadata[k]
			if !ok || docVal != v {
				match = false
				break
			}
		}
		if match {
			result = append(result, doc)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result
}

// storeChunk is a helper to store a CodeChunk as a document in the mock memory.
func storeChunk(m *mockScrollableMemory, id string, chunk codetypes.CodeChunk, extraMeta map[string]interface{}) {
	b, _ := json.Marshal(chunk)
	meta := map[string]interface{}{
		"name": chunk.Name,
		"type": chunk.Type,
	}
	if chunk.Package != "" {
		meta["package"] = chunk.Package
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	_ = m.Store(context.Background(), memory.Document{
		ID:       id,
		Content:  string(b),
		Metadata: meta,
	})
}

// --- Tests ---

func TestSearchByMetadata_Validation(t *testing.T) {
	tool := NewSearchByMetadataTool(nil)
	ctx := context.Background()

	_, err := tool.Execute(ctx, map[string]interface{}{"file_path": "/tmp/test.php"})
	if err == nil || !strings.Contains(err.Error(), "metadata_filter") {
		t.Fatalf("expected metadata_filter required error, got: %v", err)
	}
}

func TestSearchByMetadata_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "RapydFlow", Type: "class", Package: "Provider\\Rapyd",
	}, map[string]interface{}{"pspi_provider": "rapyd", "pspi_role": "payment_flow"})
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "BitoloFlow", Type: "class", Package: "Provider\\Bitolo",
	}, map[string]interface{}{"pspi_provider": "bitolo", "pspi_role": "payment_flow"})
	storeChunk(mem, "3", codetypes.CodeChunk{
		Name: "RapydMapper", Type: "class", Package: "Provider\\Rapyd",
	}, map[string]interface{}{"pspi_provider": "rapyd", "pspi_role": "status_mapper"})

	tool := NewSearchByMetadataTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"metadata_filter": map[string]interface{}{"pspi_provider": "rapyd"},
		"file_path":       "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// Should find 2 rapyd classes
	var results []codetypes.SymbolDescriptor
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results for rapyd, got %d", len(results))
	}
}

func TestListMetadataValues_Validation(t *testing.T) {
	tool := NewListMetadataValuesTool(nil)
	ctx := context.Background()

	_, err := tool.Execute(ctx, map[string]interface{}{"file_path": "/tmp/test.php"})
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("expected key required error, got: %v", err)
	}
}

func TestListMetadataValues_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{Name: "A", Type: "class"}, map[string]interface{}{"pspi_role": "payment_flow"})
	storeChunk(mem, "2", codetypes.CodeChunk{Name: "B", Type: "class"}, map[string]interface{}{"pspi_role": "payment_flow"})
	storeChunk(mem, "3", codetypes.CodeChunk{Name: "C", Type: "class"}, map[string]interface{}{"pspi_role": "status_mapper"})

	tool := NewListMetadataValuesTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"key":       "pspi_role",
		"file_path": "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, "payment_flow") || !strings.Contains(out, "status_mapper") {
		t.Errorf("expected both roles in output, got: %s", out)
	}
	if !strings.Contains(out, `"unique_values": 2`) {
		t.Errorf("expected 2 unique values, got: %s", out)
	}
}

func TestListMetadataValues_WithFilter(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{Name: "A", Type: "class"}, map[string]interface{}{"pspi_provider": "rapyd", "pspi_role": "payment_flow"})
	storeChunk(mem, "2", codetypes.CodeChunk{Name: "B", Type: "class"}, map[string]interface{}{"pspi_provider": "rapyd", "pspi_role": "status_mapper"})
	storeChunk(mem, "3", codetypes.CodeChunk{Name: "C", Type: "class"}, map[string]interface{}{"pspi_provider": "bitolo", "pspi_role": "payment_flow"})

	tool := NewListMetadataValuesTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"key":             "pspi_role",
		"metadata_filter": map[string]interface{}{"pspi_provider": "rapyd"},
		"file_path":       "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// Should only find roles for rapyd: payment_flow and status_mapper
	if !strings.Contains(out, "payment_flow") || !strings.Contains(out, "status_mapper") {
		t.Errorf("expected rapyd roles in output, got: %s", out)
	}
}

func TestGetWorkspaceStats_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{Name: "A", Type: "class"}, map[string]interface{}{"pspi_provider": "rapyd"})
	storeChunk(mem, "2", codetypes.CodeChunk{Name: "B", Type: "method"}, map[string]interface{}{"pspi_provider": "rapyd"})
	storeChunk(mem, "3", codetypes.CodeChunk{Name: "C", Type: "class"}, map[string]interface{}{"pspi_provider": "bitolo"})

	tool := NewGetWorkspaceStatsTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{"file_path": "/tmp/test.php"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, `"total_chunks": 3`) {
		t.Errorf("expected 3 total chunks, got: %s", out)
	}
	if !strings.Contains(out, "pspi_provider") {
		t.Errorf("expected pspi_provider in metadata fields, got: %s", out)
	}
}

func TestGetClassHierarchy_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "AbstractFlow", Type: "class", Package: "App",
		Signature: "class AbstractFlow implements FlowInterface",
	}, nil)
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "RapydFlow", Type: "class", Package: "Provider\\Rapyd",
		Signature: "class RapydFlow extends AbstractFlow",
	}, nil)
	storeChunk(mem, "3", codetypes.CodeChunk{
		Name: "BitoloFlow", Type: "class", Package: "Provider\\Bitolo",
		Signature: "class BitoloFlow extends AbstractFlow",
	}, nil)

	tool := NewGetClassHierarchyTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"class_name": "AbstractFlow",
		"file_path":  "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, "RapydFlow") || !strings.Contains(out, "BitoloFlow") {
		t.Errorf("expected children in output, got: %s", out)
	}
	if !strings.Contains(out, "FlowInterface") {
		t.Errorf("expected implements in output, got: %s", out)
	}
}

func TestGetClassHierarchy_NotFound(t *testing.T) {
	mem := newMockScrollableMemory()
	tool := NewGetClassHierarchyTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"class_name": "NonExistent",
		"file_path":  "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected not found message, got: %s", out)
	}
}

func TestFindBySignature_ReturnType(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "process", Type: "method", Package: "App",
		Signature: "public function process(): PaymentResponse",
		Code:      "function process(): PaymentResponse {}",
	}, nil)
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "validate", Type: "method", Package: "App",
		Signature: "public function validate(): bool",
		Code:      "function validate(): bool {}",
	}, nil)

	tool := NewFindBySignatureTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"return_type": "PaymentResponse",
		"file_path":   "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, "process") {
		t.Errorf("expected process in results, got: %s", out)
	}
	if strings.Contains(out, "validate") {
		t.Errorf("did not expect validate in results, got: %s", out)
	}
}

func TestFindBySignature_ParamType(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "handle", Type: "method", Package: "App",
		Signature: "public function handle(Request $request): void",
	}, nil)
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "other", Type: "method", Package: "App",
		Signature: "public function other(int $id): void",
	}, nil)

	tool := NewFindBySignatureTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"param_type": "Request",
		"file_path":  "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, "handle") {
		t.Errorf("expected handle in results, got: %s", out)
	}
}

func TestFindBySignature_Validation(t *testing.T) {
	tool := NewFindBySignatureTool(nil)
	ctx := context.Background()

	_, err := tool.Execute(ctx, map[string]interface{}{"file_path": "/tmp/test.php"})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("expected validation error, got: %v", err)
	}
}

func TestCompareClasses_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "ClassA", Type: "class", Package: "App", FilePath: "/tmp/a.php",
		Signature: "class ClassA",
	}, map[string]interface{}{"pspi_provider": "rapyd"})
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "execute", Type: "method", Package: "App", FilePath: "/tmp/a.php",
		Signature: "public function execute(): void",
	}, nil)
	storeChunk(mem, "3", codetypes.CodeChunk{
		Name: "onlyA", Type: "method", Package: "App", FilePath: "/tmp/a.php",
		Signature: "public function onlyA(): void",
	}, nil)

	storeChunk(mem, "4", codetypes.CodeChunk{
		Name: "ClassB", Type: "class", Package: "App", FilePath: "/tmp/b.php",
		Signature: "class ClassB",
	}, map[string]interface{}{"pspi_provider": "bitolo"})
	storeChunk(mem, "5", codetypes.CodeChunk{
		Name: "execute", Type: "method", Package: "App", FilePath: "/tmp/b.php",
		Signature: "public function execute(): Response",
	}, nil)
	storeChunk(mem, "6", codetypes.CodeChunk{
		Name: "onlyB", Type: "method", Package: "App", FilePath: "/tmp/b.php",
		Signature: "public function onlyB(): void",
	}, nil)

	tool := NewCompareClassesTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"class_a":   "ClassA",
		"class_b":   "ClassB",
		"file_path": "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, "execute") {
		t.Errorf("expected common method 'execute' in output, got: %s", out)
	}
	if !strings.Contains(out, "onlyA") {
		t.Errorf("expected 'onlyA' in only_in_a, got: %s", out)
	}
	if !strings.Contains(out, "onlyB") {
		t.Errorf("expected 'onlyB' in only_in_b, got: %s", out)
	}
}

func TestCompareClasses_NotFound(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "ClassA", Type: "class", Package: "App",
	}, nil)

	tool := NewCompareClassesTool(mem)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"class_a":   "ClassA",
		"class_b":   "Missing",
		"file_path": "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected not found message, got: %s", out)
	}
}

func TestGetCallGraph_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "main", Type: "function", Package: "app",
		Code: "func main() { process(); validate(); }",
	}, nil)
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "process", Type: "function", Package: "app",
		Code: "func process() { doStuff(); }",
	}, nil)
	storeChunk(mem, "3", codetypes.CodeChunk{
		Name: "validate", Type: "function", Package: "app",
		Code: "func validate() { return true; }",
	}, nil)
	storeChunk(mem, "4", codetypes.CodeChunk{
		Name: "doStuff", Type: "function", Package: "app",
		Code: "func doStuff() {}",
	}, nil)

	tool := NewGetCallGraphTool(mem, &mockProvider{})
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"symbol_name": "main",
		"file_path":   "/tmp/test.go",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// main calls process() and validate()
	if !strings.Contains(out, "process") || !strings.Contains(out, "validate") {
		t.Errorf("expected callees process and validate, got: %s", out)
	}
}

func TestGetCallGraph_WithDepth2(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "main", Type: "function", Package: "app",
		Code: "func main() { process(); }",
	}, nil)
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "process", Type: "function", Package: "app",
		Code: "func process() { doStuff(); }",
	}, nil)
	storeChunk(mem, "3", codetypes.CodeChunk{
		Name: "doStuff", Type: "function", Package: "app",
		Code: "func doStuff() {}",
	}, nil)

	tool := NewGetCallGraphTool(mem, &mockProvider{})
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"symbol_name": "main",
		"depth":       float64(2),
		"file_path":   "/tmp/test.go",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	// Level 2: process -> doStuff
	if !strings.Contains(out, "level2_callees") {
		t.Errorf("expected level2_callees in output, got: %s", out)
	}
	if !strings.Contains(out, "doStuff") {
		t.Errorf("expected doStuff in level2 callees, got: %s", out)
	}
}

func TestGetCallGraph_NotFound(t *testing.T) {
	mem := newMockScrollableMemory()
	tool := NewGetCallGraphTool(mem, &mockProvider{})
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"symbol_name": "nonexistent",
		"file_path":   "/tmp/test.go",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected not found message, got: %s", out)
	}
}

func TestBatchSearch_HappyPath(t *testing.T) {
	mem := newMockScrollableMemory()
	storeChunk(mem, "1", codetypes.CodeChunk{
		Name: "processPayment", Type: "method", Package: "App",
	}, nil)
	storeChunk(mem, "2", codetypes.CodeChunk{
		Name: "validateRequest", Type: "method", Package: "App",
	}, nil)

	tool := NewBatchSearchTool(mem, &mockProvider{})
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]interface{}{
		"queries": []interface{}{
			map[string]interface{}{"query": "payment processing", "limit": float64(2)},
			map[string]interface{}{"query": "validation logic", "limit": float64(2)},
		},
		"file_path": "/tmp/test.php",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(out, "batch_results") {
		t.Errorf("expected batch_results in output, got: %s", out)
	}
	if !strings.Contains(out, `"total_queries": 2`) {
		t.Errorf("expected 2 total queries, got: %s", out)
	}
}

func TestBatchSearch_Validation(t *testing.T) {
	tool := NewBatchSearchTool(nil, &mockProvider{})
	ctx := context.Background()

	_, err := tool.Execute(ctx, map[string]interface{}{"file_path": "/tmp/test.php"})
	if err == nil || !strings.Contains(err.Error(), "queries") {
		t.Fatalf("expected queries required error, got: %v", err)
	}

	_, err = tool.Execute(ctx, map[string]interface{}{
		"queries":   []interface{}{},
		"file_path": "/tmp/test.php",
	})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("expected at least one query error, got: %v", err)
	}
}

func TestExtractMetadataFilter(t *testing.T) {
	// map[string]interface{} (from JSON)
	result := extractMetadataFilter(map[string]interface{}{
		"metadata_filter": map[string]interface{}{
			"pspi_provider": "rapyd",
			"pspi_role":     "payment_flow",
		},
	})
	if len(result) != 2 || result["pspi_provider"] != "rapyd" || result["pspi_role"] != "payment_flow" {
		t.Errorf("unexpected result: %v", result)
	}

	// nil
	result = extractMetadataFilter(map[string]interface{}{})
	if result != nil {
		t.Errorf("expected nil for missing filter, got: %v", result)
	}

	// empty object
	result = extractMetadataFilter(map[string]interface{}{
		"metadata_filter": map[string]interface{}{},
	})
	if result != nil {
		t.Errorf("expected nil for empty filter, got: %v", result)
	}
}

func TestParseInheritanceFromSignature(t *testing.T) {
	tests := []struct {
		sig        string
		extends    string
		implements []string
	}{
		{"class Foo extends Bar", "Bar", nil},
		{"class Foo extends Bar implements Baz, Qux", "Bar", []string{"Baz", "Qux"}},
		{"class Foo implements IA, IB", "", []string{"IA", "IB"}},
		{"class Foo", "", nil},
		{"", "", nil},
	}

	for _, tc := range tests {
		ext, impls := parseInheritanceFromSignature(tc.sig)
		if ext != tc.extends {
			t.Errorf("sig=%q: expected extends=%q, got %q", tc.sig, tc.extends, ext)
		}
		if len(impls) != len(tc.implements) {
			t.Errorf("sig=%q: expected %d implements, got %d (%v)", tc.sig, len(tc.implements), len(impls), impls)
			continue
		}
		for i, impl := range impls {
			if impl != tc.implements[i] {
				t.Errorf("sig=%q: implements[%d] expected %q, got %q", tc.sig, i, tc.implements[i], impl)
			}
		}
	}
}
