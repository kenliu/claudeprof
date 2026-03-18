// Package analyzer computes performance metrics and optimization suggestions
// from a parsed Claude Code session.
package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"claudeprof/session"
)

// NotableEvent is a pre-computed signal flagged during analysis for use in
// deep AI analysis prompts.
type NotableEvent struct {
	TurnIdx int    // 0-based
	Kind    string // "permission-block" | "tool-error" | "repeated-tool" | "file-churn" | "context-spike" | "long-tool-chain"
	Detail  string
}

// TurnMetrics holds computed metrics for a single assistant turn.
type TurnMetrics struct {
	TurnIdx      int
	Timestamp    time.Time
	UserText     string
	AsstText     string
	ToolNames    []string
	ToolInputKeys []string              // Parallel to ToolNames — key parameter per call
	ToolResults   []session.ToolResult  // Results from this turn's tool calls
	Input        int // uncached input + cache creation
	Output       int
	CacheRead    int
	CacheCreate  int
	TotalTokens  int
	CacheHitPct  float64 // cache_read / (input + cache_read) * 100
	CumInput     int
	CumOutput    int
	CumCacheRead int
	CumTotal     int

	// Timing
	RequestTime   time.Time     // Absolute time the API request was sent
	APILatency    time.Duration // Time waiting for Claude API (RequestTime → Timestamp)
	GapBefore     time.Duration // Time since previous turn's response (tool exec or user idle)
	TurnCycle     time.Duration // GapBefore + APILatency
	IsToolTrigger bool          // True if gap was tool execution time, false if user idle
}

// Suggestion is an optimization recommendation.
type Suggestion struct {
	Level    string // "high" | "medium" | "low"
	Category string
	Title    string
	Detail   string
}

// LevelIcon returns a colored indicator for the suggestion level.
func (s Suggestion) LevelIcon() string {
	switch s.Level {
	case "high":
		return "●" // rendered red in TUI
	case "medium":
		return "●" // rendered yellow
	default:
		return "○" // rendered green
	}
}

// Analysis holds all computed metrics and suggestions for a session.
type Analysis struct {
	Session *session.Session

	// Aggregate totals
	TotalInput       int // uncached + cache creation
	TotalOutput      int
	TotalCacheRead   int
	TotalCacheCreate int
	TurnCount        int
	ToolCallCount    int

	// Derived
	OverallCacheHitPct float64 // cache_read / (cache_read + uncached_input) * 100
	AvgInputPerTurn    float64
	AvgOutputPerTurn   float64
	AvgToolsPerTurn    float64
	PeakInputTurn      int     // Index of turn with highest input tokens
	ContextGrowthRate  float64 // Ratio of last-quarter avg input vs first-quarter avg input

	// Timing
	TotalAPITime     time.Duration
	TotalToolTime    time.Duration // Sum of gaps where previous turn used tools
	TotalUserTime    time.Duration // Sum of gaps where user was idle between turns
	AvgAPILatency    time.Duration
	MaxAPILatency    time.Duration
	SlowestAPITurn   int // Index of turn with longest API latency
	MaxTurnCycle     time.Duration
	SlowestCycleTurn int // Index of turn with longest total cycle time

	// Per-turn detail
	Turns []TurnMetrics

	// Tool usage
	ToolCounts map[string]int
	ToolOrder  []string // Sorted by count, descending

	// Suggestions
	Suggestions []Suggestion

	// Notable events for deep AI analysis
	Notable []NotableEvent
}

// TotalTokens returns the total tokens consumed in the session.
func (a *Analysis) TotalTokens() int {
	return a.TotalInput + a.TotalCacheRead + a.TotalOutput
}

// Analyze computes all metrics and suggestions from a session.
func Analyze(sess *session.Session) *Analysis {
	a := &Analysis{
		Session:    sess,
		ToolCounts: make(map[string]int),
		TurnCount:  len(sess.Turns),
	}

	if len(sess.Turns) == 0 {
		return a
	}

	peakInput := 0
	var sumInput, sumOutput float64
	a.SlowestAPITurn = 0
	a.SlowestCycleTurn = 0

	for i, t := range sess.Turns {
		uncachedInput := t.Usage.InputTokens + t.Usage.CacheCreationInputTokens
		cacheRead := t.Usage.CacheReadInputTokens
		output := t.Usage.OutputTokens
		total := uncachedInput + cacheRead + output

		hitPct := 0.0
		denom := float64(uncachedInput + cacheRead)
		if denom > 0 {
			hitPct = float64(cacheRead) / denom * 100
		}

		// Timing: API latency = response time - request time.
		apiLatency := time.Duration(0)
		if !t.RequestTime.IsZero() && !t.Timestamp.IsZero() {
			if d := t.Timestamp.Sub(t.RequestTime); d > 0 {
				apiLatency = d
			}
		}
		// Timing: gap before = request time - previous turn's response time.
		gapBefore := time.Duration(0)
		if i > 0 && !t.RequestTime.IsZero() && !sess.Turns[i-1].Timestamp.IsZero() {
			if d := t.RequestTime.Sub(sess.Turns[i-1].Timestamp); d > 0 {
				gapBefore = d
			}
		}
		turnCycle := gapBefore + apiLatency

		tm := TurnMetrics{
			TurnIdx:       t.Index,
			Timestamp:     t.Timestamp,
			RequestTime:   t.RequestTime,
			UserText:      t.UserText,
			AsstText:      t.AsstText,
			Input:         uncachedInput,
			Output:        output,
			CacheRead:     cacheRead,
			CacheCreate:   t.Usage.CacheCreationInputTokens,
			TotalTokens:   total,
			CacheHitPct:   hitPct,
			CumInput:      t.CumUsage.InputTokens + t.CumUsage.CacheCreationInputTokens,
			CumOutput:     t.CumUsage.OutputTokens,
			CumCacheRead:  t.CumUsage.CacheReadInputTokens,
			CumTotal:      t.CumUsage.Total(),
			APILatency:    apiLatency,
			GapBefore:     gapBefore,
			TurnCycle:     turnCycle,
			IsToolTrigger: t.IsToolTrigger,
		}

		for _, tc := range t.ToolCalls {
			tm.ToolNames = append(tm.ToolNames, tc.Name)
			tm.ToolInputKeys = append(tm.ToolInputKeys, tc.InputKey)
			a.ToolCounts[tc.Name]++
			a.ToolCallCount++
		}
		tm.ToolResults = t.ToolResults

		a.Turns = append(a.Turns, tm)

		sumInput += float64(uncachedInput + cacheRead)
		sumOutput += float64(output)

		if uncachedInput+cacheRead > peakInput {
			peakInput = uncachedInput + cacheRead
			a.PeakInputTurn = t.Index
		}

		// Accumulate timing totals.
		a.TotalAPITime += apiLatency
		if i > 0 {
			if t.IsToolTrigger {
				a.TotalToolTime += gapBefore
			} else {
				a.TotalUserTime += gapBefore
			}
		}
		if apiLatency > a.MaxAPILatency {
			a.MaxAPILatency = apiLatency
			a.SlowestAPITurn = i
		}
		if turnCycle > a.MaxTurnCycle {
			a.MaxTurnCycle = turnCycle
			a.SlowestCycleTurn = i
		}
	}

	if a.TurnCount > 0 {
		a.AvgAPILatency = a.TotalAPITime / time.Duration(a.TurnCount)
	}

	// Totals from final cumulative usage.
	last := sess.Turns[len(sess.Turns)-1].CumUsage
	a.TotalInput = last.InputTokens + last.CacheCreationInputTokens
	a.TotalOutput = last.OutputTokens
	a.TotalCacheRead = last.CacheReadInputTokens
	a.TotalCacheCreate = last.CacheCreationInputTokens

	totalIn := float64(a.TotalInput + a.TotalCacheRead)
	if totalIn > 0 {
		a.OverallCacheHitPct = float64(a.TotalCacheRead) / totalIn * 100
	}

	if a.TurnCount > 0 {
		a.AvgInputPerTurn = sumInput / float64(a.TurnCount)
		a.AvgOutputPerTurn = sumOutput / float64(a.TurnCount)
		a.AvgToolsPerTurn = float64(a.ToolCallCount) / float64(a.TurnCount)
	}

	// Context growth: compare first-quarter avg input vs third-quarter avg input.
	if a.TurnCount >= 8 {
		qLen := a.TurnCount / 4
		var firstSum, lastSum float64
		for i := 0; i < qLen; i++ {
			firstSum += float64(a.Turns[i].Input + a.Turns[i].CacheRead)
		}
		for i := a.TurnCount - qLen; i < a.TurnCount; i++ {
			lastSum += float64(a.Turns[i].Input + a.Turns[i].CacheRead)
		}
		if firstSum > 0 {
			a.ContextGrowthRate = lastSum / firstSum
		}
	}

	// Sort tools by count.
	for name := range a.ToolCounts {
		a.ToolOrder = append(a.ToolOrder, name)
	}
	sort.Slice(a.ToolOrder, func(i, j int) bool {
		ci, cj := a.ToolCounts[a.ToolOrder[i]], a.ToolCounts[a.ToolOrder[j]]
		if ci != cj {
			return ci > cj
		}
		return a.ToolOrder[i] < a.ToolOrder[j]
	})

	a.Suggestions = computeSuggestions(a, sess)
	a.Notable = detectNotableEvents(a)
	return a
}

func computeSuggestions(a *Analysis, sess *session.Session) []Suggestion {
	var out []Suggestion

	// 1. Cache efficiency.
	switch {
	case a.OverallCacheHitPct < 20 && a.TurnCount > 5:
		out = append(out, Suggestion{
			Level:    "high",
			Category: "Cache",
			Title:    fmt.Sprintf("Very low cache hit rate (%.0f%%)", a.OverallCacheHitPct),
			Detail: "Less than 20% of input tokens are served from cache. Possible causes: " +
				"you're using /clear frequently (which destroys the cache), the session " +
				"is very long with heavy context mutation, or the model/region doesn't support " +
				"prompt caching. Keep the system prompt and early context stable.",
		})
	case a.OverallCacheHitPct < 50 && a.TurnCount > 10:
		out = append(out, Suggestion{
			Level:    "medium",
			Category: "Cache",
			Title:    fmt.Sprintf("Moderate cache utilization (%.0f%%)", a.OverallCacheHitPct),
			Detail: "Cache hit rate is below 50%. Avoid restructuring the early conversation " +
				"context between turns. Ensure you're not clearing context unnecessarily.",
		})
	}

	// 2. Context growth.
	if a.ContextGrowthRate > 4.0 {
		out = append(out, Suggestion{
			Level:    "high",
			Category: "Context",
			Title:    fmt.Sprintf("Rapid context growth (%.1f× from first to last quarter)", a.ContextGrowthRate),
			Detail: "Input token count has grown more than 4× across this session. This " +
				"inflates per-turn cost and increases latency. Use /compact to summarize, " +
				"or break the work into focused sub-sessions.",
		})
	} else if a.ContextGrowthRate > 2.5 {
		out = append(out, Suggestion{
			Level:    "medium",
			Category: "Context",
			Title:    fmt.Sprintf("Significant context growth (%.1f×)", a.ContextGrowthRate),
			Detail: "Context is growing steadily. Consider using /compact before it impacts " +
				"latency and cost, especially if the session will continue for many more turns.",
		})
	}

	// 3. Tool call density.
	if a.AvgToolsPerTurn > 6 && a.TurnCount > 5 {
		out = append(out, Suggestion{
			Level:    "medium",
			Category: "Tools",
			Title:    fmt.Sprintf("High tool call density (%.1f calls/turn avg)", a.AvgToolsPerTurn),
			Detail: "Many tool calls per turn increase latency and add tokens to the context. " +
				"Batch shell commands with && or write small scripts instead of issuing " +
				"many individual Bash calls. Combine file reads where possible.",
		})
	}

	// 4. Bash saturation.
	if bashCount, ok := a.ToolCounts["Bash"]; ok && a.ToolCallCount > 15 {
		ratio := float64(bashCount) / float64(a.ToolCallCount) * 100
		if ratio > 75 {
			out = append(out, Suggestion{
				Level:    "low",
				Category: "Tools",
				Title:    fmt.Sprintf("Bash-heavy session (%d calls, %.0f%% of all tool calls)", bashCount, ratio),
				Detail: "The session is dominated by Bash invocations. Consider providing " +
					"Claude Code with a helper script or Makefile target so multiple steps " +
					"can be expressed as a single command.",
			})
		}
	}

	// 5. No cache creation at all.
	if a.TotalCacheCreate == 0 && a.TurnCount > 3 {
		out = append(out, Suggestion{
			Level:    "medium",
			Category: "Cache",
			Title:    "No cache creation detected",
			Detail: "Zero cache_creation_input_tokens across the session. This may mean the " +
				"model configuration doesn't support prompt caching, or context structure " +
				"isn't triggering cache checkpoints. Verify you're using a cache-enabled " +
				"model (claude-opus-4-5 or claude-sonnet-4-5 with extended context).",
		})
	}

	// 6. Very high output ratio.
	if a.TotalTokens() > 0 {
		outRatio := float64(a.TotalOutput) / float64(a.TotalTokens()) * 100
		if outRatio > 30 && a.TurnCount > 5 {
			out = append(out, Suggestion{
				Level:    "low",
				Category: "Output",
				Title:    fmt.Sprintf("High output ratio (%.0f%% of all tokens)", outRatio),
				Detail: "A large fraction of tokens are output. If Claude is generating " +
					"verbose explanations or long code blocks you don't need, add output " +
					"constraints: \"be concise\", \"return only the diff\", or specify " +
					"output format precisely.",
			})
		}
	}

	// 7. Long session.
	if d := sess.Duration(); d > 3*time.Hour && a.TurnCount > 20 {
		out = append(out, Suggestion{
			Level:    "medium",
			Category: "Session",
			Title:    fmt.Sprintf("Very long session (%s)", FormatDuration(d)),
			Detail: "Sessions longer than 3 hours accumulate large context windows that " +
				"degrade response quality. Consider starting fresh sessions for distinct " +
				"sub-tasks, or using /compact periodically.",
		})
	}

	// 8. Compaction happened — context window is discontinuous.
	if sess.HasSummary {
		out = append(out, Suggestion{
			Level:    "low",
			Category: "Context",
			Title:    "Session contains a compaction summary",
			Detail: "This session was compacted at least once (/compact). Token counts before " +
				"and after compaction are not directly comparable, so aggregate metrics " +
				"may undercount the true context size at peak.",
		})
	}

	// Nothing notable? Positive signal.
	if len(out) == 0 {
		out = append(out, Suggestion{
			Level:    "low",
			Category: "General",
			Title:    "No major optimization opportunities found",
			Detail: "Cache utilization and token growth patterns look healthy for this session. " +
				"Keep an eye on context growth for longer sessions.",
		})
	}

	return out
}

func detectNotableEvents(a *Analysis) []NotableEvent {
	var events []NotableEvent

	permissionKeywords := []string{
		"permission", "not allowed", "denied", "EPERM",
		"Operation not permitted", "Permission denied",
	}

	// Track (toolName, inputKey) → last turn index seen, for repeat detection.
	type toolSig struct{ name, key string }
	lastSeen := make(map[toolSig]int)

	// Track Write/Edit targets → list of turn indices, for file churn detection.
	fileWrites := make(map[string][]int)

	// Track consecutive tool-triggered turns.
	consecutiveToolTurns := 0

	for i, tm := range a.Turns {
		// 1. Permission blocks and tool errors.
		for _, tr := range tm.ToolResults {
			if !tr.IsError {
				continue
			}
			isPermission := false
			for _, kw := range permissionKeywords {
				if strings.Contains(tr.Text, kw) {
					isPermission = true
					break
				}
			}
			if isPermission {
				events = append(events, NotableEvent{
					TurnIdx: i,
					Kind:    "permission-block",
					Detail:  Truncate(tr.Text, 120),
				})
			} else {
				events = append(events, NotableEvent{
					TurnIdx: i,
					Kind:    "tool-error",
					Detail:  Truncate(tr.Text, 120),
				})
			}
		}

		// 2. Repeated tool calls (same tool + same key within 5 turns).
		for j, name := range tm.ToolNames {
			key := ""
			if j < len(tm.ToolInputKeys) {
				key = tm.ToolInputKeys[j]
			}
			if key == "" {
				continue
			}
			sig := toolSig{name, key}
			if prevIdx, ok := lastSeen[sig]; ok && i-prevIdx <= 5 {
				events = append(events, NotableEvent{
					TurnIdx: i,
					Kind:    "repeated-tool",
					Detail:  fmt.Sprintf("%s called again with same input %q (prev turn %d)", name, Truncate(key, 60), prevIdx+1),
				})
			}
			lastSeen[sig] = i
		}

		// 3. File churn: record Write/Edit targets.
		for j, name := range tm.ToolNames {
			if name != "Write" && name != "Edit" {
				continue
			}
			key := ""
			if j < len(tm.ToolInputKeys) {
				key = tm.ToolInputKeys[j]
			}
			if key != "" {
				fileWrites[key] = append(fileWrites[key], i)
			}
		}

		// 4. Context spike: input tokens > 2× previous turn (both non-trivial).
		if i > 0 {
			prev := a.Turns[i-1]
			prevIn := prev.Input + prev.CacheRead
			curIn := tm.Input + tm.CacheRead
			if prevIn > 2000 && curIn > prevIn*2 {
				events = append(events, NotableEvent{
					TurnIdx: i,
					Kind:    "context-spike",
					Detail: fmt.Sprintf("input jumped %s → %s (%.1f×)",
						FormatTokens(prevIn), FormatTokens(curIn), float64(curIn)/float64(prevIn)),
				})
			}
		}

		// 5. Long tool chain: >5 tool calls in one turn.
		if len(tm.ToolNames) > 5 {
			events = append(events, NotableEvent{
				TurnIdx: i,
				Kind:    "long-tool-chain",
				Detail:  fmt.Sprintf("%d tool calls in a single turn", len(tm.ToolNames)),
			})
		}

		// 6. Many consecutive tool-triggered turns (no user interaction).
		if tm.IsToolTrigger {
			consecutiveToolTurns++
			if consecutiveToolTurns == 8 {
				events = append(events, NotableEvent{
					TurnIdx: i,
					Kind:    "long-tool-chain",
					Detail:  "8+ consecutive tool-triggered turns with no user interaction",
				})
			}
		} else {
			consecutiveToolTurns = 0
		}
	}

	// Post-scan: emit file churn events (3+ writes to same path).
	for path, indices := range fileWrites {
		if len(indices) < 3 {
			continue
		}
		strs := make([]string, len(indices))
		for k, idx := range indices {
			strs[k] = fmt.Sprintf("%d", idx+1)
		}
		events = append(events, NotableEvent{
			TurnIdx: indices[0],
			Kind:    "file-churn",
			Detail: fmt.Sprintf("%q written/edited %d times (turns %s)",
				Truncate(path, 50), len(indices), strings.Join(strs, ", ")),
		})
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].TurnIdx < events[j].TurnIdx
	})
	return events
}

// --- Formatting helpers ---

// FormatTokens formats a token count compactly.
func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// FormatDuration formats a duration human-readably.
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}

// Bar renders an ASCII percentage bar of the given width.
func Bar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(math.Round(pct / 100 * float64(width)))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// Truncate truncates a string to n runes, appending "…" if cut.
func Truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}
