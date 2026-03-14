package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
)

// mockSiblingFetcher simulates fetching sibling chunks by parent_id
type mockSiblingFetcher struct {
	docs []memory.Document
}

func (m *mockSiblingFetcher) ScrollByMetadata(_ context.Context, filters map[string]string, _ int) ([]memory.Document, error) {
	pid := filters["parent_id"]
	var result []memory.Document
	for _, doc := range m.docs {
		if metaString(doc.Metadata, "parent_id") == pid {
			result = append(result, doc)
		}
	}
	return result, nil
}

func makePartDoc(name, parentID string, part, total int, code string) memory.Document {
	ch := codetypes.CodeChunk{
		Name:     name,
		Type:     "method",
		FilePath: "src/Service.php",
		Code:     code,
		Metadata: map[string]any{
			"parent_id":   parentID,
			"chunk_part":  partStr(part),
			"chunk_total": partStr(total),
		},
	}
	chJSON, _ := json.Marshal(ch)
	return memory.Document{
		ID:      "doc-" + parentID + "-" + partStr(part),
		Content: string(chJSON),
		Metadata: map[string]interface{}{
			"parent_id":   parentID,
			"chunk_part":  partStr(part),
			"chunk_total": partStr(total),
			"name":        name,
			"score":       0.95,
		},
	}
}

func partStr(n int) string {
	return metaString(map[string]interface{}{"n": n}, "n")
}

func TestReassembleChunkedDocs_NoSplitChunks(t *testing.T) {
	docs := []memory.Document{
		{ID: "1", Content: "test1", Metadata: map[string]interface{}{"name": "foo"}},
		{ID: "2", Content: "test2", Metadata: map[string]interface{}{"name": "bar"}},
	}

	result := reassembleChunkedDocs(context.Background(), docs, nil)
	if len(result) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(result))
	}
}

func TestReassembleChunkedDocs_MergesParts(t *testing.T) {
	part1 := makePartDoc("processPayment", "p123", 1, 3, "line1\nline2\nline3")
	part2 := makePartDoc("processPayment", "p123", 2, 3, "line3\nline4\nline5")
	part3 := makePartDoc("processPayment", "p123", 3, 3, "line5\nline6\nline7")

	fetcher := &mockSiblingFetcher{
		docs: []memory.Document{part1, part2, part3},
	}

	// Search returns only part2 as the hit
	searchResults := []memory.Document{part2}

	result := reassembleChunkedDocs(context.Background(), searchResults, fetcher)

	if len(result) != 1 {
		t.Fatalf("expected 1 merged doc, got %d", len(result))
	}

	// Check the merged content contains code from all parts
	var merged codetypes.CodeChunk
	if err := json.Unmarshal([]byte(result[0].Content), &merged); err != nil {
		t.Fatalf("failed to unmarshal merged chunk: %v", err)
	}

	if merged.Name != "processPayment" {
		t.Errorf("expected name 'processPayment', got %q", merged.Name)
	}

	// Should contain lines from all parts (with overlap removed)
	if !containsSubstring(merged.Code, "line1") {
		t.Error("merged code missing line1 from part1")
	}
	if !containsSubstring(merged.Code, "line7") {
		t.Error("merged code missing line7 from part3")
	}

	// Should have reassembled metadata
	if metaString(result[0].Metadata, "reassembled") != "true" {
		t.Error("expected reassembled=true metadata")
	}

	// parent_id should be cleaned up
	if metaString(result[0].Metadata, "parent_id") != "" {
		t.Error("parent_id should be removed after reassembly")
	}
}

func TestReassembleChunkedDocs_MixedResults(t *testing.T) {
	normalDoc := memory.Document{
		ID:      "normal-1",
		Content: `{"name":"normalMethod","type":"method","code":"return true;"}`,
		Metadata: map[string]interface{}{
			"name": "normalMethod",
		},
	}

	part1 := makePartDoc("bigMethod", "p456", 1, 2, "part1 code")
	part2 := makePartDoc("bigMethod", "p456", 2, 2, "part2 code")

	fetcher := &mockSiblingFetcher{
		docs: []memory.Document{part1, part2},
	}

	searchResults := []memory.Document{normalDoc, part1}

	result := reassembleChunkedDocs(context.Background(), searchResults, fetcher)

	if len(result) != 2 {
		t.Fatalf("expected 2 results (1 normal + 1 merged), got %d", len(result))
	}

	// First should be the normal doc
	if result[0].ID != "normal-1" {
		t.Errorf("expected first result to be normal doc, got ID=%s", result[0].ID)
	}

	// Second should be the merged doc
	if metaString(result[1].Metadata, "reassembled") != "true" {
		t.Error("expected second result to be reassembled")
	}
}

func TestReassembleChunkedDocs_DeduplicatesSameParent(t *testing.T) {
	part1 := makePartDoc("bigMethod", "p789", 1, 2, "code part 1")
	part2 := makePartDoc("bigMethod", "p789", 2, 2, "code part 2")

	fetcher := &mockSiblingFetcher{
		docs: []memory.Document{part1, part2},
	}

	// Both parts appear in search results
	searchResults := []memory.Document{part1, part2}

	result := reassembleChunkedDocs(context.Background(), searchResults, fetcher)

	// Should deduplicate to a single merged result
	if len(result) != 1 {
		t.Fatalf("expected 1 merged doc (deduped), got %d", len(result))
	}
}

func TestFindOverlap(t *testing.T) {
	tests := []struct {
		name     string
		prev     string
		next     string
		wantGt0  bool
	}{
		{"no overlap", "line1\nline2", "line3\nline4", false},
		{"one line overlap", "line1\nline2\nline3", "line3\nline4\nline5", true},
		{"two line overlap", "line1\nline2\nline3", "line2\nline3\nline4", true},
		{"empty prev", "", "line1", false},
		{"empty next", "line1", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findOverlap(tt.prev, tt.next)
			if tt.wantGt0 && got == 0 {
				t.Errorf("expected overlap > 0, got 0")
			}
			if !tt.wantGt0 && got != 0 {
				t.Errorf("expected no overlap, got %d", got)
			}
		})
	}
}

func TestMetaHelpers(t *testing.T) {
	m := map[string]interface{}{
		"str":   "hello",
		"num":   42,
		"float": 3.14,
	}

	if metaString(m, "str") != "hello" {
		t.Error("metaString failed for string value")
	}
	if metaString(m, "num") != "42" {
		t.Error("metaString failed for int value")
	}
	if metaString(m, "missing") != "" {
		t.Error("metaString should return empty for missing key")
	}
	if metaString(nil, "key") != "" {
		t.Error("metaString should return empty for nil map")
	}
	if metaFloat(m, "float") != 3.14 {
		t.Error("metaFloat failed for float64 value")
	}
	if metaFloat(m, "num") != 42.0 {
		t.Error("metaFloat failed for int value")
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub))
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
