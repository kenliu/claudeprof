package tui

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/glamour"
	tea "github.com/charmbracelet/bubbletea/v2"

	"claudeprof/analyzer"
)

type aiAnalysisMsg struct {
	text string
	err  error
}

// renderMarkdown renders markdown text to ANSI-styled terminal output.
// Falls back to plain text on error.
func renderMarkdown(text string, width int) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithStylePath("dark"),
		glamour.WithWordWrap(width-4),
	)
	if err != nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return out
}

func runClaudeAnalysis(a *analyzer.Analysis) tea.Cmd {
	return func() tea.Msg {
		prompt := buildAnalysisPrompt(a)
		cmd := exec.Command("claude", "-p", prompt)
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return aiAnalysisMsg{err: fmt.Errorf("%w\n%s", err, strings.TrimSpace(errBuf.String()))}
		}
		return aiAnalysisMsg{text: out.String()}
	}
}

func buildAnalysisPrompt(a *analyzer.Analysis) string {
	s := a.Session
	var b strings.Builder

	b.WriteString("You are analyzing a Claude Code session. Here is the profiling data:\n\n")

	fmt.Fprintf(&b, "Session ID: %s\n", s.ID)
	fmt.Fprintf(&b, "Working directory: %s\n", s.CWD)
	fmt.Fprintf(&b, "Model: %s\n", s.Model)
	fmt.Fprintf(&b, "Duration: %s\n", analyzer.FormatDuration(s.Duration()))
	fmt.Fprintf(&b, "Total turns: %d\n", a.TurnCount)
	fmt.Fprintf(&b, "Total tool calls: %d\n", a.ToolCallCount)
	fmt.Fprintf(&b, "Compacted: %v\n", s.HasSummary)

	b.WriteString("\n## Token Usage\n")
	fmt.Fprintf(&b, "Total tokens: %s\n", analyzer.FormatTokens(a.TotalTokens()))
	fmt.Fprintf(&b, "Uncached input: %s\n", analyzer.FormatTokens(a.TotalInput))
	fmt.Fprintf(&b, "Cache reads: %s\n", analyzer.FormatTokens(a.TotalCacheRead))
	fmt.Fprintf(&b, "Cache creation: %s\n", analyzer.FormatTokens(a.TotalCacheCreate))
	fmt.Fprintf(&b, "Output: %s\n", analyzer.FormatTokens(a.TotalOutput))
	fmt.Fprintf(&b, "Overall cache hit rate: %.1f%%\n", a.OverallCacheHitPct)
	fmt.Fprintf(&b, "Average input per turn: %s\n", analyzer.FormatTokens(int(a.AvgInputPerTurn)))
	fmt.Fprintf(&b, "Average output per turn: %s\n", analyzer.FormatTokens(int(a.AvgOutputPerTurn)))
	if a.ContextGrowthRate > 0 {
		fmt.Fprintf(&b, "Context growth rate (Q1→Q4): %.1fx\n", a.ContextGrowthRate)
	}

	if len(a.ToolOrder) > 0 {
		b.WriteString("\n## Tool Usage\n")
		for _, name := range a.ToolOrder {
			fmt.Fprintf(&b, "  %s: %d calls\n", name, a.ToolCounts[name])
		}
	}

	limit := min(10, len(a.Turns))
	if limit > 0 {
		b.WriteString("\n## Turn-by-turn summary (first 10 turns)\n")
		for i := 0; i < limit; i++ {
			t := a.Turns[i]
			fmt.Fprintf(&b, "Turn %d: input=%s output=%s cache=%.0f%% tools=%s\n",
				t.TurnIdx+1,
				analyzer.FormatTokens(t.Input),
				analyzer.FormatTokens(t.Output),
				t.CacheHitPct,
				strings.Join(t.ToolNames, ","),
			)
			if t.UserText != "" {
				fmt.Fprintf(&b, "  Prompt: %s\n", analyzer.Truncate(t.UserText, 120))
			}
		}
	}

	b.WriteString("\n## Automated suggestions\n")
	for _, sg := range a.Suggestions {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", sg.Level, sg.Title, sg.Detail)
	}

	b.WriteString("\nPlease provide a concise analysis of this Claude Code session. Focus on:\n")
	b.WriteString("1. What kind of work was being done (based on prompts and tools used)\n")
	b.WriteString("2. Efficiency observations (token usage patterns, cache effectiveness)\n")
	b.WriteString("3. Any specific actionable recommendations beyond the automated suggestions\n")
	b.WriteString("\nKeep your response to 3-5 paragraphs.")

	return b.String()
}
