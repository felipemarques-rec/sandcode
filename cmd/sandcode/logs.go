package main

import (
	"context"
	"errors"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var cwd string
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Stream events of a run from the local store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.Open(resolveStorePath(cwd))
			if err != nil {
				return err
			}
			defer db.Close()

			runID := args[0]
			seen := int64(-1)
			ctx := cmd.Context()
			for {
				events, err := db.ListEvents(ctx, runID)
				if err != nil {
					return err
				}
				for _, e := range events {
					if e.Seq <= seen {
						continue
					}
					renderStoredEvent(e)
					seen = e.Seq
				}
				if !follow {
					return nil
				}

				// Stop tailing once the run reaches a terminal state.
				run, err := db.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				if run.Status != store.StatusRunning && run.Status != store.StatusPending {
					return nil
				}

				select {
				case <-ctx.Done():
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					return ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "project directory (default: current)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "tail new events until the run completes")
	return cmd
}
