package compliance

import (
	"fmt"
	"strings"
	"time"
)

// RenderMarkdown returns a human-readable rendering of the report derived
// entirely from its fields.
func (r Report) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Compliance Report — run %s\n\n", r.Run.ID)
	fmt.Fprintf(&b, "_Schema %s · generated %s_\n\n", r.SchemaVersion, r.GeneratedAt.Format(time.RFC3339))

	b.WriteString("## Run\n\n")
	fmt.Fprintf(&b, "- **ID:** %s\n", r.Run.ID)
	if r.Run.Agent != "" {
		fmt.Fprintf(&b, "- **Agent:** %s\n", r.Run.Agent)
	}
	if r.Run.Status != "" {
		fmt.Fprintf(&b, "- **Status:** %s (exit %d)\n", r.Run.Status, r.Run.ExitCode)
	}
	if !r.Run.StartedAt.IsZero() {
		fmt.Fprintf(&b, "- **Started:** %s\n", r.Run.StartedAt.Format(time.RFC3339))
	}
	if !r.Run.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "- **Finished:** %s\n", r.Run.FinishedAt.Format(time.RFC3339))
	}
	if r.TraceID != "" {
		fmt.Fprintf(&b, "- **Trace:** `%s`\n", r.TraceID)
	}
	if r.Run.Prompt != "" {
		b.WriteString("\n**Prompt:**\n\n```\n")
		b.WriteString(r.Run.Prompt)
		b.WriteString("\n```\n")
	}

	b.WriteString("\n## Decisions\n\n")
	if len(r.Decisions) == 0 {
		b.WriteString("_No governance decisions recorded._\n")
	} else {
		b.WriteString("| Time | Result | Action | Policy | Approver | Reasons |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, d := range r.Decisions {
			reasons := mdCell(strings.Join(d.Reasons, "; "))
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
				d.At.Format(time.RFC3339), mdCell(d.Result), mdCell(d.ActionType),
				mdCell(d.PolicyName), mdCell(d.Approver), reasons)
		}
	}

	b.WriteString("\n## Summary\n\n")
	fmt.Fprintf(&b, "- **Total decisions:** %d\n", r.Summary.Total)
	for _, k := range []string{"allow", "deny", "review", "approved"} {
		if n, ok := r.Summary.ByResult[k]; ok {
			fmt.Fprintf(&b, "- **%s:** %d\n", k, n)
		}
	}
	fmt.Fprintf(&b, "- **Policies fired:** %d\n", r.Summary.PoliciesFired)

	b.WriteString("\n## Integrity\n\n")
	fmt.Fprintf(&b, "- **%s:** `%s`\n", r.Integrity.Algorithm, r.Integrity.Digest)
	return b.String()
}

// mdCell makes a string safe to drop into a Markdown table cell: collapse
// newlines and escape pipes so cell content can't inject extra columns.
func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "\\|")
}
