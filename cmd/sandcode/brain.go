package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/spf13/cobra"
)

func newBrainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "brain",
		Short: "Manage the cognitive learning brain",
		Long:  "View statistics, list lessons, and prune the brain's knowledge store.",
	}
	cmd.AddCommand(newBrainStatsCmd())
	cmd.AddCommand(newBrainLessonsCmd())
	cmd.AddCommand(newBrainPruneCmd())
	return cmd
}

func newBrainStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show brain learning statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := openBrain()
			if err != nil {
				return err
			}
			defer b.Close()

			stats, err := b.Stats(cmd.Context())
			if err != nil {
				return err
			}

			fmt.Printf("\033[1;36m[sandcode brain]\033[0m statistics\n")
			fmt.Printf("  Total lessons:  %d\n", stats.TotalLessons)
			fmt.Printf("  Skills:         %d\n", stats.Skills)
			fmt.Printf("  Anti-patterns:  %d\n", stats.AntiPatterns)
			fmt.Printf("  Preferences:    %d\n", stats.Preferences)
			fmt.Printf("  Principles:     %d\n", stats.Principles)
			fmt.Printf("  Avg confidence: %.2f\n", stats.AvgConfidence)
			if !stats.OldestLesson.IsZero() {
				fmt.Printf("  Oldest lesson:  %s\n", stats.OldestLesson.Format(time.RFC3339))
			}
			if !stats.NewestLesson.IsZero() {
				fmt.Printf("  Newest lesson:  %s\n", stats.NewestLesson.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newBrainLessonsCmd() *cobra.Command {
	var (
		category string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "lessons",
		Short: "List learned lessons",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := openBrain()
			if err != nil {
				return err
			}
			defer b.Close()

			lessons, err := b.ListLessons(cmd.Context(), brain.Category(category), limit)
			if err != nil {
				return err
			}

			if len(lessons) == 0 {
				fmt.Println("No lessons yet. Use --learn with `sandcode run` to start learning.")
				return nil
			}

			fmt.Printf("\033[1;36m[sandcode brain]\033[0m %d lessons\n\n", len(lessons))
			for _, l := range lessons {
				icon := "✅"
				switch l.Category {
				case brain.CategoryAntiPattern:
					icon = "❌"
				case brain.CategoryPreference:
					icon = "⚙️"
				case brain.CategoryPrinciple:
					icon = "📐"
				}
				fmt.Printf("%s [%s] %s\n", icon, l.Category, l.Content)
				fmt.Printf("   confidence=%.2f  used=%d  run=%s  created=%s\n",
					l.Confidence, l.UsedCount, l.RunID, l.CreatedAt.Format("2006-01-02 15:04"))
				if l.Evidence != "" {
					ev := l.Evidence
					if len(ev) > 100 {
						ev = ev[:100] + "…"
					}
					fmt.Printf("   evidence: %s\n", ev)
				}
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&category, "category", "", "filter by category: skill|antipattern|preference|principle")
	cmd.Flags().IntVar(&limit, "limit", 20, "max lessons to show")
	return cmd
}

func newBrainPruneCmd() *cobra.Command {
	var (
		maxAge        string
		minConfidence float64
		dryRun        bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove old low-confidence lessons",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := openBrain()
			if err != nil {
				return err
			}
			defer b.Close()

			dur, err := time.ParseDuration(maxAge)
			if err != nil {
				return fmt.Errorf("invalid --max-age: %w", err)
			}

			if dryRun {
				lessons, err := b.ListLessons(cmd.Context(), "", 1000)
				if err != nil {
					return err
				}
				cutoff := time.Now().Add(-dur)
				count := 0
				for _, l := range lessons {
					if l.CreatedAt.Before(cutoff) && l.Confidence < minConfidence {
						count++
					}
				}
				fmt.Printf("Would prune %d lessons (dry-run)\n", count)
				return nil
			}

			n, err := b.Prune(cmd.Context(), dur, minConfidence)
			if err != nil {
				return err
			}
			fmt.Printf("\033[1;36m[sandcode brain]\033[0m pruned %d lessons\n", n)
			return nil
		},
	}
	cmd.Flags().StringVar(&maxAge, "max-age", "720h", "prune lessons older than this (e.g. 720h = 30 days)")
	cmd.Flags().Float64Var(&minConfidence, "min-confidence", 0.3, "prune lessons below this confidence")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be pruned without deleting")
	return cmd
}

// openBrain opens the brain database at the standard location.
func openBrain() (*brain.SQLiteBrain, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cwd, ".sandcode", "brain.db")
	return brain.OpenBrain(path)
}

// resolveBrainPath returns the brain DB path for a given CWD.
func resolveBrainPath(cwd string) string {
	return filepath.Join(cwd, ".sandcode", "brain.db")
}
