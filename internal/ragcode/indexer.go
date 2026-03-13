package ragcode

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
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
}

func NewIndexer(analyzer codetypes.PathAnalyzer, embedder llm.Provider, ltm memory.LongTermMemory) *Indexer {
	return &Indexer{analyzer: analyzer, embedder: embedder, ltm: ltm}
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
	lastFile := ""

	for idx, ch := range chunks {
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

		emb, err := i.embedder.Embed(ctx, text)
		if err != nil {
			p.clear()
			return indexed, fmt.Errorf("embed failed for %s:%s: %w", ch.FilePath, ch.Name, err)
		}

		h := fnv.New64a()
		h.Write([]byte(fmt.Sprintf("%s:%d-%d:%s", ch.FilePath, ch.StartLine, ch.EndLine, ch.Name)))
		id := fmt.Sprintf("%d", h.Sum64())

		chunkJSON, err := json.Marshal(ch)
		if err != nil {
			p.clear()
			return indexed, fmt.Errorf("marshal chunk failed for %s: %w", ch.Name, err)
		}

		docMeta := map[string]interface{}{
			"file":       ch.FilePath,
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
		for k, v := range ch.Metadata {
			docMeta[k] = v
		}

		doc := memory.Document{
			ID:        id,
			Content:   string(chunkJSON),
			Embedding: emb,
			Metadata:  docMeta,
		}

		if err := i.ltm.Store(ctx, doc); err != nil {
			p.clear()
			return indexed, fmt.Errorf("store failed for %s: %w", id, err)
		}
		indexed++

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
