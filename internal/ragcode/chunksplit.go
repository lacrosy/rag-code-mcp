package ragcode

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
)

const (
	// DefaultMaxChunkTokens is the maximum number of estimated tokens before splitting.
	// mxbai-embed-large context is 512 tokens; with conservative 2.5 chars/token estimate
	// we target 420 to leave headroom for tokenizer variance.
	DefaultMaxChunkTokens = 420

	// DefaultOverlapTokens is the number of tokens to overlap between adjacent parts.
	// Provides context continuity without excessive duplication.
	DefaultOverlapTokens = 50
)

// subChunk represents one part of a split CodeChunk.
type subChunk struct {
	// Text to embed (this part only)
	EmbedText string
	// Full CodeChunk JSON content for this part
	Chunk codetypes.CodeChunk
	// Metadata fields added for linking
	ParentID   string
	PartIndex  int
	TotalParts int
}

// estimateTokens gives a rough token count for text.
// WordPiece tokenizers (used by mxbai-embed-large) split code aggressively —
// identifiers, punctuation, and non-English text average ~2.5 chars/token.
// We use 2.5 to avoid exceeding the model's 512-token context window.
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return (len(text)*10 + 24) / 25 // equivalent to ceil(len/2.5)
}

// parentID generates a deterministic parent ID from the original chunk identity.
func parentID(ch codetypes.CodeChunk) string {
	h := fnv.New64a()
	h.Write([]byte(fmt.Sprintf("parent:%s:%d-%d:%s", ch.FilePath, ch.StartLine, ch.EndLine, ch.Name)))
	return fmt.Sprintf("p%d", h.Sum64())
}

// splitChunkIfNeeded checks if a chunk's embed text exceeds maxTokens.
// If it does, it splits into multiple subChunks with overlap.
// If it fits, it returns a single subChunk with no parent/part metadata.
func splitChunkIfNeeded(ch codetypes.CodeChunk, embedText string, maxTokens, overlapTokens int) []subChunk {
	tokens := estimateTokens(embedText)
	if tokens <= maxTokens {
		// Fits — return as-is, no split metadata
		return []subChunk{{
			EmbedText:  embedText,
			Chunk:      ch,
			TotalParts: 1,
		}}
	}

	// Split strategy: keep signature+docstring as prefix for every part,
	// then split the code body into overlapping windows.
	prefix := strings.TrimSpace(strings.Join(filterNonEmpty([]string{
		ch.Docstring,
		ch.Signature,
	}), "\n\n"))

	prefixTokens := estimateTokens(prefix)
	if prefixTokens >= maxTokens-50 {
		// Prefix alone is huge (unlikely but handle it) — just split raw text
		return splitRawText(ch, embedText, maxTokens, overlapTokens)
	}

	codeBody := strings.TrimSpace(ch.Code)
	if codeBody == "" {
		// No code body to split — return as single chunk
		return []subChunk{{
			EmbedText:  embedText,
			Chunk:      ch,
			TotalParts: 1,
		}}
	}

	// Available tokens for code body per part
	bodyBudget := maxTokens - prefixTokens - 10 // 10 token margin
	if bodyBudget < 100 {
		bodyBudget = 100
	}

	// Split code body into lines, then group into windows
	lines := strings.Split(codeBody, "\n")
	parts := splitLinesIntoWindows(lines, bodyBudget, overlapTokens)

	pid := parentID(ch)
	result := make([]subChunk, 0, len(parts))

	for i, partLines := range parts {
		partCode := strings.Join(partLines, "\n")

		// Build embed text: prefix + part code
		var embedParts []string
		if prefix != "" {
			embedParts = append(embedParts, prefix)
		}
		embedParts = append(embedParts, partCode)
		partEmbedText := strings.Join(embedParts, "\n\n")

		// Create a modified CodeChunk for this part
		partChunk := ch // copy
		partChunk.Code = partCode
		if partChunk.Metadata == nil {
			partChunk.Metadata = make(map[string]any)
		}
		partChunk.Metadata["parent_id"] = pid
		partChunk.Metadata["chunk_part"] = fmt.Sprintf("%d", i+1)
		partChunk.Metadata["chunk_total"] = fmt.Sprintf("%d", len(parts))

		result = append(result, subChunk{
			EmbedText:  partEmbedText,
			Chunk:      partChunk,
			ParentID:   pid,
			PartIndex:  i + 1,
			TotalParts: len(parts),
		})
	}

	return result
}

// splitLinesIntoWindows groups lines into windows that fit within tokenBudget,
// with overlapTokens worth of lines repeated between adjacent windows.
func splitLinesIntoWindows(lines []string, tokenBudget, overlapTokens int) [][]string {
	if len(lines) == 0 {
		return nil
	}

	var windows [][]string
	start := 0

	for start < len(lines) {
		// Greedily take lines until we exceed budget
		end := start
		currentTokens := 0
		for end < len(lines) {
			lineTokens := estimateTokens(lines[end]) + 1 // +1 for newline
			if currentTokens+lineTokens > tokenBudget && end > start {
				break
			}
			currentTokens += lineTokens
			end++
		}

		windows = append(windows, lines[start:end])

		if end >= len(lines) {
			break
		}

		// Calculate overlap: walk back from end to find overlap start
		overlapStart := end
		overlapAccum := 0
		for overlapStart > start {
			lineTokens := estimateTokens(lines[overlapStart-1]) + 1
			if overlapAccum+lineTokens > overlapTokens {
				break
			}
			overlapAccum += lineTokens
			overlapStart--
		}

		// Next window starts from overlap point
		if overlapStart >= end {
			// No overlap possible, just continue from end
			start = end
		} else {
			start = overlapStart
		}
	}

	return windows
}

// splitRawText is a fallback splitter when even the prefix is too large.
// It splits the entire embed text by lines.
func splitRawText(ch codetypes.CodeChunk, text string, maxTokens, overlapTokens int) []subChunk {
	lines := strings.Split(text, "\n")
	parts := splitLinesIntoWindows(lines, maxTokens, overlapTokens)

	pid := parentID(ch)
	result := make([]subChunk, 0, len(parts))

	for i, partLines := range parts {
		partText := strings.Join(partLines, "\n")

		partChunk := ch // copy
		partChunk.Code = partText
		if partChunk.Metadata == nil {
			partChunk.Metadata = make(map[string]any)
		}
		partChunk.Metadata["parent_id"] = pid
		partChunk.Metadata["chunk_part"] = fmt.Sprintf("%d", i+1)
		partChunk.Metadata["chunk_total"] = fmt.Sprintf("%d", len(parts))

		result = append(result, subChunk{
			EmbedText:  partText,
			Chunk:      partChunk,
			ParentID:   pid,
			PartIndex:  i + 1,
			TotalParts: len(parts),
		})
	}

	return result
}
