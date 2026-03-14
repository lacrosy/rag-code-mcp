package ragcode

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/doITmagic/rag-code-mcp/internal/codetypes"
	"github.com/doITmagic/rag-code-mcp/internal/llm"
	"github.com/doITmagic/rag-code-mcp/internal/memory"
)

// OnFileIndexed is called after all chunks for a file have been successfully indexed.
// The caller can use this to update state and save progress incrementally.
type OnFileIndexed func(filePath string)

// Indexer indexes CodeChunks into LongTermMemory using an embedding Provider.
type Indexer struct {
	analyzer      codetypes.PathAnalyzer
	embedder      llm.Provider
	ltm           memory.LongTermMemory
	onFileIndexed OnFileIndexed
	// WorkspaceRoot is used to convert absolute file paths to relative paths
	// in metadata (state.json, Qdrant payloads). Empty = store absolute paths.
	WorkspaceRoot string
}

func NewIndexer(analyzer codetypes.PathAnalyzer, embedder llm.Provider, ltm memory.LongTermMemory) *Indexer {
	return &Indexer{analyzer: analyzer, embedder: embedder, ltm: ltm}
}

// relFilePath returns path relative to WorkspaceRoot, or original path if WorkspaceRoot is empty.
func (i *Indexer) relFilePath(absPath string) string {
	if i.WorkspaceRoot == "" {
		return absPath
	}
	rel, err := filepath.Rel(i.WorkspaceRoot, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

// SetOnFileIndexed sets a callback invoked after all chunks of a file are stored.
func (i *Indexer) SetOnFileIndexed(fn OnFileIndexed) {
	i.onFileIndexed = fn
}

// IndexPaths analyzes, embeds and stores all code chunks under the given paths.
// collection and dimension management should be handled by the caller (Qdrant client).
func (i *Indexer) IndexPaths(ctx context.Context, paths []string, sourceTag string) (int, error) {
	chunks, err := i.analyzer.AnalyzePaths(paths)
	if err != nil {
		return 0, err
	}

	total := len(chunks)
	if total == 0 {
		return 0, nil
	}

	// Count unique files for display
	fileSet := make(map[string]struct{})
	for _, ch := range chunks {
		fileSet[ch.FilePath] = struct{}{}
	}
	totalFiles := len(fileSet)

	fmt.Fprintf(os.Stderr, "  %d files → %d chunks\n", totalFiles, total)
	p := newProgress(total)
	indexed := 0
	skipped := 0
	lastFile := ""

	for idx, ch := range chunks {
		// Check if parent context was cancelled (e.g. Ctrl+C)
		if ctx.Err() != nil {
			p.clear()
			if skipped > 0 {
				log.Printf("⚠️  Skipped %d chunks due to embed errors before cancellation", skipped)
			}
			return indexed, fmt.Errorf("indexing cancelled: %w", ctx.Err())
		}

		text := strings.TrimSpace(strings.Join(filterNonEmpty([]string{
			ch.Docstring,
			ch.Signature,
			ch.Code,
		}), "\n\n"))
		if text == "" {
			// Notify file completed if switching to a new file
			if lastFile != "" && ch.FilePath != lastFile && i.onFileIndexed != nil {
				i.onFileIndexed(lastFile)
			}
			lastFile = ch.FilePath
			p.update(idx+1, ch.FilePath)
			continue
		}

		// Split large chunks that would exceed embedding model context window
		subChunks := splitChunkIfNeeded(ch, text, DefaultMaxChunkTokens, DefaultOverlapTokens)

		for partIdx, sc := range subChunks {
			// Per-chunk timeout: 2 minutes max per embed call (prevents one slow chunk from blocking)
			embedCtx, embedCancel := context.WithTimeout(ctx, 2*time.Minute)
			emb, err := i.embedder.Embed(embedCtx, sc.EmbedText)
			embedCancel()
			if err != nil {
				// Skip failed chunks instead of aborting the entire indexing run
				skipped++
				p.clearForLog("⚠️  Skip chunk %s:%s (part %d/%d): %v",
					ch.FilePath, ch.Name, partIdx+1, len(subChunks), err)
				continue
			}

			// Generate unique ID; for split chunks include part index
			h := fnv.New64a()
			if sc.TotalParts > 1 {
				h.Write([]byte(fmt.Sprintf("%s:%d-%d:%s:part%d",
					ch.FilePath, ch.StartLine, ch.EndLine, ch.Name, sc.PartIndex)))
			} else {
				h.Write([]byte(fmt.Sprintf("%s:%d-%d:%s",
					ch.FilePath, ch.StartLine, ch.EndLine, ch.Name)))
			}
			id := fmt.Sprintf("%d", h.Sum64())

			chunkJSON, err := json.Marshal(sc.Chunk)
			if err != nil {
				skipped++
				p.clearForLog("⚠️  Skip chunk %s (marshal error): %v", ch.Name, err)
				continue
			}

			relFile := i.relFilePath(ch.FilePath)
			docMeta := map[string]interface{}{
				"file":       relFile,
				"package":    ch.Package,
				"name":       ch.Name,
				"type":       ch.Type,
				"signature":  ch.Signature,
				"start_line": ch.StartLine,
				"end_line":   ch.EndLine,
				"source":     sourceTag,
				"basename":   filepath.Base(ch.FilePath),
			}
			// Merge extra metadata from analyzers (e.g. pspi_provider, symfony_type)
			for k, v := range sc.Chunk.Metadata {
				docMeta[k] = v
			}
			// Add split metadata to Qdrant payload for search reassembly
			if sc.TotalParts > 1 {
				docMeta["parent_id"] = sc.ParentID
				docMeta["chunk_part"] = fmt.Sprintf("%d", sc.PartIndex)
				docMeta["chunk_total"] = fmt.Sprintf("%d", sc.TotalParts)
			}

			doc := memory.Document{
				ID:        id,
				Content:   string(chunkJSON),
				Embedding: emb,
				Metadata:  docMeta,
			}

			if err := i.ltm.Store(ctx, doc); err != nil {
				skipped++
				p.clearForLog("⚠️  Skip chunk %s (store error): %v", id, err)
				continue
			}
			indexed++
		}

		// Notify when switching to a new file (all chunks of previous file are done)
		if lastFile != "" && ch.FilePath != lastFile && i.onFileIndexed != nil {
			i.onFileIndexed(lastFile)
		}
		lastFile = ch.FilePath
		p.update(idx+1, ch.FilePath)
	}

	// Notify for the last file
	if lastFile != "" && i.onFileIndexed != nil {
		i.onFileIndexed(lastFile)
	}

	p.clear()
	if skipped > 0 {
		log.Printf("⚠️  Indexing completed with %d skipped chunks (out of %d total)", skipped, total)
	}
	return indexed, nil
}

// progress renders an in-place 3-line display:
//
//	line 1: current file path (truncated)
//	line 2: progress bar  [████████░░░░░░░░░░░░]
//	line 3: stats — percentage, done/total, speed, ETA
type progress struct {
	total     int
	startTime time.Time
	printed   bool
}

func newProgress(total int) *progress {
	return &progress{total: total, startTime: time.Now()}
}

func (p *progress) update(done int, filePath string) {
	if p.total == 0 {
		return
	}

	// Overwrite previous 3 lines (move cursor up)
	if p.printed {
		fmt.Fprintf(os.Stderr, "\033[3A")
	}
	p.printed = true

	width := 80 // default terminal width
	pct := float64(done) / float64(p.total)
	elapsed := time.Since(p.startTime)

	// Speed & ETA
	speed := float64(0)
	eta := ""
	if elapsed > 0 {
		speed = float64(done) / elapsed.Seconds()
	}
	if speed > 0 && done < p.total {
		remaining := float64(p.total-done) / speed
		eta = formatDuration(time.Duration(remaining * float64(time.Second)))
	} else if done >= p.total {
		eta = "0s"
	}

	// Line 1: current file (truncated to width)
	display := truncatePath(filePath, width-2)
	// Clear line, print, pad to width
	fmt.Fprintf(os.Stderr, "\033[2K  %s\n", display)

	// Line 2: progress bar
	barWidth := width - 4 // [ ... ]  + 2 spaces
	if barWidth < 10 {
		barWidth = 10
	}
	filled := int(pct * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	fmt.Fprintf(os.Stderr, "\033[2K  [%s]\n", bar)

	// Line 3: stats
	stats := fmt.Sprintf("%.1f%% | %d/%d | %.1f/s | ETA %s",
		pct*100, done, p.total, speed, eta)
	fmt.Fprintf(os.Stderr, "\033[2K  %s\n", stats)
}

// clearForLog temporarily removes the progress bar, prints a log message, then marks
// the bar as needing redraw. Call update() after to restore it.
func (p *progress) clearForLog(format string, args ...interface{}) {
	if p.printed {
		// Move up 3 lines and clear each
		fmt.Fprintf(os.Stderr, "\033[3A\033[2K\033[1B\033[2K\033[1B\033[2K\033[3A")
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	p.printed = false // next update() won't try to overwrite old bar
}

// clear removes the progress display after completion
func (p *progress) clear() {
	if !p.printed {
		return
	}
	// Move up 3 lines and clear each
	fmt.Fprintf(os.Stderr, "\033[3A\033[2K\033[1B\033[2K\033[1B\033[2K\033[1A\033[1A")
}

func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	// Show .../<last components>
	parts := strings.Split(path, "/")
	result := ""
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := parts[i]
		if result != "" {
			candidate = parts[i] + "/" + result
		}
		if len(candidate)+4 > maxLen { // 4 for ".../"
			break
		}
		result = candidate
	}
	return ".../" + result
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m >= 60 {
		h := m / 60
		m = m % 60
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

func filterNonEmpty(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}
