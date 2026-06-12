package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/compliance"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/spf13/cobra"
)

func newComplianceCmd() *cobra.Command {
	var cwd string
	var format string
	cmd := &cobra.Command{
		Use:   "compliance <run-id>",
		Short: "Export a per-run compliance & explainability report (JSON or Markdown)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "json" && format != "md" {
				return fmt.Errorf("--format: must be one of json|md, got %q", format)
			}
			db, err := store.Open(resolveStorePath(cwd))
			if err != nil {
				return err
			}
			defer db.Close()
			run, err := db.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			al, err := governance.OpenAuditLog(resolveAuditPath(cwd))
			if err != nil {
				return err
			}
			defer al.Close()
			rows, err := al.ListByRun(cmd.Context(), run.ID)
			if err != nil {
				return err
			}
			rep := compliance.Build(compliance.ReportInput{
				Run: compliance.RunIdentity{
					ID:         run.ID,
					Agent:      run.Agent,
					Prompt:     run.Prompt,
					Status:     string(run.Status),
					StartedAt:  run.StartedAt,
					FinishedAt: run.FinishedAt,
					ExitCode:   run.ExitCode,
				},
				AuditRows: rows,
				Now:       time.Now(),
			})
			if format == "md" {
				fmt.Fprintln(cmd.OutOrStdout(), rep.RenderMarkdown())
				return nil
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			return enc.Encode(rep)
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "project directory (default: current)")
	cmd.Flags().StringVar(&format, "format", "json", "output format: json|md")
	return cmd
}
