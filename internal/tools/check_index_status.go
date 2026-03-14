package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/doITmagic/rag-code-mcp/internal/workspace"
)

// CheckIndexStatusTool checks whether the code index is up-to-date.
// It does NOT trigger re-indexing — that is only done via `index-all` CLI.
type CheckIndexStatusTool struct {
	wm *workspace.Manager
}

func NewCheckIndexStatusTool(wm *workspace.Manager) *CheckIndexStatusTool {
	return &CheckIndexStatusTool{wm: wm}
}

func (t *CheckIndexStatusTool) Name() string {
	return "check_index_status"
}

func (t *CheckIndexStatusTool) Description() string {
	return "Check if the code index is up-to-date. Returns 'fresh' if no changes detected, " +
		"or 'stale' with details about what changed. " +
		"If stale, you MUST ask the user to run `task rag:index` before proceeding with code search. " +
		"This tool does NOT trigger re-indexing."
}

func (t *CheckIndexStatusTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	if t.wm == nil {
		return "", fmt.Errorf("workspace manager not available")
	}

	// DetectWorkspace uses workspace_root from config.yaml (no auto-detection needed)
	info, err := t.wm.DetectWorkspace(params)
	if err != nil || info == nil {
		return "", fmt.Errorf("cannot detect workspace: %v", err)
	}

	report, err := t.wm.CheckIndexFreshness(info)
	if err != nil {
		return "", fmt.Errorf("freshness check failed: %w", err)
	}

	if report.Fresh {
		out, _ := json.Marshal(map[string]interface{}{
			"status":        "fresh",
			"indexed_files": report.IndexedFiles,
			"last_indexed":  report.LastIndexed,
		})
		return string(out), nil
	}

	// Stale — build summary
	var reasons []string
	if len(report.Added) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d new files", len(report.Added)))
	}
	if len(report.Modified) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d modified files", len(report.Modified)))
	}
	if len(report.Deleted) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d deleted files", len(report.Deleted)))
	}

	// Show first few examples
	all := append(append(report.Added, report.Modified...), report.Deleted...)
	examples := all
	if len(examples) > 10 {
		examples = examples[:10]
	}

	out, _ := json.Marshal(map[string]interface{}{
		"status":   "stale",
		"reason":   strings.Join(reasons, " + "),
		"examples": examples,
		"action":   "Ask user to run: task rag:index",
	})
	return string(out), nil
}
