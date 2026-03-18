// Package session parses Claude Code JSONL session files.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RawEntry is a single line in a Claude Code JSONL session file.
type RawEntry struct {
	ParentUUID  string          `json:"parentUuid"`
	IsSidechain bool            `json:"isSidechain"`
	UserType    string          `json:"userType"`
	CWD         string          `json:"cwd"`
	SessionID   string          `json:"sessionId"`
	Version     string          `json:"version"`
	Type        string          `json:"type"` // "user" | "assistant" | "summary"
	Message     json.RawMessage `json:"message"`
	UUID        string          `json:"uuid"`
	Timestamp   time.Time       `json:"timestamp"`
	CostUSD     *float64        `json:"costUSD"`
}

// Usage holds token usage from an Anthropic API response.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// TotalInput returns the sum of all input-side token counts (uncached + cached).
func (u Usage) TotalInput() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Total returns the sum of all token counts.
func (u Usage) Total() int {
	return u.TotalInput() + u.OutputTokens
}

// Add accumulates another Usage into this one.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.CacheCreationInputTokens += other.CacheCreationInputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
	u.OutputTokens += other.OutputTokens
}

// ContentBlock is a block within a message content array.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content (string or block array)
}

// ToolResult captures the output of a tool call returned in a user message.
type ToolResult struct {
	ToolUseID string
	IsError   bool
	Text      string // First 300 chars of result text
}

// AssistantMsg is the API response object embedded in an assistant entry.
type AssistantMsg struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// UserMsg is the request object embedded in a user entry.
type UserMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ToolCall is a tool invocation extracted from an assistant message.
type ToolCall struct {
	ID       string
	Name     string
	Input    json.RawMessage
	InputKey string // Key parameter extracted from Input (command, file path, pattern, etc.)
}

// Turn represents a single assistant API call with its preceding context.
// Each Turn corresponds to one assistant message (which may include tool calls).
type Turn struct {
	Index      int
	Timestamp  time.Time
	UserText   string // Text of the most recent human (non-tool-result) message
	AsstText   string // Concatenated text blocks from the assistant response
	ToolCalls  []ToolCall
	Model      string
	StopReason string
	Usage      Usage // Token usage for this specific API call
	CumUsage   Usage // Cumulative usage through this turn

	// Timing fields
	RequestTime   time.Time // Timestamp of the user/tool-result message that triggered this call
	IsToolTrigger bool      // True when triggered by tool results, false when triggered by human

	// Tool result feedback (populated from subsequent user message)
	ToolResults []ToolResult
}

// Session is a fully parsed Claude Code session.
type Session struct {
	ID         string
	FilePath   string
	CWD        string
	Model      string
	HasSummary bool // True if the session was compacted at least once
	StartTime  time.Time
	EndTime    time.Time
	Turns      []Turn
}

// Duration returns the session wall-clock duration.
func (s *Session) Duration() time.Duration {
	if s.StartTime.IsZero() || s.EndTime.IsZero() {
		return 0
	}
	return s.EndTime.Sub(s.StartTime)
}

// TotalUsage returns the cumulative token usage for the entire session.
func (s *Session) TotalUsage() Usage {
	if len(s.Turns) == 0 {
		return Usage{}
	}
	return s.Turns[len(s.Turns)-1].CumUsage
}

// DiscoveredSession holds metadata about a session file found on disk,
// without fully parsing it.
type DiscoveredSession struct {
	FilePath    string
	SessionID   string
	ProjectPath string // Best-effort decoded from directory name
	ModTime     time.Time
	Size        int64
}

// DiscoverSessions finds all session JSONL files under the given Claude config dir.
func DiscoverSessions(claudeDir string) ([]DiscoveredSession, error) {
	projectsDir := filepath.Join(claudeDir, "projects")
	var sessions []DiscoveredSession

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, fmt.Errorf("reading projects dir %q: %w", projectsDir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(projectsDir, entry.Name())
		projectPath := decodeProjectDir(entry.Name())

		files, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
		if err != nil {
			continue
		}
		for _, f := range files {
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			sessionID := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			sessions = append(sessions, DiscoveredSession{
				FilePath:    f,
				SessionID:   sessionID,
				ProjectPath: projectPath,
				ModTime:     info.ModTime(),
				Size:        info.Size(),
			})
		}
	}

	// Most recent first.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})
	return sessions, nil
}

// decodeProjectDir converts a Claude Code encoded project directory name back
// to a filesystem path. Claude Code stores project paths by replacing slashes
// with dashes and prepending a dash for absolute paths.
func decodeProjectDir(name string) string {
	if strings.HasPrefix(name, "-") {
		return "/" + strings.ReplaceAll(name[1:], "-", "/")
	}
	return strings.ReplaceAll(name, "-", "/")
}

// ParseFile parses a Claude Code JSONL session file into a structured Session.
func ParseFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()

	var entries []RawEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 16*1024*1024), 16*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e RawEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // Skip malformed lines gracefully
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %q: %w", path, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no parseable entries in %q", path)
	}

	sess := &Session{
		ID:       entries[0].SessionID,
		FilePath: path,
		CWD:      entries[0].CWD,
	}

	sess.Turns = buildTurns(entries, sess)

	if len(sess.Turns) > 0 {
		sess.StartTime = sess.Turns[0].Timestamp
		sess.EndTime = sess.Turns[len(sess.Turns)-1].Timestamp
	}
	for _, t := range sess.Turns {
		if t.Model != "" {
			sess.Model = t.Model
			break
		}
	}

	return sess, nil
}

func buildTurns(entries []RawEntry, sess *Session) []Turn {
	var turns []Turn
	var cumUsage Usage
	lastUserText := ""
	lastUserTimestamp := time.Time{}
	lastIsToolResult := false
	idx := 0

	for _, e := range entries {
		// Skip sidechain entries (used for internal tool sub-agents).
		if e.IsSidechain {
			continue
		}

		switch e.Type {
		case "summary":
			sess.HasSummary = true

		case "user":
			var msg UserMsg
			if err := json.Unmarshal(e.Message, &msg); err != nil {
				continue
			}
			// Track request time for every user entry (human or tool result).
			lastUserTimestamp = e.Timestamp
			lastIsToolResult = isToolResult(msg)
			// Only update lastUserText for human turns, not tool result feedback.
			if text := extractUserText(msg); text != "" && !isToolResult(msg) {
				lastUserText = text
			}
			// Attach tool results to the preceding assistant turn.
			if lastIsToolResult && len(turns) > 0 {
				results := extractToolResults(msg)
				turns[len(turns)-1].ToolResults = append(turns[len(turns)-1].ToolResults, results...)
			}

		case "assistant":
			var msg AssistantMsg
			if err := json.Unmarshal(e.Message, &msg); err != nil {
				continue
			}
			// Skip assistant entries without usage data (e.g. streaming partials).
			if msg.Usage.Total() == 0 {
				continue
			}

			cumUsage.Add(msg.Usage)

			t := Turn{
				Index:         idx,
				Timestamp:     e.Timestamp,
				UserText:      lastUserText,
				AsstText:      extractAsstText(msg),
				ToolCalls:     extractToolCalls(msg),
				Model:         msg.Model,
				StopReason:    msg.StopReason,
				Usage:         msg.Usage,
				CumUsage:      cumUsage,
				RequestTime:   lastUserTimestamp,
				IsToolTrigger: lastIsToolResult,
			}
			turns = append(turns, t)
			idx++
			lastUserText = "" // Consumed; reset until next human message
		}
	}
	return turns
}

func extractUserText(msg UserMsg) string {
	// Content can be a plain string or an array of content blocks.
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return s
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func isToolResult(msg UserMsg) bool {
	var blocks []ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

func extractAsstText(msg AssistantMsg) string {
	var parts []string
	for _, b := range msg.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

func extractToolCalls(msg AssistantMsg) []ToolCall {
	var calls []ToolCall
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			calls = append(calls, ToolCall{
				ID:       b.ID,
				Name:     b.Name,
				Input:    b.Input,
				InputKey: extractToolInputKey(b.Name, b.Input),
			})
		}
	}
	return calls
}

// extractToolInputKey pulls the most meaningful parameter from a tool's JSON
// input — the command string, file path, search pattern, etc.
func extractToolInputKey(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}

	var fields []string
	switch toolName {
	case "Bash":
		fields = []string{"command"}
	case "Read", "Write", "Edit", "NotebookEdit":
		fields = []string{"file_path", "path"}
	case "Glob":
		fields = []string{"pattern"}
	case "Grep":
		fields = []string{"pattern"}
	case "WebFetch":
		fields = []string{"url"}
	case "WebSearch":
		fields = []string{"query"}
	default:
		fields = []string{"file_path", "path", "command", "pattern", "query"}
	}

	for _, f := range fields {
		v, ok := m[f]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			runes := []rune(s)
			if len(runes) > 120 {
				s = string(runes[:119]) + "…"
			}
			return s
		}
	}
	return ""
}

// extractToolResults pulls tool_result content blocks from a user message.
func extractToolResults(msg UserMsg) []ToolResult {
	var blocks []ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	var results []ToolResult
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text := extractToolResultText(b)
		runes := []rune(text)
		if len(runes) > 300 {
			text = string(runes[:299]) + "…"
		}
		results = append(results, ToolResult{
			ToolUseID: b.ToolUseID,
			IsError:   b.IsError,
			Text:      text,
		})
	}
	return results
}

// extractToolResultText gets the text content from a tool_result block.
// The content field can be a plain string or an array of content blocks.
func extractToolResultText(b ContentBlock) string {
	if len(b.Content) > 0 {
		var s string
		if err := json.Unmarshal(b.Content, &s); err == nil {
			return s
		}
		var blocks []ContentBlock
		if err := json.Unmarshal(b.Content, &blocks); err == nil {
			var parts []string
			for _, sub := range blocks {
				if sub.Type == "text" && sub.Text != "" {
					parts = append(parts, sub.Text)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return b.Text
}
