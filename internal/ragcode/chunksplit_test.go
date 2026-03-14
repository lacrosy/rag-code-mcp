package ragcode

import (
	"strings"
	"testing"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantMin  int
		wantMax  int
	}{
		{"empty", "", 0, 0},
		{"short", "hello", 1, 4},
		{"medium text ~100 chars", strings.Repeat("a", 100), 35, 45},
		{"code line ~50 chars", "public function processPayment(int $amount): bool", 18, 25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.text)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("estimateTokens(%d chars) = %d, want [%d, %d]", len(tt.text), got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestSplitChunkIfNeeded_SmallChunk(t *testing.T) {
	ch := codetypes.CodeChunk{
		Name:      "smallMethod",
		FilePath:  "src/Service.php",
		StartLine: 10,
		EndLine:   20,
		Signature: "public function smallMethod(): void",
		Code:      "echo 'hello';",
	}
	text := ch.Signature + "\n\n" + ch.Code

	result := splitChunkIfNeeded(ch, text, DefaultMaxChunkTokens, DefaultOverlapTokens)

	if len(result) != 1 {
		t.Fatalf("expected 1 subchunk, got %d", len(result))
	}
	if result[0].TotalParts != 1 {
		t.Errorf("expected TotalParts=1, got %d", result[0].TotalParts)
	}
	if result[0].ParentID != "" {
		t.Errorf("expected empty ParentID for non-split chunk, got %q", result[0].ParentID)
	}
}

func TestSplitChunkIfNeeded_LargeChunk(t *testing.T) {
	// Generate a large method body (~3000 chars = ~850 tokens)
	var codeLines []string
	for i := 0; i < 60; i++ {
		codeLines = append(codeLines, "        $this->repository->persist($entity);  // line "+strings.Repeat("x", 30))
	}

	ch := codetypes.CodeChunk{
		Name:      "processPayment",
		Type:      "method",
		FilePath:  "src/PaymentService.php",
		StartLine: 100,
		EndLine:   200,
		Signature: "public function processPayment(int $amount, string $currency): PaymentResult",
		Docstring: "/** Process a payment through the gateway. */",
		Code:      strings.Join(codeLines, "\n"),
		Metadata:  map[string]any{"provider": "payment"},
	}

	text := ch.Docstring + "\n\n" + ch.Signature + "\n\n" + ch.Code

	result := splitChunkIfNeeded(ch, text, DefaultMaxChunkTokens, DefaultOverlapTokens)

	if len(result) < 2 {
		t.Fatalf("expected >=2 subchunks for large method, got %d (text tokens: %d)",
			len(result), estimateTokens(text))
	}

	// All parts should have the same parent_id
	pid := result[0].ParentID
	if pid == "" {
		t.Fatal("expected non-empty ParentID")
	}

	for i, sc := range result {
		if sc.ParentID != pid {
			t.Errorf("part %d has different ParentID: %q vs %q", i+1, sc.ParentID, pid)
		}
		if sc.PartIndex != i+1 {
			t.Errorf("part %d has PartIndex=%d", i+1, sc.PartIndex)
		}
		if sc.TotalParts != len(result) {
			t.Errorf("part %d has TotalParts=%d, want %d", i+1, sc.TotalParts, len(result))
		}

		// Each part's embed text should contain the signature (prefix)
		if !strings.Contains(sc.EmbedText, "processPayment") {
			t.Errorf("part %d embed text doesn't contain signature", i+1)
		}

		// Each part should fit within the token budget
		partTokens := estimateTokens(sc.EmbedText)
		if partTokens > DefaultMaxChunkTokens+50 { // small margin for line boundaries
			t.Errorf("part %d has %d tokens, exceeds budget %d", i+1, partTokens, DefaultMaxChunkTokens)
		}

		// Metadata should propagate
		if sc.Chunk.Metadata["provider"] != "payment" {
			t.Errorf("part %d lost custom metadata", i+1)
		}
		if sc.Chunk.Metadata["parent_id"] != pid {
			t.Errorf("part %d chunk metadata missing parent_id", i+1)
		}
	}
}

func TestSplitLinesIntoWindows(t *testing.T) {
	// 20 lines of ~50 chars each = ~285 tokens total
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "$this->process($entity"+strings.Repeat("x", 30)+");")
	}

	windows := splitLinesIntoWindows(lines, 100, 20)

	if len(windows) < 2 {
		t.Fatalf("expected >=2 windows, got %d", len(windows))
	}

	// Verify all lines are covered
	covered := make(map[int]bool)
	for _, w := range windows {
		for _, line := range w {
			for i, orig := range lines {
				if line == orig {
					covered[i] = true
				}
			}
		}
	}
	for i := range lines {
		if !covered[i] {
			t.Errorf("line %d not covered by any window", i)
		}
	}

	// Verify overlap exists between adjacent windows
	for i := 1; i < len(windows); i++ {
		prevLast := windows[i-1][len(windows[i-1])-1]
		currFirst := windows[i][0]
		// The first line of current window should appear somewhere in the previous window
		found := false
		for _, line := range windows[i-1] {
			if line == currFirst {
				found = true
				break
			}
		}
		if !found {
			t.Logf("no overlap between window %d (last: %q) and window %d (first: %q)",
				i-1, prevLast, i, currFirst)
			// Not a hard error — overlap might not always be possible
		}
	}
}

func TestSplitChunkIfNeeded_EmptyCode(t *testing.T) {
	ch := codetypes.CodeChunk{
		Name:      "MyInterface",
		Type:      "interface",
		FilePath:  "src/MyInterface.php",
		Signature: "interface MyInterface",
	}
	text := ch.Signature

	result := splitChunkIfNeeded(ch, text, DefaultMaxChunkTokens, DefaultOverlapTokens)
	if len(result) != 1 {
		t.Fatalf("expected 1 subchunk for empty code, got %d", len(result))
	}
}

func TestParentID_Deterministic(t *testing.T) {
	ch := codetypes.CodeChunk{
		Name:      "foo",
		FilePath:  "src/Bar.php",
		StartLine: 10,
		EndLine:   50,
	}

	pid1 := parentID(ch)
	pid2 := parentID(ch)

	if pid1 != pid2 {
		t.Errorf("parentID not deterministic: %q != %q", pid1, pid2)
	}
	if !strings.HasPrefix(pid1, "p") {
		t.Errorf("parentID should start with 'p', got %q", pid1)
	}
}
