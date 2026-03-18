# claudeprof

A profiler for [Claude Code](https://claude.ai/code) sessions. Parses the JSONL session files Claude Code writes to `~/.claude/projects/` and gives you a detailed breakdown of where tokens and time went — so you can optimize your skills and workflows.

## Features

- **Token analysis** — per-turn input/output/cache breakdown, cumulative growth, cache hit rate
- **Time profiling** — API latency vs tool execution time vs user idle time, per turn and in aggregate
- **Flame chart** — Gantt-style timeline view with wall-clock time on the X axis
- **HTML report** — standalone report with Chart.js charts (session timeline, token consumption, tool usage, Gantt); opens in your browser automatically
- **Optimization suggestions** — ranked recommendations for cache efficiency, context growth, tool density
- **AI analysis** — invokes `claude -p` with session data for freeform diagnosis
- **Session browser** — lists all discovered sessions with project path, date, and size

## Install

```bash
git clone https://github.com/kenliu/claudeprof
cd claudeprof
go build -o claudeprof .
```

Requires Go 1.22+.

## Usage

```bash
claudeprof                                   # open session browser
claudeprof path/to/session.jsonl             # open a specific session directly
claudeprof --claude-dir /path/to/.claude     # non-default Claude data directory
```

## TUI keyboard shortcuts

### Session browser
| Key | Action |
|-----|--------|
| `↑` / `↓` / `j` / `k` | Navigate sessions |
| `enter` | Open session |
| `q` | Quit |

### Profile view
| Key | Action |
|-----|--------|
| `tab` / `shift+tab` | Cycle tabs |
| `1`–`7` | Jump to tab directly |
| `↑` / `↓` / `j` / `k` | Scroll content |
| `g` / `G` | Top / bottom |
| `f` | Toggle flame chart (Timing tab only) |
| `r` | Generate and open HTML report |
| `a` | Run AI analysis via `claude -p` |
| `esc` / `backspace` | Back to session browser |
| `q` | Quit |

### Tabs
| # | Tab | Description |
|---|-----|-------------|
| 1 | Overview | Aggregate token breakdown, cache efficiency, top tools, suggestion summary |
| 2 | Timeline | Turn-by-turn table: tokens, cache %, tools used, prompt preview |
| 3 | Timing | Per-turn timing bars (API latency vs gap); press `f` for flame chart |
| 4 | Tokens | Cumulative token consumption chart, per-turn input breakdown |
| 5 | Tools | Tool usage frequency chart |
| 6 | Suggestions | Ranked optimization recommendations |
| 7 | AI Analysis | Freeform analysis from `claude -p` |

## Timing analysis

The **Timing** tab breaks each turn's wall-clock time into three categories:

- **API latency** — time waiting for Claude's response (request sent → response received)
- **Tool execution** — inter-turn gap when the previous turn ended with tool calls
- **User idle** — inter-turn gap when waiting for human input

Press `f` on the Timing tab to switch to the **flame chart** — a Gantt-style view with wall-clock time on the X axis and one row per turn, making it easy to spot slow turns at a glance.

The HTML report includes both views: a session timeline Gantt chart and a per-turn stacked duration chart.

## HTML report

Press `r` in the profile view to generate a standalone HTML report and open it in your browser. The report includes:

- Session metadata and metric cards (tokens, cache rate, timing breakdown)
- **Session timeline Gantt chart** — horizontal floating bars showing absolute wall-clock positions for API calls and inter-turn gaps
- Cumulative token consumption line chart
- Tool usage bar chart
- Per-turn stacked timing chart
- Turn detail table with timing columns (gap type, API latency, cycle time)
- Optimization suggestions

## Package structure

| Package | Responsibility |
|---------|----------------|
| `session` | JSONL parsing, session discovery, turn extraction |
| `analyzer` | Metrics computation, timing analysis, suggestion engine |
| `tui` | Bubble Tea v2 interactive terminal UI |
| `report` | HTML report generation and browser launch |

## Session file location

Claude Code stores sessions at `~/.claude/projects/<encoded-path>/<session-id>.jsonl`. `claudeprof` discovers all sessions by scanning this directory tree.
