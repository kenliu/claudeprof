package tui

import (
	"bytes"
	_ "embed"
	"fmt"
	"os/exec"
	"strings"
	"text/template"

	"github.com/charmbracelet/glamour"
	tea "github.com/charmbracelet/bubbletea/v2"

	"claudeprof/analyzer"
)

//go:embed prompt.tmpl
var promptTemplate string

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

	// Build pre-rendered string blocks for multi-line sections.
	var toolLines strings.Builder
	for _, name := range a.ToolOrder {
		fmt.Fprintf(&toolLines, "  %s: %d calls\n", name, a.ToolCounts[name])
	}

	var turnLines strings.Builder
	for i := 0; i < min(10, len(a.Turns)); i++ {
		t := a.Turns[i]
		fmt.Fprintf(&turnLines, "Turn %d: input=%s output=%s cache=%.0f%% tools=%s\n",
			t.TurnIdx+1,
			analyzer.FormatTokens(t.Input),
			analyzer.FormatTokens(t.Output),
			t.CacheHitPct,
			strings.Join(t.ToolNames, ","),
		)
		if t.UserText != "" {
			fmt.Fprintf(&turnLines, "  Prompt: %s\n", analyzer.Truncate(t.UserText, 120))
		}
	}

	var suggLines strings.Builder
	for _, sg := range a.Suggestions {
		fmt.Fprintf(&suggLines, "- [%s] %s: %s\n", sg.Level, sg.Title, sg.Detail)
	}

	data := map[string]any{
		"Session":            s,
		"Duration":           analyzer.FormatDuration(s.Duration()),
		"TurnCount":          a.TurnCount,
		"ToolCallCount":      a.ToolCallCount,
		"TotalTokens":        analyzer.FormatTokens(a.TotalTokens()),
		"TotalInput":         analyzer.FormatTokens(a.TotalInput),
		"TotalCacheRead":     analyzer.FormatTokens(a.TotalCacheRead),
		"TotalCacheCreate":   analyzer.FormatTokens(a.TotalCacheCreate),
		"TotalOutput":        analyzer.FormatTokens(a.TotalOutput),
		"OverallCacheHitPct": fmt.Sprintf("%.1f", a.OverallCacheHitPct),
		"AvgInputPerTurn":    analyzer.FormatTokens(int(a.AvgInputPerTurn)),
		"AvgOutputPerTurn":   analyzer.FormatTokens(int(a.AvgOutputPerTurn)),
		"ContextGrowthRate":  a.ContextGrowthRate,
		"ToolLines":          toolLines.String(),
		"TurnLines":          turnLines.String(),
		"SuggestionLines":    suggLines.String(),
	}

	tmpl := template.Must(template.New("prompt").Parse(promptTemplate))
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		// Fallback: return raw template text so the caller still gets something.
		return promptTemplate
	}
	return out.String()
}
