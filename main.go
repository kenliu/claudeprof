package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea/v2"
	"github.com/urfave/cli/v3"

	"claudeprof/tui"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claudeprof: cannot determine home directory:", err)
		os.Exit(1)
	}

	app := &cli.Command{
		Name:      "claudeprof",
		Usage:     "Profile Claude Code sessions",
		UsageText: "claudeprof [flags] [session.jsonl]",
		Description: "Profile Claude Code sessions. Without a file argument, opens the session browser.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "claude-dir",
				Value: filepath.Join(home, ".claude"),
				Usage: "path to Claude data directory",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			claudeDir := cmd.String("claude-dir")
			sessionFile := ""
			if cmd.NArg() > 0 {
				sessionFile = cmd.Args().First()
			}

			m, err := tui.New(claudeDir, sessionFile)
			if err != nil {
				return err
			}

			p := tea.NewProgram(m, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "claudeprof:", err)
		os.Exit(1)
	}
}
