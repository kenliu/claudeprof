## claudeprof — Claude Code Session Profiler

`claudeprof` is a Go CLI tool that parses and analyzes Claude Code session files (`.jsonl`) stored in `~/.claude/projects/`. It provides a terminal UI for interactive exploration and generates standalone HTML reports.

### What it does

- **Parses** Claude Code JSONL session files into structured turn-by-turn data including token usage (input, output, cache creation, cache read), tool calls, and timing
- **Analyzes** sessions for token efficiency: cache hit rate, context growth rate, tool call patterns, per-turn averages, and cumulative consumption
- **Generates optimization suggestions** (high/medium/low severity) across categories: Cache, Context, Tools — e.g. low cache hit rate, runaway context growth, overuse of bash
- **TUI** (Bubble Tea v2): seven-tab interactive profile view — Overview, Timeline, Timing (with flame chart), Tokens, Tools, Suggestions, AI Analysis — with a session browser for picking from all discovered sessions
- **HTML report**: standalone report with Chart.js charts (cumulative token consumption, tool usage), turn detail table, and suggestions; opens in browser automatically

### Build

```bash
cd claudeprof
go mod tidy
go build -o claudeprof .
```

### Usage

```bash
claudeprof                                        # open session browser
claudeprof path/to/session.jsonl                  # open a specific session directly
claudeprof --claude-dir /path/to/.claude          # non-default Claude data directory
```

Inside the TUI, press `r` to generate and open an HTML report, `a` for AI analysis, and `f` on the Timing tab to toggle the flame chart view.

### Key packages

| Package | Responsibility |
|---|---|
| `session` | JSONL parsing, session discovery, turn extraction |
| `analyzer` | Metrics computation, suggestion engine |
| `tui` | Bubble Tea v2 interactive UI |
| `report` | HTML report generation and browser launch |

### Session file location

Claude Code stores sessions at `~/.claude/projects/<encoded-path>/<session-id>.jsonl`. `claudeprof` discovers all sessions by scanning this directory tree and decodes the project directory names back to readable paths.
