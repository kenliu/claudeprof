// Package report generates a standalone HTML profiling report from a session analysis.
package report

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"claudeprof/analyzer"
)

// TemplateData is the data passed to the HTML template.
type TemplateData struct {
	SessionID    string
	CWD          string
	Model        string
	Duration     string
	StartTime    string
	TurnCount    int
	ToolCallCount int
	HasSummary   bool
	Generated    string

	// Token totals
	TotalInput      int
	TotalCacheRead  int
	TotalCacheCreate int
	TotalOutput     int
	TotalAll        int

	CacheHitPct   float64
	ContextGrowth float64

	// JSON-encoded chart datasets
	TurnLabelsJSON   template.JS
	CumInputJSON     template.JS
	CumCacheReadJSON template.JS
	CumOutputJSON    template.JS
	ToolNamesJSON    template.JS
	ToolCountsJSON   template.JS

	// Timing summary (seconds as float64 for JS)
	TotalAPITimeSec  float64
	TotalToolTimeSec float64
	TotalUserTimeSec float64
	AvgAPILatencySec float64
	HasTimingData    bool

	// Timing chart datasets (seconds per turn)
	APILatencyJSON template.JS
	ToolGapJSON    template.JS
	UserGapJSON    template.JS

	// Gantt chart datasets: [startSec, endSec] pairs per turn (null when absent)
	GanttAPIJSON      template.JS // [[start,end], ...]
	GanttToolGapJSON  template.JS // [[start,end] or null, ...]
	GanttUserGapJSON  template.JS // [[start,end] or null, ...]

	// Table rows
	TurnRows []TurnRow

	// Suggestions
	Suggestions []analyzer.Suggestion
}

// TurnRow is a single row in the turn detail table.
type TurnRow struct {
	Num        int
	Time       string
	UserText   string
	ToolNames  string
	Input      string
	Output     string
	CacheRead  string
	CacheHit   float64
	HitClass   string // "good" | "warn" | "bad"
	APILatency string
	GapBefore  string
	GapType    string // "tool" | "user" | "—"
	CycleTime  string
}

// Generate produces an HTML report for the given analysis and opens it in the browser.
// If outputPath is non-empty, the report is written there; otherwise a temp file is used.
func Generate(a *analyzer.Analysis, outputPath string) (string, error) {
	td := buildTemplateData(a)

	tmpl, err := template.New("report").Parse(htmlTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	// Determine output path.
	var outFile *os.File
	if outputPath != "" {
		outFile, err = os.Create(outputPath)
		if err != nil {
			return "", fmt.Errorf("creating output file: %w", err)
		}
	} else {
		outFile, err = os.CreateTemp("", "claudeprof-*.html")
		if err != nil {
			return "", fmt.Errorf("creating temp file: %w", err)
		}
	}
	defer outFile.Close()

	if err := tmpl.Execute(outFile, td); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	outPath := outFile.Name()

	if err := openBrowser("file://" + filepath.ToSlash(outPath)); err != nil {
		return outPath, fmt.Errorf("opening browser: %w", err)
	}
	return outPath, nil
}

func buildTemplateData(a *analyzer.Analysis) TemplateData {
	s := a.Session

	// Per-turn data for charts.
	var turnLabels []string
	var cumInput, cumCacheRead, cumOutput []int
	var apiLatencySecs, toolGapSecs, userGapSecs []float64

	// Gantt: compute session start for absolute positions.
	sessionStart := time.Time{}
	for _, t := range a.Turns {
		if !t.RequestTime.IsZero() {
			sessionStart = t.RequestTime
			break
		}
	}
	type floatPair [2]float64
	type nullablePair struct {
		val   floatPair
		valid bool
	}
	var ganttAPI, ganttTool, ganttUser []interface{}

	for i, t := range a.Turns {
		turnLabels = append(turnLabels, fmt.Sprintf("#%d", t.TurnIdx+1))
		cumInput = append(cumInput, t.CumInput)
		cumCacheRead = append(cumCacheRead, t.CumCacheRead)
		cumOutput = append(cumOutput, t.CumOutput)

		apiLatencySecs = append(apiLatencySecs, t.APILatency.Seconds())
		if t.IsToolTrigger {
			toolGapSecs = append(toolGapSecs, t.GapBefore.Seconds())
			userGapSecs = append(userGapSecs, 0)
		} else {
			toolGapSecs = append(toolGapSecs, 0)
			userGapSecs = append(userGapSecs, t.GapBefore.Seconds())
		}

		// Gantt chart: absolute positions in seconds from session start.
		if !sessionStart.IsZero() && !t.RequestTime.IsZero() && !t.Timestamp.IsZero() {
			apiStartSec := t.RequestTime.Sub(sessionStart).Seconds()
			apiEndSec := t.Timestamp.Sub(sessionStart).Seconds()
			ganttAPI = append(ganttAPI, floatPair{apiStartSec, apiEndSec})

			if i > 0 && t.GapBefore > 0 {
				prevEnd := a.Turns[i-1].Timestamp.Sub(sessionStart).Seconds()
				gapEnd := t.RequestTime.Sub(sessionStart).Seconds()
				if t.IsToolTrigger {
					ganttTool = append(ganttTool, floatPair{prevEnd, gapEnd})
					ganttUser = append(ganttUser, nil)
				} else {
					ganttTool = append(ganttTool, nil)
					ganttUser = append(ganttUser, floatPair{prevEnd, gapEnd})
				}
			} else {
				ganttTool = append(ganttTool, nil)
				ganttUser = append(ganttUser, nil)
			}
		} else {
			ganttAPI = append(ganttAPI, nil)
			ganttTool = append(ganttTool, nil)
			ganttUser = append(ganttUser, nil)
		}
	}

	// Tool data.
	toolNames := a.ToolOrder
	toolCounts := make([]int, len(toolNames))
	for i, name := range toolNames {
		toolCounts[i] = a.ToolCounts[name]
	}

	// Build turn rows.
	var rows []TurnRow
	for i, t := range a.Turns {
		hitClass := "good"
		if t.CacheHitPct < 30 {
			hitClass = "bad"
		} else if t.CacheHitPct < 60 {
			hitClass = "warn"
		}

		toolStr := ""
		if len(t.ToolNames) > 0 {
			toolStr = joinTools(t.ToolNames)
		}

		gapType := "—"
		gapStr := "—"
		if i > 0 && t.GapBefore > 0 {
			if t.IsToolTrigger {
				gapType = "tool"
			} else {
				gapType = "user"
			}
			gapStr = analyzer.FormatDuration(t.GapBefore)
		}

		apiStr := "—"
		if t.APILatency > 0 {
			apiStr = analyzer.FormatDuration(t.APILatency)
		}

		cycleStr := "—"
		if t.TurnCycle > 0 {
			cycleStr = analyzer.FormatDuration(t.TurnCycle)
		}

		rows = append(rows, TurnRow{
			Num:        t.TurnIdx + 1,
			Time:       formatTime(t.Timestamp),
			UserText:   truncate(t.UserText, 80),
			ToolNames:  toolStr,
			Input:      analyzer.FormatTokens(t.Input),
			Output:     analyzer.FormatTokens(t.Output),
			CacheRead:  analyzer.FormatTokens(t.CacheRead),
			CacheHit:   t.CacheHitPct,
			HitClass:   hitClass,
			APILatency: apiStr,
			GapBefore:  gapStr,
			GapType:    gapType,
			CycleTime:  cycleStr,
		})
	}

	td := TemplateData{
		SessionID:     s.ID,
		CWD:           s.CWD,
		Model:         s.Model,
		Duration:      analyzer.FormatDuration(s.Duration()),
		StartTime:     s.StartTime.Format("2006-01-02 15:04:05"),
		TurnCount:     a.TurnCount,
		ToolCallCount: a.ToolCallCount,
		HasSummary:    s.HasSummary,
		Generated:     time.Now().Format("2006-01-02 15:04:05"),

		TotalInput:       a.TotalInput,
		TotalCacheRead:   a.TotalCacheRead,
		TotalCacheCreate: a.TotalCacheCreate,
		TotalOutput:      a.TotalOutput,
		TotalAll:         a.TotalTokens(),

		CacheHitPct:   a.OverallCacheHitPct,
		ContextGrowth: a.ContextGrowthRate,

		TotalAPITimeSec:  a.TotalAPITime.Seconds(),
		TotalToolTimeSec: a.TotalToolTime.Seconds(),
		TotalUserTimeSec: a.TotalUserTime.Seconds(),
		AvgAPILatencySec: a.AvgAPILatency.Seconds(),
		HasTimingData:    a.TotalAPITime > 0,

		TurnRows:    rows,
		Suggestions: a.Suggestions,
	}

	td.TurnLabelsJSON = toJS(turnLabels)
	td.CumInputJSON = toJS(cumInput)
	td.CumCacheReadJSON = toJS(cumCacheRead)
	td.CumOutputJSON = toJS(cumOutput)
	td.ToolNamesJSON = toJS(toolNames)
	td.ToolCountsJSON = toJS(toolCounts)
	td.APILatencyJSON = toJS(apiLatencySecs)
	td.ToolGapJSON = toJS(toolGapSecs)
	td.UserGapJSON = toJS(userGapSecs)
	td.GanttAPIJSON = toJS(ganttAPI)
	td.GanttToolGapJSON = toJS(ganttTool)
	td.GanttUserGapJSON = toJS(ganttUser)

	return td
}

func toJS(v interface{}) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b)
}

func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("15:04:05")
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

func joinTools(names []string) string {
	counts := map[string]int{}
	var order []string
	for _, n := range names {
		if counts[n] == 0 {
			order = append(order, n)
		}
		counts[n]++
	}
	result := ""
	for _, n := range order {
		if result != "" {
			result += " "
		}
		if counts[n] > 1 {
			result += fmt.Sprintf("%s×%d", n, counts[n])
		} else {
			result += n
		}
	}
	return result
}

// ---- HTML Template ----

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>claudeprof · {{.SessionID}}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans:wght@300;400;500;600&display=swap" rel="stylesheet">
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
<style>
  :root {
    --bg:        #080C14;
    --surface:   #0E1422;
    --surface2:  #141928;
    --border:    #1E2740;
    --border2:   #2A3558;
    --text:      #D4DCEF;
    --muted:     #5A6B8A;
    --dim:       #344060;
    --blue:      #4F8EF7;
    --blue-dim:  #1E3A6E;
    --green:     #3DD68C;
    --green-dim: #0F3D25;
    --amber:     #F5A623;
    --amber-dim: #3D2800;
    --red:       #F45B5B;
    --red-dim:   #3D1010;
    --purple:    #9B7FEA;
    --teal:      #2ECFD4;
    --radius:    6px;
    --mono:      'IBM Plex Mono', monospace;
    --sans:      'IBM Plex Sans', sans-serif;
  }
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  html { font-size: 14px; }
  body {
    background: var(--bg);
    color: var(--text);
    font-family: var(--sans);
    line-height: 1.6;
    min-height: 100vh;
  }

  /* ---- Layout ---- */
  .page { max-width: 1200px; margin: 0 auto; padding: 0 24px 64px; }

  /* ---- Header ---- */
  header {
    border-bottom: 1px solid var(--border);
    padding: 24px 0 20px;
    margin-bottom: 32px;
  }
  .header-top {
    display: flex;
    align-items: baseline;
    gap: 12px;
    margin-bottom: 6px;
  }
  .logo {
    font-family: var(--mono);
    font-size: 11px;
    font-weight: 600;
    letter-spacing: 0.15em;
    text-transform: uppercase;
    color: var(--blue);
    background: var(--blue-dim);
    padding: 3px 8px;
    border-radius: 3px;
  }
  .session-id {
    font-family: var(--mono);
    font-size: 13px;
    color: var(--muted);
  }
  h1 {
    font-family: var(--sans);
    font-size: 20px;
    font-weight: 500;
    color: var(--text);
  }
  .header-meta {
    display: flex;
    gap: 24px;
    margin-top: 12px;
    flex-wrap: wrap;
  }
  .meta-item {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .meta-label {
    font-family: var(--mono);
    font-size: 10px;
    letter-spacing: 0.1em;
    text-transform: uppercase;
    color: var(--muted);
  }
  .meta-value {
    font-family: var(--mono);
    font-size: 13px;
    color: var(--text);
  }

  /* ---- Section ---- */
  section { margin-bottom: 40px; }
  .section-title {
    font-family: var(--mono);
    font-size: 10px;
    font-weight: 600;
    letter-spacing: 0.12em;
    text-transform: uppercase;
    color: var(--muted);
    margin-bottom: 16px;
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .section-title::after {
    content: '';
    flex: 1;
    height: 1px;
    background: var(--border);
  }

  /* ---- Metric cards ---- */
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
    gap: 12px;
    margin-bottom: 32px;
  }
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 16px 18px;
    position: relative;
    overflow: hidden;
  }
  .card::before {
    content: '';
    position: absolute;
    top: 0; left: 0; right: 0;
    height: 2px;
    background: var(--blue);
  }
  .card.green::before { background: var(--green); }
  .card.amber::before { background: var(--amber); }
  .card.purple::before { background: var(--purple); }
  .card.teal::before  { background: var(--teal); }
  .card-label {
    font-family: var(--mono);
    font-size: 10px;
    letter-spacing: 0.1em;
    text-transform: uppercase;
    color: var(--muted);
    margin-bottom: 8px;
  }
  .card-value {
    font-family: var(--mono);
    font-size: 22px;
    font-weight: 600;
    color: var(--text);
    line-height: 1.2;
  }
  .card-sub {
    font-family: var(--mono);
    font-size: 11px;
    color: var(--muted);
    margin-top: 4px;
  }

  /* ---- Token breakdown bar ---- */
  .token-breakdown { margin-bottom: 32px; }
  .token-bar-track {
    height: 10px;
    background: var(--surface2);
    border-radius: 5px;
    overflow: hidden;
    display: flex;
    margin-bottom: 16px;
  }
  .token-bar-seg { height: 100%; transition: width 0.3s; }
  .token-legend {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    gap: 8px;
  }
  .token-legend-item {
    display: flex;
    align-items: center;
    gap: 10px;
    font-family: var(--mono);
    font-size: 12px;
  }
  .legend-dot {
    width: 10px; height: 10px;
    border-radius: 2px;
    flex-shrink: 0;
  }
  .legend-name { color: var(--muted); flex: 1; }
  .legend-val  { color: var(--text); font-weight: 500; }
  .legend-pct  { color: var(--dim); }

  /* ---- Charts ---- */
  .charts-grid {
    display: grid;
    grid-template-columns: 1fr 360px;
    gap: 16px;
    margin-bottom: 40px;
  }
  @media (max-width: 900px) {
    .charts-grid { grid-template-columns: 1fr; }
  }
  .chart-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 20px;
  }
  .chart-title {
    font-family: var(--mono);
    font-size: 11px;
    font-weight: 600;
    letter-spacing: 0.1em;
    text-transform: uppercase;
    color: var(--muted);
    margin-bottom: 16px;
  }
  .chart-wrap { position: relative; }

  /* ---- Suggestions ---- */
  .suggestions { display: flex; flex-direction: column; gap: 10px; }
  .suggestion {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 14px 16px;
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 10px 14px;
    align-items: start;
  }
  .sug-dot {
    width: 8px; height: 8px;
    border-radius: 50%;
    margin-top: 6px;
    flex-shrink: 0;
  }
  .sug-dot.high   { background: var(--red); box-shadow: 0 0 6px var(--red); }
  .sug-dot.medium { background: var(--amber); box-shadow: 0 0 6px var(--amber); }
  .sug-dot.low    { background: var(--green); }
  .sug-body {}
  .sug-title {
    font-weight: 600;
    font-size: 13px;
    color: var(--text);
    margin-bottom: 4px;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .sug-category {
    font-family: var(--mono);
    font-size: 10px;
    color: var(--muted);
    background: var(--surface2);
    padding: 2px 6px;
    border-radius: 3px;
    font-weight: 400;
  }
  .sug-detail {
    font-size: 12px;
    color: var(--muted);
    line-height: 1.7;
  }

  /* ---- Table ---- */
  .table-wrap {
    overflow-x: auto;
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-family: var(--mono);
    font-size: 12px;
  }
  th {
    background: var(--surface2);
    color: var(--muted);
    font-weight: 500;
    font-size: 10px;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    text-align: left;
    padding: 10px 14px;
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }
  td {
    padding: 8px 14px;
    border-bottom: 1px solid var(--border);
    color: var(--text);
    vertical-align: top;
  }
  tr:last-child td { border-bottom: none; }
  tr:hover td { background: var(--surface2); }
  .td-num { color: var(--muted); text-align: right; }
  .td-right { text-align: right; }
  .td-prompt { max-width: 320px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .td-tools { max-width: 200px; color: var(--blue); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .hit-good { color: var(--green); }
  .hit-warn { color: var(--amber); }
  .hit-bad  { color: var(--red); }

  /* ---- Footer ---- */
  footer {
    border-top: 1px solid var(--border);
    padding-top: 20px;
    font-family: var(--mono);
    font-size: 11px;
    color: var(--muted);
    display: flex;
    justify-content: space-between;
  }
</style>
</head>
<body>
<div class="page">

<header>
  <div class="header-top">
    <span class="logo">claudeprof</span>
    <span class="session-id">{{.SessionID}}</span>
  </div>
  <h1>{{.CWD}}</h1>
  <div class="header-meta">
    <div class="meta-item">
      <span class="meta-label">Started</span>
      <span class="meta-value">{{.StartTime}}</span>
    </div>
    <div class="meta-item">
      <span class="meta-label">Duration</span>
      <span class="meta-value">{{.Duration}}</span>
    </div>
    <div class="meta-item">
      <span class="meta-label">Model</span>
      <span class="meta-value">{{.Model}}</span>
    </div>
    <div class="meta-item">
      <span class="meta-label">Turns</span>
      <span class="meta-value">{{.TurnCount}}</span>
    </div>
    <div class="meta-item">
      <span class="meta-label">Tool Calls</span>
      <span class="meta-value">{{.ToolCallCount}}</span>
    </div>{{if .HasSummary}}
    <div class="meta-item">
      <span class="meta-label">Note</span>
      <span class="meta-value" style="color:var(--amber)">compacted</span>
    </div>{{end}}
  </div>
</header>

<!-- Metric Cards -->
<section>
  <div class="section-title">Session Metrics</div>
  <div class="cards">
    <div class="card">
      <div class="card-label">Total Tokens</div>
      <div class="card-value" id="total-tokens">—</div>
      <div class="card-sub">all input + output</div>
    </div>
    <div class="card green">
      <div class="card-label">Cache Hit Rate</div>
      <div class="card-value" id="cache-rate">—</div>
      <div class="card-sub">cache_read / total_input</div>
    </div>
    <div class="card amber">
      <div class="card-label">Avg Input / Turn</div>
      <div class="card-value" id="avg-input">—</div>
      <div class="card-sub">context size proxy</div>
    </div>
    <div class="card purple">
      <div class="card-label">Context Growth</div>
      <div class="card-value" id="ctx-growth">—</div>
      <div class="card-sub">Q1 → Q4 input ratio</div>
    </div>
    <div class="card teal">
      <div class="card-label">Output Tokens</div>
      <div class="card-value" id="total-output">—</div>
      <div class="card-sub">{{.TotalOutput}} total</div>
    </div>
  </div>
</section>

{{if .HasTimingData}}
<!-- Timing Cards -->
<section>
  <div class="section-title">Timing Breakdown</div>
  <div class="cards">
    <div class="card">
      <div class="card-label">API Wait</div>
      <div class="card-value" id="api-time">—</div>
      <div class="card-sub">waiting for Claude</div>
    </div>
    <div class="card amber">
      <div class="card-label">Tool Execution</div>
      <div class="card-value" id="tool-time">—</div>
      <div class="card-sub">running tools between turns</div>
    </div>
    <div class="card">
      <div class="card-label">User Idle</div>
      <div class="card-value" id="user-time">—</div>
      <div class="card-sub">user think / type time</div>
    </div>
    <div class="card green">
      <div class="card-label">Avg API Latency</div>
      <div class="card-value" id="avg-api">—</div>
      <div class="card-sub">per turn</div>
    </div>
  </div>
</section>
{{end}}

<!-- Token Breakdown -->
<section>
  <div class="section-title">Token Distribution</div>
  <div class="token-breakdown">
    <div class="token-bar-track">
      <div class="token-bar-seg" id="bar-input"  style="background:var(--blue)"></div>
      <div class="token-bar-seg" id="bar-cache"  style="background:var(--green)"></div>
      <div class="token-bar-seg" id="bar-create" style="background:var(--purple)"></div>
      <div class="token-bar-seg" id="bar-output" style="background:var(--amber)"></div>
    </div>
    <div class="token-legend">
      <div class="token-legend-item">
        <div class="legend-dot" style="background:var(--blue)"></div>
        <span class="legend-name">Uncached Input</span>
        <span class="legend-val" id="leg-input">—</span>
        <span class="legend-pct" id="leg-input-pct"></span>
      </div>
      <div class="token-legend-item">
        <div class="legend-dot" style="background:var(--green)"></div>
        <span class="legend-name">Cache Read</span>
        <span class="legend-val" id="leg-cache">—</span>
        <span class="legend-pct" id="leg-cache-pct"></span>
      </div>
      <div class="token-legend-item">
        <div class="legend-dot" style="background:var(--purple)"></div>
        <span class="legend-name">Cache Creation</span>
        <span class="legend-val" id="leg-create">—</span>
        <span class="legend-pct" id="leg-create-pct"></span>
      </div>
      <div class="token-legend-item">
        <div class="legend-dot" style="background:var(--amber)"></div>
        <span class="legend-name">Output</span>
        <span class="legend-val" id="leg-output">—</span>
        <span class="legend-pct" id="leg-output-pct"></span>
      </div>
    </div>
  </div>
</section>

<!-- Charts -->
<section>
  <div class="section-title">Token Timeline</div>
  <div class="charts-grid">
    <div class="chart-card">
      <div class="chart-title">Cumulative Token Consumption</div>
      <div class="chart-wrap" style="height:260px">
        <canvas id="timelineChart"></canvas>
      </div>
    </div>
    <div class="chart-card">
      <div class="chart-title">Tool Usage</div>
      <div class="chart-wrap" style="height:260px">
        <canvas id="toolsChart"></canvas>
      </div>
    </div>
  </div>
</section>

{{if .HasTimingData}}
<!-- Timing Charts -->
<section>
  <div class="section-title">Timing Analysis</div>
  <div class="chart-card" style="margin-bottom:16px">
    <div class="chart-title">Session timeline — wall clock (seconds from start)</div>
    <div class="chart-wrap" style="height:320px">
      <canvas id="ganttChart"></canvas>
    </div>
  </div>
  <div class="chart-card" style="margin-bottom:40px">
    <div class="chart-title">Per-turn duration breakdown (seconds, stacked)</div>
    <div class="chart-wrap" style="height:240px">
      <canvas id="timingChart"></canvas>
    </div>
  </div>
</section>
{{end}}

<!-- Suggestions -->
<section>
  <div class="section-title">Optimization Suggestions</div>
  <div class="suggestions">
    {{range .Suggestions}}
    <div class="suggestion">
      <div class="sug-dot {{.Level}}"></div>
      <div class="sug-body">
        <div class="sug-title">
          {{.Title}}
          <span class="sug-category">{{.Category}}</span>
        </div>
        <div class="sug-detail">{{.Detail}}</div>
      </div>
    </div>
    {{end}}
  </div>
</section>

<!-- Turn Detail Table -->
<section>
  <div class="section-title">Turn Detail ({{.TurnCount}} turns)</div>
  <div class="table-wrap">
    <table>
      <thead>
        <tr>
          <th>#</th>
          <th>Time</th>
          <th>Prompt Preview</th>
          <th>Tools</th>
          <th>Input</th>
          <th>Output</th>
          <th>Cache Read</th>
          <th>Cache %</th>
          <th>Gap</th>
          <th>API Latency</th>
          <th>Cycle</th>
        </tr>
      </thead>
      <tbody>
        {{range .TurnRows}}
        <tr>
          <td class="td-num">{{.Num}}</td>
          <td style="white-space:nowrap">{{.Time}}</td>
          <td class="td-prompt">{{.UserText}}</td>
          <td class="td-tools">{{.ToolNames}}</td>
          <td class="td-right">{{.Input}}</td>
          <td class="td-right">{{.Output}}</td>
          <td class="td-right">{{.CacheRead}}</td>
          <td class="td-right {{.HitClass}}">{{printf "%.0f%%" .CacheHit}}</td>
          <td class="td-right" style="white-space:nowrap">{{if ne .GapType "—"}}<span style="font-size:10px;color:var(--muted)">{{.GapType}} </span>{{end}}{{.GapBefore}}</td>
          <td class="td-right">{{.APILatency}}</td>
          <td class="td-right">{{.CycleTime}}</td>
        </tr>
        {{end}}
      </tbody>
    </table>
  </div>
</section>

<footer>
  <span>claudeprof · Claude Code session profiler</span>
  <span>Generated {{.Generated}}</span>
</footer>

</div><!-- .page -->

<script>
(function() {
  // ---- Raw data from Go ----
  const data = {
    totalInput:   {{.TotalInput}},
    totalCache:   {{.TotalCacheRead}},
    totalCreate:  {{.TotalCacheCreate}},
    totalOutput:  {{.TotalOutput}},
    totalAll:     {{.TotalAll}},
    cacheHitPct:  {{printf "%.2f" .CacheHitPct}},
    ctxGrowth:    {{printf "%.2f" .ContextGrowth}},
    turnCount:    {{.TurnCount}},

    turnLabels:   {{.TurnLabelsJSON}},
    cumInput:     {{.CumInputJSON}},
    cumCacheRead: {{.CumCacheReadJSON}},
    cumOutput:    {{.CumOutputJSON}},
    toolNames:    {{.ToolNamesJSON}},
    toolCounts:   {{.ToolCountsJSON}},

    hasTimingData:  {{if .HasTimingData}}true{{else}}false{{end}},
    totalAPISec:    {{printf "%.2f" .TotalAPITimeSec}},
    totalToolSec:   {{printf "%.2f" .TotalToolTimeSec}},
    totalUserSec:   {{printf "%.2f" .TotalUserTimeSec}},
    avgAPISec:      {{printf "%.2f" .AvgAPILatencySec}},
    apiLatency:     {{.APILatencyJSON}},
    toolGap:        {{.ToolGapJSON}},
    userGap:        {{.UserGapJSON}},

    ganttAPI:       {{.GanttAPIJSON}},
    ganttTool:      {{.GanttToolGapJSON}},
    ganttUser:      {{.GanttUserGapJSON}},
  };

  // ---- Helpers ----
  function fmtTokens(n) {
    if (n >= 1e6) return (n/1e6).toFixed(2) + 'M';
    if (n >= 1e3) return (n/1e3).toFixed(1) + 'k';
    return String(n);
  }
  function pct(n, total) {
    if (!total) return '0.0%';
    return (n/total*100).toFixed(1) + '%';
  }

  // ---- Helpers ----
  function fmtSec(s) {
    if (s < 1)   return s.toFixed(1) + 's';
    if (s < 60)  return Math.round(s) + 's';
    const m = Math.floor(s / 60), r = Math.round(s % 60);
    if (s < 3600) return m + 'm' + (r ? r + 's' : '');
    const h = Math.floor(s / 3600), mm = Math.floor((s % 3600) / 60);
    return h + 'h' + (mm ? mm + 'm' : '');
  }
  function pctOf(a, b) { return b > 0 ? (a/b*100).toFixed(0) + '%' : '—'; }

  // ---- Metric cards ----
  document.getElementById('total-tokens').textContent = fmtTokens(data.totalAll);
  document.getElementById('cache-rate').textContent   = data.cacheHitPct.toFixed(1) + '%';
  document.getElementById('avg-input').textContent    =
    fmtTokens(data.turnCount ? Math.round((data.totalInput + data.totalCache) / data.turnCount) : 0);
  document.getElementById('ctx-growth').textContent  =
    data.ctxGrowth > 0 ? data.ctxGrowth.toFixed(1) + '×' : '—';
  document.getElementById('total-output').textContent = fmtTokens(data.totalOutput);

  // ---- Timing cards ----
  if (data.hasTimingData) {
    const totalSec = data.totalAPISec + data.totalToolSec + data.totalUserSec;
    document.getElementById('api-time').textContent  = fmtSec(data.totalAPISec)  + ' (' + pctOf(data.totalAPISec,  totalSec) + ')';
    document.getElementById('tool-time').textContent = fmtSec(data.totalToolSec) + ' (' + pctOf(data.totalToolSec, totalSec) + ')';
    document.getElementById('user-time').textContent = fmtSec(data.totalUserSec) + ' (' + pctOf(data.totalUserSec, totalSec) + ')';
    document.getElementById('avg-api').textContent   = fmtSec(data.avgAPISec);
  }

  // Color cache rate card based on value.
  const cacheCard = document.getElementById('cache-rate').closest('.card');
  if (data.cacheHitPct < 30) { cacheCard.className = 'card'; cacheCard.style.setProperty('--c', 'var(--red)'); cacheCard.style.borderColor = 'var(--red)'; }

  // ---- Token breakdown bar + legend ----
  const t = data.totalAll || 1;
  document.getElementById('bar-input').style.width   = pct(data.totalInput,  t);
  document.getElementById('bar-cache').style.width   = pct(data.totalCache,  t);
  document.getElementById('bar-create').style.width  = pct(data.totalCreate, t);
  document.getElementById('bar-output').style.width  = pct(data.totalOutput, t);

  document.getElementById('leg-input').textContent   = fmtTokens(data.totalInput);
  document.getElementById('leg-cache').textContent   = fmtTokens(data.totalCache);
  document.getElementById('leg-create').textContent  = fmtTokens(data.totalCreate);
  document.getElementById('leg-output').textContent  = fmtTokens(data.totalOutput);
  document.getElementById('leg-input-pct').textContent   = '(' + pct(data.totalInput,  t) + ')';
  document.getElementById('leg-cache-pct').textContent   = '(' + pct(data.totalCache,  t) + ')';
  document.getElementById('leg-create-pct').textContent  = '(' + pct(data.totalCreate, t) + ')';
  document.getElementById('leg-output-pct').textContent  = '(' + pct(data.totalOutput, t) + ')';

  // ---- Chart defaults ----
  Chart.defaults.color = '#5A6B8A';
  Chart.defaults.font.family = "'IBM Plex Mono', monospace";
  Chart.defaults.font.size = 11;

  const gridColor = '#1E2740';

  // ---- Timeline chart ----
  new Chart(document.getElementById('timelineChart'), {
    type: 'line',
    data: {
      labels: data.turnLabels,
      datasets: [
        {
          label: 'Cum. Input',
          data: data.cumInput,
          borderColor: '#4F8EF7',
          backgroundColor: 'rgba(79,142,247,0.08)',
          fill: true,
          tension: 0.3,
          borderWidth: 1.5,
          pointRadius: 0,
        },
        {
          label: 'Cum. Cache Read',
          data: data.cumCacheRead,
          borderColor: '#3DD68C',
          backgroundColor: 'rgba(61,214,140,0.06)',
          fill: true,
          tension: 0.3,
          borderWidth: 1.5,
          pointRadius: 0,
        },
        {
          label: 'Cum. Output',
          data: data.cumOutput,
          borderColor: '#F5A623',
          backgroundColor: 'rgba(245,166,35,0.06)',
          fill: true,
          tension: 0.3,
          borderWidth: 1.5,
          pointRadius: 0,
        },
      ]
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: {
          labels: { boxWidth: 10, padding: 16, color: '#5A6B8A' }
        },
        tooltip: {
          backgroundColor: '#0E1422',
          borderColor: '#1E2740',
          borderWidth: 1,
          callbacks: {
            label: ctx => ' ' + ctx.dataset.label + ': ' + fmtTokens(ctx.raw),
          }
        }
      },
      scales: {
        x: {
          grid: { color: gridColor },
          ticks: { maxTicksLimit: 10, maxRotation: 0 },
        },
        y: {
          grid: { color: gridColor },
          ticks: { callback: v => fmtTokens(v) }
        }
      }
    }
  });

  // ---- Tools chart ----
  if (data.toolNames && data.toolNames.length > 0) {
    const palette = ['#4F8EF7','#3DD68C','#9B7FEA','#F5A623','#2ECFD4','#F45B5B','#FFD166','#06D6A0'];
    new Chart(document.getElementById('toolsChart'), {
      type: 'bar',
      data: {
        labels: data.toolNames,
        datasets: [{
          label: 'Calls',
          data: data.toolCounts,
          backgroundColor: data.toolNames.map((_, i) => palette[i % palette.length] + '99'),
          borderColor:     data.toolNames.map((_, i) => palette[i % palette.length]),
          borderWidth: 1,
          borderRadius: 3,
        }]
      },
      options: {
        indexAxis: 'y',
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: false },
          tooltip: {
            backgroundColor: '#0E1422',
            borderColor: '#1E2740',
            borderWidth: 1,
          }
        },
        scales: {
          x: { grid: { color: gridColor }, ticks: { stepSize: 1 } },
          y: { grid: { display: false } }
        }
      }
    });
  } else {
    document.getElementById('toolsChart').closest('.chart-card').innerHTML =
      '<div class="chart-title">Tool Usage</div><div style="color:var(--muted);padding:40px;text-align:center;font-size:12px">No tool calls recorded</div>';
  }

  // ---- Gantt chart (session timeline) ----
  if (data.hasTimingData && document.getElementById('ganttChart')) {
    new Chart(document.getElementById('ganttChart'), {
      type: 'bar',
      data: {
        labels: data.turnLabels,
        datasets: [
          {
            label: 'Tool exec gap',
            data: data.ganttTool,
            backgroundColor: 'rgba(245,166,35,0.8)',
            borderColor: '#F5A623',
            borderWidth: 1,
            borderRadius: 3,
            borderSkipped: false,
          },
          {
            label: 'User idle gap',
            data: data.ganttUser,
            backgroundColor: 'rgba(90,107,138,0.55)',
            borderColor: '#5A6B8A',
            borderWidth: 1,
            borderRadius: 3,
            borderSkipped: false,
          },
          {
            label: 'API latency',
            data: data.ganttAPI,
            backgroundColor: 'rgba(79,142,247,0.85)',
            borderColor: '#4F8EF7',
            borderWidth: 1,
            borderRadius: 3,
            borderSkipped: false,
          },
        ]
      },
      options: {
        indexAxis: 'y',
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
          legend: { labels: { boxWidth: 10, padding: 16, color: '#5A6B8A' } },
          tooltip: {
            backgroundColor: '#0E1422',
            borderColor: '#1E2740',
            borderWidth: 1,
            callbacks: {
              label: ctx => {
                if (!ctx.raw || !Array.isArray(ctx.raw)) return null;
                const dur = ctx.raw[1] - ctx.raw[0];
                return ' ' + ctx.dataset.label + ': ' + fmtSec(dur) +
                  ' (' + fmtSec(ctx.raw[0]) + '–' + fmtSec(ctx.raw[1]) + ')';
              }
            }
          }
        },
        scales: {
          x: {
            grid: { color: gridColor },
            ticks: { callback: v => fmtSec(v) },
            title: { display: true, text: 'seconds from session start', color: '#5A6B8A', font: { size: 10 } },
          },
          y: { grid: { display: false } }
        }
      }
    });
  }

  // ---- Timing stacked bar chart ----
  if (data.hasTimingData && document.getElementById('timingChart')) {
    new Chart(document.getElementById('timingChart'), {
      type: 'bar',
      data: {
        labels: data.turnLabels,
        datasets: [
          {
            label: 'Tool exec gap',
            data: data.toolGap,
            backgroundColor: 'rgba(245,166,35,0.75)',
            borderColor: '#F5A623',
            borderWidth: 1,
            borderRadius: 2,
            stack: 'timing',
          },
          {
            label: 'User idle gap',
            data: data.userGap,
            backgroundColor: 'rgba(90,107,138,0.5)',
            borderColor: '#5A6B8A',
            borderWidth: 1,
            borderRadius: 2,
            stack: 'timing',
          },
          {
            label: 'API latency',
            data: data.apiLatency,
            backgroundColor: 'rgba(79,142,247,0.8)',
            borderColor: '#4F8EF7',
            borderWidth: 1,
            borderRadius: 2,
            stack: 'timing',
          },
        ]
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
          legend: { labels: { boxWidth: 10, padding: 16, color: '#5A6B8A' } },
          tooltip: {
            backgroundColor: '#0E1422',
            borderColor: '#1E2740',
            borderWidth: 1,
            callbacks: {
              label: ctx => ' ' + ctx.dataset.label + ': ' + fmtSec(ctx.raw),
            }
          }
        },
        scales: {
          x: { stacked: true, grid: { color: gridColor }, ticks: { maxTicksLimit: 15, maxRotation: 0 } },
          y: { stacked: true, grid: { color: gridColor }, ticks: { callback: v => fmtSec(v) } }
        }
      }
    });
  }
})();
</script>
</body>
</html>`
