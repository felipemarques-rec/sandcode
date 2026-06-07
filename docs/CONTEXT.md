# Project Context — Sandcode

<!-- This file is used by sandcode's Brain (Grill with Docs) to understand
     your project's domain before enriching agent prompts. Keep it updated
     with your project's terminology, architecture decisions, and conventions. -->

## Domain Language

<!-- Define key terms used in your project. Example:
- **Run**: A single agent execution in a sandbox
- **Lesson**: A unit of learned knowledge extracted from a run outcome
-->

## Architecture Decisions

<!-- Summarize key architectural choices. Example:
- SQLite for persistence (zero-ops, embedded, pure Go)
- Docker sandboxing for agent isolation
- Git worktrees for concurrent branch management
-->

## Conventions

<!-- Code style, naming conventions, patterns to follow. Example:
- All interfaces are defined in the consumer package
- Errors are wrapped with context: fmt.Errorf("operation: %w", err)
- Configuration via environment variables (12-Factor)
-->

## Anti-Patterns (do NOT do these)

<!-- Patterns to explicitly avoid. Example:
- Do NOT silence errors with _ =
- Do NOT hardcode configuration values
- Do NOT use fmt.Printf for logging (use slog)
-->
