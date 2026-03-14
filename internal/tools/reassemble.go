package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
)

// ChunkSiblingFetcher can fetch sibling chunks by parent_id.
// Implemented by QdrantLongTermMemory via ScrollByMetadata.
type ChunkSiblingFetcher interface {
	ScrollByMetadata(ctx context.Context, filters map[string]string, limit int) ([]memory.Document, error)
}

// reassembleChunkedDocs processes search results to merge split chunks back together.
// For each document that has parent_id metadata (indicating it's a part of a split chunk),
// it fetches all sibling parts and merges them into a single document.
//
// Documents without parent_id are returned as-is.
// The fetcher parameter is optional — if nil, split chunks are returned individually.
func reassembleChunkedDocs(ctx context.Context, docs []memory.Document, fetcher ChunkSiblingFetcher) []memory.Document {
	if fetcher == nil {
		return docs
	}

	// Track which parent_ids we've already reassembled to avoid duplicates
	reassembled := make(map[string]memory.Document)
	// Track best score per parent_id (from the original search hit)
	bestScore := make(map[string]float64)
	var result []memory.Document

	for _, doc := range docs {
		pid := metaString(doc.Metadata, "parent_id")
		if pid == "" {
			// Not a split chunk — keep as-is
			result = append(result, doc)
			continue
		}

		// Already reassembled this parent?
		if _, done := reassembled[pid]; done {
			continue
		}

		// Fetch all siblings
		siblings, err := fetcher.ScrollByMetadata(ctx, map[string]string{"parent_id": pid}, 20)
		if err != nil || len(siblings) == 0 {
			// Fallback: return the individual part
			result = append(result, doc)
			reassembled[pid] = doc
			continue
		}

		// Sort siblings by chunk_part
		sort.Slice(siblings, func(i, j int) bool {
			pi := metaInt(siblings[i].Metadata, "chunk_part")
			pj := metaInt(siblings[j].Metadata, "chunk_part")
			return pi < pj
		})

		// Merge: combine code from all parts, keep metadata from the first part
		merged := mergeSiblingDocs(siblings)

		// Preserve the search score from the original hit
		score := metaFloat(doc.Metadata, "score")
		if score > 0 {
			if merged.Metadata == nil {
				merged.Metadata = make(map[string]interface{})
			}
			merged.Metadata["score"] = score
		}

		result = append(result, merged)
		reassembled[pid] = merged
		bestScore[pid] = score
	}

	return result
}

// mergeSiblingDocs merges multiple part documents into a single document.
// It combines code from all parts (removing overlap) and keeps other fields from the first part.
func mergeSiblingDocs(siblings []memory.Document) memory.Document {
	if len(siblings) == 0 {
		return memory.Document{}
	}
	if len(siblings) == 1 {
		return siblings[0]
	}

	// Parse CodeChunks from all siblings
	var chunks []codetypes.CodeChunk
	for _, sib := range siblings {
		var ch codetypes.CodeChunk
		if err := json.Unmarshal([]byte(sib.Content), &ch); err == nil {
			chunks = append(chunks, ch)
		}
	}

	if len(chunks) == 0 {
		return siblings[0]
	}

	// Use the first chunk as base, merge code from subsequent parts
	base := chunks[0]
	var codeBuilder strings.Builder
	codeBuilder.WriteString(base.Code)

	for i := 1; i < len(chunks); i++ {
		partCode := chunks[i].Code
		if partCode == "" {
			continue
		}

		// Remove overlap: find common suffix of previous code / prefix of this code
		prevCode := chunks[i-1].Code
		overlapLen := findOverlap(prevCode, partCode)
		if overlapLen > 0 && overlapLen < len(partCode) {
			partCode = partCode[overlapLen:]
		}

		if partCode != "" {
			codeBuilder.WriteString("\n")
			codeBuilder.WriteString(partCode)
		}
	}

	base.Code = codeBuilder.String()

	// Remove split metadata from the merged chunk
	delete(base.Metadata, "parent_id")
	delete(base.Metadata, "chunk_part")
	delete(base.Metadata, "chunk_total")

	// Marshal merged chunk back to JSON
	mergedJSON, _ := json.Marshal(base)

	// Build merged document
	merged := siblings[0]
	merged.Content = string(mergedJSON)

	// Clean up metadata
	if merged.Metadata != nil {
		delete(merged.Metadata, "parent_id")
		delete(merged.Metadata, "chunk_part")
		delete(merged.Metadata, "chunk_total")
		merged.Metadata["reassembled"] = "true"
		merged.Metadata["reassembled_parts"] = fmt.Sprintf("%d", len(siblings))
	}

	return merged
}

// findOverlap finds the length of overlap between the suffix of `prev` and prefix of `next`.
// It looks for matching lines at the boundary.
func findOverlap(prev, next string) int {
	if prev == "" || next == "" {
		return 0
	}

	prevLines := strings.Split(prev, "\n")
	nextLines := strings.Split(next, "\n")

	// Try to find the longest suffix of prevLines that matches a prefix of nextLines
	maxOverlap := len(prevLines)
	if maxOverlap > len(nextLines) {
		maxOverlap = len(nextLines)
	}

	bestOverlap := 0
	for overlapLines := 1; overlapLines <= maxOverlap; overlapLines++ {
		match := true
		for j := 0; j < overlapLines; j++ {
			prevLine := strings.TrimRight(prevLines[len(prevLines)-overlapLines+j], " \t")
			nextLine := strings.TrimRight(nextLines[j], " \t")
			if prevLine != nextLine {
				match = false
				break
			}
		}
		if match {
			bestOverlap = overlapLines
		}
	}

	if bestOverlap == 0 {
		return 0
	}

	// Calculate byte length of the overlapping prefix in next
	overlapBytes := 0
	for i := 0; i < bestOverlap; i++ {
		overlapBytes += len(nextLines[i])
		if i < bestOverlap-1 {
			overlapBytes++ // newline
		}
	}
	// Include trailing newline if present
	if bestOverlap < len(nextLines) {
		overlapBytes++ // the newline after the last overlap line
	}

	return overlapBytes
}

// Helper functions for metadata extraction

func metaString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func metaInt(m map[string]interface{}, key string) int {
	s := metaString(m, key)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func metaFloat(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch f := v.(type) {
	case float64:
		return f
	case float32:
		return float64(f)
	case int:
		return float64(f)
	default:
		return 0
	}
}
