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

//go:embed deep_prompt.tmpl
var deepPromptTemplate string

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

// ---- Deep AI analysis ----

type selectedTurn struct {
	TurnNum    int
	StopReason string
	UserText   string
	ToolCalls  string   // pre-formatted: "Bash(cmd), Read(path)"
	ToolErrors []string // error result texts
	AsstText   string
}

func runDeepClaudeAnalysis(a *analyzer.Analysis) tea.Cmd {
	return func() tea.Msg {
		prompt := buildDeepAnalysisPrompt(a)
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

func buildDeepAnalysisPrompt(a *analyzer.Analysis) string {
	s := a.Session

	// Determine which turn indices are "interesting".
	interestingTurns := make(map[int]bool)
	for _, ev := range a.Notable {
		interestingTurns[ev.TurnIdx] = true
	}
	// Also include first, last, and any turn with a real user message.
	n := len(a.Turns)
	if n > 0 {
		interestingTurns[0] = true
		interestingTurns[n-1] = true
	}
	for i, tm := range a.Turns {
		if strings.TrimSpace(tm.UserText) != "" {
			interestingTurns[i] = true
		}
	}

	// Build selected turns list.
	var selected []selectedTurn
	for i, tm := range a.Turns {
		if !interestingTurns[i] {
			continue
		}

		// Format tool calls as "Name(key), Name(key)".
		var callParts []string
		for j, name := range tm.ToolNames {
			key := ""
			if j < len(tm.ToolInputKeys) {
				key = tm.ToolInputKeys[j]
			}
			if key != "" {
				callParts = append(callParts, fmt.Sprintf("%s(%s)", name, analyzer.Truncate(key, 50)))
			} else {
				callParts = append(callParts, name)
			}
		}

		// Collect error texts.
		var errs []string
		for _, tr := range tm.ToolResults {
			if tr.IsError {
				errs = append(errs, tr.Text)
			}
		}

		// Get the raw turn's stop reason.
		stopReason := ""
		if i < len(s.Turns) {
			stopReason = s.Turns[i].StopReason
		}

		selected = append(selected, selectedTurn{
			TurnNum:    i + 1,
			StopReason: stopReason,
			UserText:   analyzer.Truncate(strings.TrimSpace(tm.UserText), 500),
			ToolCalls:  strings.Join(callParts, ", "),
			ToolErrors: errs,
			AsstText:   analyzer.Truncate(strings.TrimSpace(tm.AsstText), 200),
		})
	}

	growthStr := ""
	if a.ContextGrowthRate > 0 {
		growthStr = fmt.Sprintf("%.1f", a.ContextGrowthRate)
	}

	data := map[string]any{
		"CWD":              s.CWD,
		"Model":            s.Model,
		"Duration":         analyzer.FormatDuration(s.Duration()),
		"TurnCount":        a.TurnCount,
		"TotalTokens":      analyzer.FormatTokens(a.TotalTokens()),
		"CacheHitPct":      fmt.Sprintf("%.1f", a.OverallCacheHitPct),
		"HasGrowth":        a.ContextGrowthRate > 0,
		"ContextGrowthStr": growthStr,
		"Notable":          a.Notable,
		"SelectedTurns":    selected,
	}

	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}
	tmpl := template.Must(template.New("deep").Funcs(funcMap).Parse(deepPromptTemplate))
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return deepPromptTemplate
	}
	return out.String()
}
