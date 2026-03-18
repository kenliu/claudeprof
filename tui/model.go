// Package tui implements the Bubble Tea v2 interactive terminal UI for claudeprof.
package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"

	"claudeprof/analyzer"
	"claudeprof/report"
	"claudeprof/session"
)

// ---- Layout constants ----

const (
	headerLines  = 3
	tabBarLines  = 2
	footerLines  = 2
	minWidth     = 80
	barWidth     = 22 // width of ASCII percentage bars
)

// ---- App states and tabs ----

type appState int

const (
	statePicker  appState = iota // Session browser
	stateLoading                  // Parsing in progress
	stateProfile                  // Session profiler
)

type tab int

const (
	tabOverview tab = iota
	tabTimeline
	tabTiming
	tabTokens
	tabTools
	tabAIAnalysis
	tabCount
)

var tabLabels = []string{"Overview", "Timeline", "Timing", "Tokens", "Tools", "AI Analysis"}

// ---- Async messages ----

type sessionsLoadedMsg struct {
	sessions []session.DiscoveredSession
	err      error
}

type sessionParsedMsg struct {
	sess *session.Session
	err  error
}

type reportGeneratedMsg struct {
	path string
	err  error
}

type aiAnalysisLoadingMsg struct{}

// ---- Styles ----

var (
	clrPrimary  = lipgloss.Color("#7C86D8")
	clrAccent   = lipgloss.Color("#5C6AC4")
	clrText     = lipgloss.Color("#E6EDF3")
	clrMuted    = lipgloss.Color("#636E7B")
	clrGood     = lipgloss.Color("#3FB950")
	clrWarn     = lipgloss.Color("#D29922")
	clrError    = lipgloss.Color("#F85149")
	clrBorder   = lipgloss.Color("#30363D")
	clrSelected = lipgloss.Color("#1C2733")
	clrHeader   = lipgloss.Color("#0D1117")

	styleBold = lipgloss.NewStyle().Bold(true)

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(clrPrimary)

	styleMuted = lipgloss.NewStyle().
			Foreground(clrMuted)

	styleGood = lipgloss.NewStyle().
			Foreground(clrGood)

	styleWarn = lipgloss.NewStyle().
			Foreground(clrWarn)

	styleError = lipgloss.NewStyle().
			Foreground(clrError)

	styleSelected = lipgloss.NewStyle().
			Background(clrSelected).
			Foreground(clrText)

	styleHeader = lipgloss.NewStyle().
			Background(clrHeader).
			Foreground(clrText).
			Padding(0, 1)

	styleDivider = lipgloss.NewStyle().
			Foreground(clrBorder)

	styleActiveTab = lipgloss.NewStyle().
			Bold(true).
			Foreground(clrPrimary).
			Underline(true)

	styleInactiveTab = lipgloss.NewStyle().
				Foreground(clrMuted)
)

// ---- Model ----

// Model is the root Bubble Tea model.
type Model struct {
	state  appState
	width  int
	height int
	err    error

	claudeDir string

	// Picker state
	discovered  []session.DiscoveredSession
	pickerPos   int
	pickerScroll int

	// Profile state
	sess         *session.Session
	analysis     *analyzer.Analysis
	activeTab    tab
	scrollOffset int

	// AI analysis
	aiRaw     string // raw markdown from claude
	aiText    string // rendered for display
	aiErr     error
	aiLoading bool

	// Transient
	statusMsg   string
	sessionFile string // non-empty if opened directly via CLI arg

	// Timing tab sub-view: false = per-turn table, true = flame/timeline chart
	timingFlame bool
}

// New creates a Model ready to launch. If sessionFile is non-empty the given
// session is opened directly instead of showing the picker.
func New(claudeDir, sessionFile string) (Model, error) {
	m := Model{
		claudeDir:   claudeDir,
		sessionFile: sessionFile,
		state:       statePicker,
	}
	if sessionFile != "" {
		m.state = stateLoading
		m.statusMsg = "Parsing session…"
	}
	return m, nil
}

// ---- Bubble Tea interface (v2) ----

func (m Model) Init() (tea.Model, tea.Cmd) {
	return m, m.bootCmd()
}

func (m Model) bootCmd() tea.Cmd {
	if m.state == stateLoading && m.sessionFile != "" {
		path := m.sessionFile
		return func() tea.Msg {
			sess, err := session.ParseFile(path)
			return sessionParsedMsg{sess: sess, err: err}
		}
	}
	return func() tea.Msg {
		sessions, err := session.DiscoverSessions(m.claudeDir)
		return sessionsLoadedMsg{sessions: sessions, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = max(msg.Width, minWidth)
		m.height = msg.Height
		if m.aiRaw != "" {
			m.aiText = renderMarkdown(m.aiRaw, m.width)
		}
		return m, nil

	case sessionsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.discovered = msg.sessions
		m.state = statePicker
		return m, nil

	case sessionParsedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = statePicker
			return m, nil
		}
		m.sess = msg.sess
		m.analysis = analyzer.Analyze(msg.sess)
		m.state = stateProfile
		m.activeTab = tabOverview
		m.scrollOffset = 0
		return m, nil

	case reportGeneratedMsg:
		if msg.err != nil {
			m.statusMsg = "report error: " + msg.err.Error()
		} else {
			m.statusMsg = "report opened: " + msg.path
		}
		return m, nil

	case aiAnalysisMsg:
		m.aiLoading = false
		m.aiErr = msg.err
		if msg.text != "" {
			m.aiRaw = msg.text
			m.aiText = renderMarkdown(msg.text, m.width)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global.
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	}

	switch m.state {
	case statePicker:
		return m.pickerKey(key)
	case stateLoading:
		// Nothing interactive during load.
		return m, nil
	case stateProfile:
		return m.profileKey(key)
	}
	return m, nil
}

func (m Model) pickerKey(key string) (tea.Model, tea.Cmd) {
	n := len(m.discovered)
	switch key {
	case "q":
		return m, tea.Quit
	case "up", "k":
		if m.pickerPos > 0 {
			m.pickerPos--
			m.clampPickerScroll()
		}
	case "down", "j":
		if m.pickerPos < n-1 {
			m.pickerPos++
			m.clampPickerScroll()
		}
	case "enter", " ":
		if n > 0 {
			chosen := m.discovered[m.pickerPos]
			m.state = stateLoading
			m.statusMsg = "Parsing…"
			return m, parseSessionCmd(chosen.FilePath)
		}
	}
	return m, nil
}

func (m *Model) clampPickerScroll() {
	visibleRows := m.height - headerLines - footerLines - 5
	if visibleRows < 1 {
		visibleRows = 1
	}
	if m.pickerPos < m.pickerScroll {
		m.pickerScroll = m.pickerPos
	}
	if m.pickerPos >= m.pickerScroll+visibleRows {
		m.pickerScroll = m.pickerPos - visibleRows + 1
	}
}

func (m Model) profileKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "q", "esc", "backspace":
		m.state = statePicker
		m.sess = nil
		m.analysis = nil
		m.scrollOffset = 0
		m.timingFlame = false
		m.aiRaw = ""
		m.aiText = ""
		m.aiErr = nil
		m.aiLoading = false
	case "f":
		if m.activeTab == tabTiming {
			m.timingFlame = !m.timingFlame
			m.scrollOffset = 0
		}
	case "tab":
		m.activeTab = (m.activeTab + 1) % tabCount
		m.scrollOffset = 0
		m.timingFlame = false
		m.statusMsg = ""
	case "shift+tab":
		m.activeTab = (m.activeTab - 1 + tabCount) % tabCount
		m.scrollOffset = 0
		m.timingFlame = false
		m.statusMsg = ""
	case "1":
		m.activeTab = tabOverview
		m.scrollOffset = 0
	case "2":
		m.activeTab = tabTimeline
		m.scrollOffset = 0
	case "3":
		m.activeTab = tabTiming
		m.scrollOffset = 0
	case "4":
		m.activeTab = tabTokens
		m.scrollOffset = 0
	case "5":
		m.activeTab = tabTools
		m.scrollOffset = 0
	case "6":
		m.activeTab = tabAIAnalysis
		m.scrollOffset = 0
	case "a":
		if m.analysis != nil && !m.aiLoading {
			m.aiLoading = true
			m.aiRaw = ""
			m.aiText = ""
			m.aiErr = nil
			m.activeTab = tabAIAnalysis
			m.scrollOffset = 0
			return m, runClaudeAnalysis(m.analysis)
		}
	case "up", "k":
		if m.scrollOffset > 0 {
			m.scrollOffset--
		}
	case "down", "j":
		m.scrollOffset++
	case "g":
		m.scrollOffset = 0
	case "G":
		m.scrollOffset = 9999
	case "r":
		if m.analysis != nil {
			m.statusMsg = "generating report…"
			a := m.analysis
			return m, func() tea.Msg {
				path, err := report.Generate(a, "")
				return reportGeneratedMsg{path: path, err: err}
			}
		}
	}
	return m, nil
}

func parseSessionCmd(path string) tea.Cmd {
	return func() tea.Msg {
		sess, err := session.ParseFile(path)
		return sessionParsedMsg{sess: sess, err: err}
	}
}

// ---- View ----

func (m Model) View() string {
	if m.err != nil {
		return styleError.Render("error: "+m.err.Error()) + "\n\nPress ctrl+c to quit.\n"
	}

	var sb strings.Builder

	switch m.state {
	case statePicker:
		sb.WriteString(m.viewPicker())
	case stateLoading:
		sb.WriteString(m.viewLoading())
	case stateProfile:
		sb.WriteString(m.viewProfile())
	}

	return sb.String()
}

// ---- Picker view ----

func (m Model) viewPicker() string {
	w := m.width
	var b strings.Builder

	b.WriteString(m.renderHeader("claudeprof  ·  claude code session profiler", ""))
	b.WriteString("\n")

	if len(m.discovered) == 0 {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  No sessions found in %s\n", m.claudeDir)))
		b.WriteString(m.renderFooter("↑↓/jk navigate  enter select  q quit"))
		return b.String()
	}

	// Column header
	projW := max(w-52, 20)
	header := styleMuted.Render(
		fmt.Sprintf("  %-*s  %-16s  %6s  %8s", projW, "PROJECT", "DATE", "SIZE", ""),
	)
	b.WriteString(header + "\n")
	b.WriteString(styleDivider.Render("  "+strings.Repeat("─", w-4)) + "\n")

	visibleRows := m.height - headerLines - footerLines - 4
	if visibleRows < 1 {
		visibleRows = 1
	}

	end := m.pickerScroll + visibleRows
	if end > len(m.discovered) {
		end = len(m.discovered)
	}

	for i := m.pickerScroll; i < end; i++ {
		s := m.discovered[i]
		proj := truncate(s.ProjectPath, projW)
		date := formatRelTime(s.ModTime)
		size := formatSize(s.Size)

		line := fmt.Sprintf("  %-*s  %-16s  %6s", projW, proj, date, size)

		if i == m.pickerPos {
			b.WriteString(styleSelected.Render("▶ " + line[2:]) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}

	if len(m.discovered) > visibleRows {
		shown := fmt.Sprintf("%d–%d of %d", m.pickerScroll+1, end, len(m.discovered))
		b.WriteString(styleMuted.Render("  " + shown) + "\n")
	}

	b.WriteString(m.renderFooter("↑↓/jk navigate  enter select  q quit"))
	return b.String()
}

// ---- Loading view ----

func (m Model) viewLoading() string {
	var b strings.Builder
	b.WriteString(m.renderHeader("claudeprof", ""))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render("  ◌  "+m.statusMsg) + "\n")
	return b.String()
}

// ---- Profile view ----

func (m Model) viewProfile() string {
	if m.sess == nil || m.analysis == nil {
		return ""
	}

	var b strings.Builder
	project := filepath.Base(m.sess.CWD)
	subtitle := fmt.Sprintf("%s  ·  %s  ·  %s",
		project,
		m.sess.StartTime.Format("Jan 2 15:04"),
		analyzer.FormatDuration(m.sess.Duration()),
	)
	b.WriteString(m.renderHeader("claudeprof", subtitle))
	b.WriteString(m.renderTabBar())

	contentH := m.height - headerLines - tabBarLines - footerLines
	if contentH < 4 {
		contentH = 4
	}

	var content string
	switch m.activeTab {
	case tabOverview:
		content = m.viewOverview()
	case tabTimeline:
		content = m.viewTimeline(contentH)
	case tabTiming:
		if m.timingFlame {
			content = m.viewTimingFlame(contentH)
		} else {
			content = m.viewTiming(contentH)
		}
	case tabTokens:
		content = m.viewTokens(contentH)
	case tabTools:
		content = m.viewTools(contentH)
	case tabAIAnalysis:
		content = m.viewAIAnalysis(contentH)
	}

	b.WriteString(content)

	footer := "tab/shift-tab switch  1-6 jump  a ai  ↑↓/jk scroll  r report  esc back  q quit"
	if m.activeTab == tabTiming {
		flameHint := "f flame"
		if m.timingFlame {
			flameHint = "f table"
		}
		footer = flameHint + "  tab/1-7 switch  ↑↓/jk scroll  r report  esc back  q quit"
	}
	if m.statusMsg != "" {
		footer = m.statusMsg + "   " + styleMuted.Render("(tab/shift-tab  a ai  r report  esc back  q quit)")
	}
	b.WriteString(m.renderFooter(footer))
	return b.String()
}

// ---- Overview tab ----

func (m Model) viewOverview() string {
	a := m.analysis
	s := m.sess
	w := m.width - 4
	var b strings.Builder

	b.WriteString("\n")

	// Stats grid
	col1 := [][]string{
		{"Duration", analyzer.FormatDuration(s.Duration())},
		{"Turns", fmt.Sprintf("%d", a.TurnCount)},
		{"Model", s.Model},
	}
	col2 := [][]string{
		{"Tool calls", fmt.Sprintf("%d (%d types)", a.ToolCallCount, len(a.ToolCounts))},
		{"Avg tools/turn", fmt.Sprintf("%.1f", a.AvgToolsPerTurn)},
		{"Compacted", boolStr(s.HasSummary)},
	}
	b.WriteString(renderStatGrid(col1, col2, w))
	b.WriteString("\n")

	// Token breakdown
	total := a.TotalTokens()
	b.WriteString("  " + styleBold.Render("Token Breakdown") + "\n\n")

	type tokenRow struct {
		label string
		val   int
		color lipgloss.Color
	}
	rows := []tokenRow{
		{"Uncached input ", a.TotalInput, clrText},
		{"Cache read     ", a.TotalCacheRead, clrGood},
		{"Cache creation ", a.TotalCacheCreate, clrAccent},
		{"Output         ", a.TotalOutput, clrWarn},
	}
	for _, r := range rows {
		pct := 0.0
		if total > 0 {
			pct = float64(r.val) / float64(total) * 100
		}
		bar := lipgloss.NewStyle().Foreground(r.color).Render(analyzer.Bar(pct, barWidth))
		b.WriteString(fmt.Sprintf("  %s  %s  %6.1f%%  %s\n",
			styleMuted.Render(r.label),
			bar,
			pct,
			lipgloss.NewStyle().Foreground(r.color).Render(analyzer.FormatTokens(r.val)),
		))
	}

	b.WriteString("\n")

	// Cache efficiency indicator
	cachePct := a.OverallCacheHitPct
	cacheColor := clrGood
	cacheLabel := "excellent"
	switch {
	case cachePct < 30:
		cacheColor = clrError
		cacheLabel = "poor"
	case cachePct < 60:
		cacheColor = clrWarn
		cacheLabel = "moderate"
	}
	cacheStyle := lipgloss.NewStyle().Foreground(cacheColor)
	b.WriteString(fmt.Sprintf("  Cache efficiency  %s  %s\n",
		cacheStyle.Render(fmt.Sprintf("%.1f%%", cachePct)),
		styleMuted.Render("▸ "+cacheLabel),
	))

	// Context growth
	if a.ContextGrowthRate > 0 {
		growthColor := clrGood
		growthLabel := "stable"
		switch {
		case a.ContextGrowthRate > 4:
			growthColor = clrError
			growthLabel = "rapid"
		case a.ContextGrowthRate > 2:
			growthColor = clrWarn
			growthLabel = "notable"
		}
		gs := lipgloss.NewStyle().Foreground(growthColor)
		b.WriteString(fmt.Sprintf("  Context growth    %s  %s\n",
			gs.Render(fmt.Sprintf("%.1f×", a.ContextGrowthRate)),
			styleMuted.Render("▸ "+growthLabel),
		))
	}

	// Top tools preview
	if len(a.ToolOrder) > 0 {
		b.WriteString("\n")
		b.WriteString("  " + styleBold.Render("Top Tools") + "\n\n")
		max3 := min(3, len(a.ToolOrder))
		for i := 0; i < max3; i++ {
			name := a.ToolOrder[i]
			count := a.ToolCounts[name]
			b.WriteString(fmt.Sprintf("  %-20s  %d\n",
				styleMuted.Render(name), count))
		}
	}

	// Suggestions inline
	b.WriteString("\n")
	b.WriteString("  " + styleBold.Render("Suggestions") + "\n\n")
	if len(a.Suggestions) == 0 {
		b.WriteString(fmt.Sprintf("  %s  Session looks efficient\n", styleGood.Render("●")))
	} else {
		for _, sg := range a.Suggestions {
			iconStyle := styleGood
			switch sg.Level {
			case "high":
				iconStyle = styleError
			case "medium":
				iconStyle = styleWarn
			}
			b.WriteString(fmt.Sprintf("  %s  %s  %s\n",
				iconStyle.Render("●"),
				styleBold.Render(sg.Title),
				styleMuted.Render("["+sg.Category+"]"),
			))
			wrapped := wordWrap(sg.Detail, m.width-8)
			for _, line := range wrapped {
				b.WriteString(styleMuted.Render("     "+line) + "\n")
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// ---- Timeline tab ----

func (m Model) viewTimeline(contentH int) string {
	a := m.analysis
	var b strings.Builder

	b.WriteString("\n")
	w := m.width - 4

	// Header row
	b.WriteString(styleMuted.Render(fmt.Sprintf(
		"  %4s  %-7s  %7s  %7s  %6s  %s\n",
		"#", "TIME", "INPUT", "OUTPUT", "CACHE%", "TOOLS / PROMPT",
	)))
	b.WriteString(styleDivider.Render("  "+strings.Repeat("─", w-2)) + "\n")

	rowH := 2
	pageH := contentH - 3
	maxRows := pageH / rowH
	if maxRows < 1 {
		maxRows = 1
	}

	start := m.scrollOffset
	if start >= len(a.Turns) {
		start = max(0, len(a.Turns)-1)
	}
	end := start + maxRows
	if end > len(a.Turns) {
		end = len(a.Turns)
	}

	for i := start; i < end; i++ {
		t := a.Turns[i]
		timeStr := ""
		if !t.Timestamp.IsZero() {
			timeStr = t.Timestamp.Format("15:04:05")
		}

		toolStr := strings.Join(t.ToolNames, " ")
		if len(toolStr) == 0 {
			toolStr = styleMuted.Render("(text only)")
		}

		cacheColor := clrGood
		if t.CacheHitPct < 30 {
			cacheColor = clrError
		} else if t.CacheHitPct < 60 {
			cacheColor = clrWarn
		}

		line1 := fmt.Sprintf("  %4d  %-7s  %7s  %7s  %s  %s",
			t.TurnIdx+1,
			styleMuted.Render(timeStr),
			analyzer.FormatTokens(t.Input),
			analyzer.FormatTokens(t.Output),
			lipgloss.NewStyle().Foreground(cacheColor).Render(fmt.Sprintf("%5.0f%%", t.CacheHitPct)),
			truncate(toolStr, w-48),
		)
		b.WriteString(line1 + "\n")

		prompt := t.UserText
		if prompt == "" {
			prompt = t.AsstText
		}
		b.WriteString(styleMuted.Render("       ↳ "+truncate(prompt, w-10)) + "\n")
	}

	if len(a.Turns) > maxRows {
		b.WriteString(styleMuted.Render(fmt.Sprintf(
			"\n  %d–%d of %d turns  (↑↓ to scroll)",
			start+1, end, len(a.Turns),
		)) + "\n")
	}

	return b.String()
}

// ---- Timing tab ----

func (m Model) viewTiming(contentH int) string {
	a := m.analysis
	var b strings.Builder

	b.WriteString("\n")

	// Summary block: time breakdown.
	totalWall := a.TotalAPITime + a.TotalToolTime + a.TotalUserTime
	b.WriteString("  " + styleBold.Render("Time Breakdown") + "\n\n")

	type timingRow struct {
		label string
		dur   time.Duration
		color lipgloss.Color
	}
	trows := []timingRow{
		{"API wait  ", a.TotalAPITime, clrPrimary},
		{"Tool exec ", a.TotalToolTime, clrWarn},
		{"User idle ", a.TotalUserTime, clrMuted},
	}
	for _, r := range trows {
		pct := 0.0
		if totalWall > 0 {
			pct = float64(r.dur) / float64(totalWall) * 100
		}
		bar := lipgloss.NewStyle().Foreground(r.color).Render(analyzer.Bar(pct, barWidth))
		b.WriteString(fmt.Sprintf("  %s  %s  %6.1f%%  %s\n",
			styleMuted.Render(r.label),
			bar,
			pct,
			lipgloss.NewStyle().Foreground(r.color).Render(analyzer.FormatDuration(r.dur)),
		))
	}

	b.WriteString("\n")
	if a.TurnCount > 0 {
		b.WriteString(fmt.Sprintf("  %-22s  %s\n", "Avg API latency", analyzer.FormatDuration(a.AvgAPILatency)))
	}
	if a.MaxAPILatency > 0 && a.SlowestAPITurn < len(a.Turns) {
		b.WriteString(fmt.Sprintf("  %-22s  turn %d (%s)\n",
			"Slowest API turn",
			a.Turns[a.SlowestAPITurn].TurnIdx+1,
			analyzer.FormatDuration(a.MaxAPILatency),
		))
	}
	if a.MaxTurnCycle > 0 && a.SlowestCycleTurn < len(a.Turns) {
		b.WriteString(fmt.Sprintf("  %-22s  turn %d (%s)\n",
			"Slowest cycle",
			a.Turns[a.SlowestCycleTurn].TurnIdx+1,
			analyzer.FormatDuration(a.MaxTurnCycle),
		))
	}

	// Check if timing data is meaningful.
	if a.TotalAPITime == 0 {
		b.WriteString("\n")
		b.WriteString(styleMuted.Render("  No timing data available (session timestamps may be missing).") + "\n")
		return b.String()
	}

	b.WriteString("\n")

	// Per-turn timing table with two bar columns.
	w := m.width - 4
	gapBarW := 18
	apiBarW := 18
	if w > 90 {
		gapBarW = 22
		apiBarW = 22
	}

	b.WriteString(styleMuted.Render(fmt.Sprintf(
		"  %4s  %-*s  %-*s  %8s\n",
		"#",
		gapBarW+10, "INTER-TURN GAP",
		apiBarW+8, "API LATENCY",
		"CYCLE",
	)))
	b.WriteString(styleDivider.Render("  "+strings.Repeat("─", w-2)) + "\n")

	// Compute max values for bar scaling.
	maxGap := time.Duration(1)
	maxAPI := time.Duration(1)
	for _, t := range a.Turns {
		if t.GapBefore > maxGap {
			maxGap = t.GapBefore
		}
		if t.APILatency > maxAPI {
			maxAPI = t.APILatency
		}
	}

	pageH := contentH - 10
	if pageH < 3 {
		pageH = 3
	}
	start := m.scrollOffset
	if start >= len(a.Turns) {
		start = max(0, len(a.Turns)-1)
	}
	end := start + pageH
	if end > len(a.Turns) {
		end = len(a.Turns)
	}

	for i := start; i < end; i++ {
		t := a.Turns[i]
		slowest := (i == a.SlowestCycleTurn && a.MaxTurnCycle > 0)

		// Gap bar.
		var gapBar string
		var gapLabel string
		if i == 0 {
			gapBar = strings.Repeat("·", gapBarW)
			gapLabel = fmt.Sprintf("%-10s", "(start)")
		} else {
			filled := int(float64(gapBarW) * float64(t.GapBefore) / float64(maxGap))
			if filled > gapBarW {
				filled = gapBarW
			}
			gapColor := clrMuted
			trigLabel := "user"
			if t.IsToolTrigger {
				gapColor = clrWarn
				trigLabel = "tool"
			}
			barStr := strings.Repeat("█", filled) + strings.Repeat("·", gapBarW-filled)
			gapBar = lipgloss.NewStyle().Foreground(gapColor).Render(barStr)
			gapLabel = fmt.Sprintf("%-5s %-5s", trigLabel, analyzer.FormatDuration(t.GapBefore))
		}

		// API bar.
		apiFilled := int(float64(apiBarW) * float64(t.APILatency) / float64(maxAPI))
		if apiFilled > apiBarW {
			apiFilled = apiBarW
		}
		apiBarStr := strings.Repeat("█", apiFilled) + strings.Repeat("·", apiBarW-apiFilled)
		apiBar := lipgloss.NewStyle().Foreground(clrPrimary).Render(apiBarStr)
		apiLabel := analyzer.FormatDuration(t.APILatency)

		cycleStr := analyzer.FormatDuration(t.TurnCycle)
		marker := ""
		if slowest {
			marker = styleError.Render(" ←")
		}

		b.WriteString(fmt.Sprintf("  %4d  %s %-10s  %s %-8s  %8s%s\n",
			t.TurnIdx+1,
			gapBar, gapLabel,
			apiBar, apiLabel,
			cycleStr,
			marker,
		))
	}

	if len(a.Turns) > pageH {
		b.WriteString(styleMuted.Render(fmt.Sprintf(
			"\n  %d–%d of %d turns  (↑↓ to scroll)",
			start+1, end, len(a.Turns),
		)) + "\n")
	}

	return b.String()
}

// ---- Timing flame / Gantt view ----

func (m Model) viewTimingFlame(contentH int) string {
	a := m.analysis
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString("  " + styleBold.Render("Flame Chart") + "  " +
		styleMuted.Render("f · back to table  ↑↓ scroll") + "\n\n")

	if a.TotalAPITime == 0 {
		b.WriteString(styleMuted.Render("  No timing data available.") + "\n")
		return b.String()
	}

	// Session time bounds: first RequestTime → last Timestamp.
	sessionStart := time.Time{}
	for _, t := range a.Turns {
		if !t.RequestTime.IsZero() {
			sessionStart = t.RequestTime
			break
		}
	}
	if sessionStart.IsZero() {
		sessionStart = a.Turns[0].Timestamp
	}
	sessionEnd := a.Turns[len(a.Turns)-1].Timestamp
	totalDur := sessionEnd.Sub(sessionStart)
	if totalDur <= 0 {
		b.WriteString(styleMuted.Render("  Insufficient timing data.") + "\n")
		return b.String()
	}

	labelW := 5 // "T123 "
	chartW := m.width - labelW - 6
	if chartW < 20 {
		chartW = 20
	}
	if chartW > 120 {
		chartW = 120
	}

	secPerChar := totalDur.Seconds() / float64(chartW)

	// toPos converts an absolute time to a chart column index [0, chartW].
	toPos := func(t time.Time) int {
		if t.IsZero() {
			return 0
		}
		p := int(t.Sub(sessionStart).Seconds() / secPerChar)
		if p < 0 {
			p = 0
		}
		if p > chartW {
			p = chartW
		}
		return p
	}

	// Header: scale info + time ruler.
	b.WriteString(styleMuted.Render(fmt.Sprintf(
		"  start %s  ·  total %s  ·  1 char ≈ %s\n",
		sessionStart.Format("15:04:05"),
		analyzer.FormatDuration(totalDur),
		analyzer.FormatDuration(time.Duration(float64(time.Second)*secPerChar)),
	)))
	b.WriteString("\n")
	// Ruler: tick every ~15 chars.
	ruler := make([]byte, chartW)
	for i := range ruler {
		if i%15 == 0 {
			ruler[i] = '|'
		} else {
			ruler[i] = '·'
		}
	}
	b.WriteString("  " + strings.Repeat(" ", labelW) + styleMuted.Render(string(ruler)) + "\n")
	b.WriteString(styleDivider.Render("  "+strings.Repeat("─", m.width-4)) + "\n")

	// Per-turn rows.
	pageH := contentH - 9
	if pageH < 2 {
		pageH = 2
	}
	start := m.scrollOffset
	if start >= len(a.Turns) {
		start = max(0, len(a.Turns)-1)
	}
	end := start + pageH
	if end > len(a.Turns) {
		end = len(a.Turns)
	}

	for i := start; i < end; i++ {
		t := a.Turns[i]

		// Compute character positions for gap and API segments.
		apiStartPos := toPos(t.RequestTime)
		apiEndPos := toPos(t.Timestamp)
		// Guarantee minimum 1 char for any non-zero duration.
		if t.APILatency > 0 && apiEndPos <= apiStartPos {
			apiEndPos = apiStartPos + 1
		}
		if apiEndPos > chartW {
			apiEndPos = chartW
		}

		leadSpaces := 0
		gapLen := 0
		gapColor := clrMuted
		gapChar := "░"

		if i > 0 {
			gapStartPos := toPos(a.Turns[i-1].Timestamp)
			gapEndPos := apiStartPos
			if t.GapBefore > 0 && gapEndPos <= gapStartPos {
				gapEndPos = gapStartPos + 1
				// Push API bar right to avoid overlap.
				if gapEndPos > apiStartPos {
					apiStartPos = gapEndPos
					if apiEndPos <= apiStartPos {
						apiEndPos = apiStartPos + 1
					}
				}
			}
			if gapEndPos > chartW {
				gapEndPos = chartW
			}
			leadSpaces = gapStartPos
			gapLen = gapEndPos - gapStartPos
			if gapLen < 0 {
				gapLen = 0
			}
			if t.IsToolTrigger {
				gapColor = clrWarn
				gapChar = "▒"
			}
		} else {
			leadSpaces = apiStartPos
		}

		apiLen := apiEndPos - apiStartPos
		if apiLen < 0 {
			apiLen = 0
		}
		tailSpaces := chartW - leadSpaces - gapLen - apiLen
		if tailSpaces < 0 {
			tailSpaces = 0
		}

		// Build styled row string.
		row := strings.Repeat(" ", leadSpaces)
		if gapLen > 0 {
			row += lipgloss.NewStyle().Foreground(gapColor).Render(strings.Repeat(gapChar, gapLen))
		}
		if apiLen > 0 {
			row += lipgloss.NewStyle().Foreground(clrPrimary).Render(strings.Repeat("█", apiLen))
		}
		row += strings.Repeat(" ", tailSpaces)

		// Slowest cycle marker.
		marker := ""
		if i == a.SlowestCycleTurn && a.MaxTurnCycle > 0 {
			marker = "  " + styleError.Render("← "+analyzer.FormatDuration(t.TurnCycle))
		}

		label := fmt.Sprintf("T%-3d ", t.TurnIdx+1)
		b.WriteString("  " + styleMuted.Render(label) + row + marker + "\n")
	}

	if len(a.Turns) > pageH {
		b.WriteString(styleMuted.Render(fmt.Sprintf(
			"\n  %d–%d of %d  (↑↓ to scroll)",
			start+1, end, len(a.Turns),
		)) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  %s API  %s tool exec  %s user idle\n",
		lipgloss.NewStyle().Foreground(clrPrimary).Render("█"),
		lipgloss.NewStyle().Foreground(clrWarn).Render("▒"),
		styleMuted.Render("░"),
	))

	return b.String()
}

// ---- Tokens tab ----

func (m Model) viewTokens(contentH int) string {
	a := m.analysis
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString("  " + styleBold.Render("Cumulative Token Consumption") + "\n\n")

	w := m.width - 4
	chartW := w - 20
	if chartW < 20 {
		chartW = 20
	}

	// Spark-bar chart: one row per turn, bar = cumulative total tokens.
	pageH := contentH - 5
	if pageH < 5 {
		pageH = 5
	}

	n := len(a.Turns)
	start := m.scrollOffset
	if start >= n {
		start = max(0, n-1)
	}
	end := start + pageH
	if end > n {
		end = n
	}

	if n == 0 {
		b.WriteString(styleMuted.Render("  No turns.") + "\n")
		return b.String()
	}

	maxCum := a.Turns[len(a.Turns)-1].CumTotal
	if maxCum == 0 {
		maxCum = 1
	}

	b.WriteString(styleMuted.Render(fmt.Sprintf(
		"  %4s  %-"+fmt.Sprintf("%d", chartW)+"s  %10s  %8s\n",
		"#", "CUMULATIVE TOTAL", "TOKENS", "Δ OUTPUT",
	)))
	b.WriteString(styleDivider.Render("  "+strings.Repeat("─", w-2)) + "\n")

	for i := start; i < end; i++ {
		t := a.Turns[i]
		pct := float64(t.CumTotal) / float64(maxCum) * 100
		bar := lipgloss.NewStyle().Foreground(clrAccent).Render(analyzer.Bar(pct, chartW))
		b.WriteString(fmt.Sprintf("  %4d  %s  %10s  %8s\n",
			t.TurnIdx+1,
			bar,
			analyzer.FormatTokens(t.CumTotal),
			"+"+analyzer.FormatTokens(t.Output),
		))
	}

	if n > pageH {
		b.WriteString(styleMuted.Render(fmt.Sprintf(
			"\n  Showing %d–%d of %d  (↑↓ to scroll)",
			start+1, end, n,
		)) + "\n")
	}

	b.WriteString("\n")
	b.WriteString("  " + styleBold.Render("Per-Turn Input Breakdown") + "\n\n")

	// Show averages
	b.WriteString(fmt.Sprintf("  %-22s  %s\n",
		"Avg input/turn", analyzer.FormatTokens(int(a.AvgInputPerTurn))))
	b.WriteString(fmt.Sprintf("  %-22s  %s\n",
		"Avg output/turn", analyzer.FormatTokens(int(a.AvgOutputPerTurn))))
	b.WriteString(fmt.Sprintf("  %-22s  %.1f%%\n",
		"Overall cache hit rate", a.OverallCacheHitPct))
	if a.PeakInputTurn < len(a.Turns) {
		pk := a.Turns[a.PeakInputTurn]
		b.WriteString(fmt.Sprintf("  %-22s  turn %d (%s)\n",
			"Peak input turn",
			pk.TurnIdx+1,
			analyzer.FormatTokens(pk.Input+pk.CacheRead)))
	}

	return b.String()
}

// ---- Tools tab ----

func (m Model) viewTools(contentH int) string {
	a := m.analysis
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  %s  %s total\n\n",
		styleBold.Render("Tool Usage"),
		styleMuted.Render(fmt.Sprintf("%d calls across %d types", a.ToolCallCount, len(a.ToolCounts))),
	))

	if len(a.ToolOrder) == 0 {
		b.WriteString(styleMuted.Render("  No tool calls recorded.\n"))
		return b.String()
	}

	maxCount := a.ToolCounts[a.ToolOrder[0]]
	barW := m.width - 36
	if barW < 10 {
		barW = 10
	}

	b.WriteString(styleMuted.Render(fmt.Sprintf("  %-20s  %-*s  %6s  %5s\n",
		"TOOL", barW, "", "CALLS", "SHARE")) + "\n")
	b.WriteString(styleDivider.Render("  "+strings.Repeat("─", m.width-4)) + "\n")

	pageH := contentH - 6
	if pageH < 1 {
		pageH = 1
	}
	start := m.scrollOffset
	if start >= len(a.ToolOrder) {
		start = max(0, len(a.ToolOrder)-1)
	}
	end := start + pageH
	if end > len(a.ToolOrder) {
		end = len(a.ToolOrder)
	}

	for i := start; i < end; i++ {
		name := a.ToolOrder[i]
		count := a.ToolCounts[name]
		pct := float64(count) / float64(maxCount) * 100
		sharePct := 0.0
		if a.ToolCallCount > 0 {
			sharePct = float64(count) / float64(a.ToolCallCount) * 100
		}
		bar := lipgloss.NewStyle().Foreground(clrAccent).Render(analyzer.Bar(pct, barW))
		b.WriteString(fmt.Sprintf("  %-20s  %s  %6d  %4.0f%%\n",
			name, bar, count, sharePct))
	}

	return b.String()
}

// ---- AI Analysis tab ----

func (m Model) viewAIAnalysis(contentH int) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("  " + styleBold.Render("AI Analysis") + "  " +
		styleMuted.Render("press a to run · uses claude -p") + "\n\n")

	switch {
	case m.aiLoading:
		b.WriteString(styleMuted.Render("  ◌  Running claude analysis…") + "\n")
	case m.aiErr != nil:
		b.WriteString(styleError.Render("  error: "+m.aiErr.Error()) + "\n")
	case m.aiText == "":
		b.WriteString(styleMuted.Render("  No analysis yet. Press a to invoke claude.") + "\n")
	default:
		lines := strings.Split(m.aiText, "\n")
		start := m.scrollOffset
		if start >= len(lines) {
			start = max(0, len(lines)-1)
		}
		end := start + contentH - 4
		if end > len(lines) {
			end = len(lines)
		}
		for _, line := range lines[start:end] {
			b.WriteString("  " + line + "\n")
		}
		if len(lines) > contentH-4 {
			b.WriteString(styleMuted.Render(fmt.Sprintf(
				"\n  lines %d–%d of %d  (↑↓ to scroll)",
				start+1, end, len(lines),
			)) + "\n")
		}
	}

	return b.String()
}

// ---- Shared chrome ----

func (m Model) renderHeader(title, subtitle string) string {
	w := m.width
	left := styleTitle.Render(title)
	right := styleMuted.Render(subtitle)

	gap := w - lipgloss.Width(left) - lipgloss.Width(right) - 4
	if gap < 1 {
		gap = 1
	}
	line := "  " + left + strings.Repeat(" ", gap) + right
	divider := styleDivider.Render(strings.Repeat("─", w))
	return line + "\n" + divider + "\n"
}

func (m Model) renderTabBar() string {
	var parts []string
	for i, label := range tabLabels {
		t := tab(i)
		if t == m.activeTab {
			parts = append(parts, styleActiveTab.Render(label))
		} else {
			parts = append(parts, styleInactiveTab.Render(label))
		}
	}
	bar := "  " + strings.Join(parts, styleMuted.Render("  ·  "))
	divider := styleDivider.Render(strings.Repeat("─", m.width))
	return bar + "\n" + divider + "\n"
}

func (m Model) renderFooter(help string) string {
	divider := styleDivider.Render(strings.Repeat("─", m.width))
	return divider + "\n" + styleMuted.Render("  "+help) + "\n"
}

// ---- Utility ----

func renderStatGrid(col1, col2 [][]string, w int) string {
	colW := (w - 4) / 2
	var b strings.Builder
	rows := max(len(col1), len(col2))
	for i := 0; i < rows; i++ {
		var l, r string
		if i < len(col1) {
			l = fmt.Sprintf("%-16s  %s", styleMuted.Render(col1[i][0]), col1[i][1])
		}
		if i < len(col2) {
			r = fmt.Sprintf("%-18s  %s", styleMuted.Render(col2[i][0]), col2[i][1])
		}
		if lipgloss.Width(l) > colW {
			l = truncate(l, colW)
		}
		b.WriteString(fmt.Sprintf("  %-*s  %s\n", colW, l, r))
	}
	return b.String()
}

func wordWrap(text string, width int) []string {
	words := strings.Fields(text)
	var lines []string
	var cur string
	for _, w := range words {
		if cur == "" {
			cur = w
		} else if len(cur)+1+len(w) <= width {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func formatRelTime(t time.Time) string {
	now := time.Now()
	diff := now.Sub(t)
	switch {
	case diff < 24*time.Hour && t.Day() == now.Day():
		return "today " + t.Format("15:04")
	case diff < 48*time.Hour:
		return "yesterday " + t.Format("15:04")
	default:
		return t.Format("Jan 2 15:04")
	}
}

func formatSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(bytes)/1024/1024)
	case bytes >= 1024:
		return fmt.Sprintf("%.0fkB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ParseAndRun parses a session file and opens the profile view directly.
// Used when claudeprof is invoked with a session file path argument.
func ParseAndRun(claudeDir, sessionFile string) error {
	m, err := New(claudeDir, sessionFile)
	if err != nil {
		return err
	}

	// Pre-load the session so the TUI opens directly in profile view.
	sess, err := session.ParseFile(sessionFile)
	if err != nil {
		return fmt.Errorf("parsing session: %w", err)
	}
	m.sess = sess
	m.analysis = analyzer.Analyze(sess)
	m.state = stateProfile

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, runErr := p.Run()
	return runErr
}
