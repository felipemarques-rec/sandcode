package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	var cwd string
	var tail int
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Display details of a single run with its events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.Open(resolveStorePath(cwd))
			if err != nil {
				return err
			}
			defer db.Close()

			run, err := db.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Run ID:    %s\n", run.ID)
			if run.ParentID != "" {
				fmt.Printf("Parent:    %s\n", run.ParentID)
			}
			fmt.Printf("Agent:     %s\n", run.Agent)
			fmt.Printf("Sandbox:   %s\n", run.Sandbox)
			fmt.Printf("Status:    %s (exit %d)\n", run.Status, run.ExitCode)
			fmt.Printf("Started:   %s\n", run.StartedAt.Format(time.RFC3339))
			if !run.FinishedAt.IsZero() {
				fmt.Printf("Finished:  %s (%s)\n", run.FinishedAt.Format(time.RFC3339),
					run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond))
			}
			fmt.Printf("Strategy:  %s\n", run.Strategy)
			fmt.Printf("CWD:       %s\n", run.CWD)
			fmt.Println("Prompt:")
			fmt.Println("  " + strings.ReplaceAll(run.Prompt, "\n", "\n  "))
			fmt.Println()

			// If this is a parallel parent, surface its children + ranking.
			if run.Agent == "parallel" {
				children, err := db.ListRuns(cmd.Context(), store.ListFilter{ParentID: run.ID})
				if err == nil && len(children) > 0 {
					fmt.Printf("Sub-runs (%d):\n", len(children))
					for _, c := range children {
						dur := "—"
						if !c.FinishedAt.IsZero() {
							dur = c.FinishedAt.Sub(c.StartedAt).Round(time.Millisecond).String()
						}
						fmt.Printf("  • %s  %-12s %s exit=%d %s\n", c.ID, c.Agent, c.Status, c.ExitCode, dur)
					}
					fmt.Println()
				}
				if rk, err := db.GetRanking(cmd.Context(), run.ID); err == nil {
					fmt.Printf("Ranking by %s:\n", rk.Judge)
					for id, s := range rk.Scores {
						marker := "  "
						if id == rk.WinnerRunID {
							marker = "★ "
						}
						fmt.Printf("  %s%s  score=%.2f\n", marker, id, s)
					}
					if rk.Rationale != "" {
						fmt.Printf("  rationale: %s\n", rk.Rationale)
					}
					fmt.Println()
				}
			}

			events, err := db.ListEvents(cmd.Context(), run.ID)
			if err != nil {
				return err
			}
			if tail > 0 && len(events) > tail {
				events = events[len(events)-tail:]
				fmt.Printf("Events (last %d of %d):\n", tail, len(events))
			} else {
				fmt.Printf("Events (%d):\n", len(events))
			}
			for _, e := range events {
				renderStoredEvent(e)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "project directory (default: current)")
	cmd.Flags().IntVarP(&tail, "tail", "t", 0, "show only the last N events (0 = all)")
	return cmd
}

func renderStoredEvent(e store.Event) {
	var ev agent.StreamEvent
	if err := json.Unmarshal([]byte(e.Payload), &ev); err == nil {
		stamp := e.Timestamp.Format("15:04:05.000")
		switch ev.Kind {
		case agent.EventText:
			fmt.Printf("[%s] %s\n", stamp, ev.Text)
		case agent.EventToolCall:
			fmt.Printf("[%s] \033[33m▶ %s\033[0m %s\n", stamp, ev.ToolName, truncate(ev.ToolInput, 200))
		case agent.EventWarning:
			fmt.Printf("[%s] \033[31m! %s\033[0m\n", stamp, ev.Text)
		case agent.EventSession:
			fmt.Printf("[%s] \033[2msession=%s\033[0m\n", stamp, ev.SessionID)
		default:
			fmt.Printf("[%s] %s\n", stamp, ev.Text)
		}
		return
	}
	fmt.Printf("[%s] %s\n", e.Timestamp.Format("15:04:05.000"), e.Payload)
}
