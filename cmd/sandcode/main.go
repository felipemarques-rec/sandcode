// sandcode CLI entrypoint.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	root := newRootCmd()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "sandcode",
		Short:         "Orchestrate AI coding agents in isolated sandboxes",
		Long:          "sandcode runs Claude Code, Codex, Cursor (and others) inside Docker/Podman sandboxes with git-worktree isolation.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newInitCmd())
	cmd.AddCommand(newRunCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newShowCmd())
	cmd.AddCommand(newComplianceCmd())
	cmd.AddCommand(newLogsCmd())
	cmd.AddCommand(newWorktreeCmd())
	cmd.AddCommand(newBrainCmd())
	cmd.AddCommand(newServeCmd())
	return cmd
}
