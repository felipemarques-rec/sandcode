package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var limit int
	var status string
	var agent string
	var includeChildren bool
	var cwd string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show recent runs from the local store",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := resolveStorePath(cwd)
			db, err := store.Open(path)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer db.Close()

			f := store.ListFilter{Limit: limit, Agent: agent}
			if status != "" {
				f.Status = store.RunStatus(status)
			}
			if includeChildren {
				f.ParentID = "*"
			}
			runs, err := db.ListRuns(cmd.Context(), f)
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				fmt.Println("(no runs)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAGENT\tSTATUS\tEXIT\tDURATION\tWHEN\tPROMPT")
			now := time.Now()
			for _, r := range runs {
				dur := "—"
				if !r.FinishedAt.IsZero() {
					dur = r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond).String()
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
					r.ID, r.Agent, r.Status, r.ExitCode, dur,
					formatAgo(now.Sub(r.StartedAt)), truncate(r.Prompt, 60),
				)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "max rows to show")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (running|success|failure|cancelled)")
	cmd.Flags().StringVar(&agent, "agent", "", "filter by agent name")
	cmd.Flags().BoolVar(&includeChildren, "all", false, "include sub-runs of parallel parents")
	cmd.Flags().StringVar(&cwd, "cwd", "", "project directory (default: current)")
	return cmd
}

func formatAgo(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
