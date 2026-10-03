---
name: loom
description: Coordinate local project tasks with the Loom CLI, including dependency readiness, claims, handoff notes, and completion evidence, when the project uses Loom or the user requests it.
---

# Loom

Use the available `loom` executable or the project's built `bin/loom`.
Keep work within the user's task boundaries; a Loom record does not authorize unrelated work or external actions.

## Select the shared database

Use the project's agreed database.
Resolution is `--db PATH`, then `LOOM_DB`, then the nearest ancestor's `.loom/loom.db`.
Across local worktrees, use the same absolute `--db` path or `LOOM_DB` value for every writer.
Initialize only when setting up a new project tracker is in scope: `loom init` creates the selected database, defaulting to `.loom/loom.db` in the current directory.
Ordinary commands do not initialize missing databases.
Keep the database on a local filesystem, outside NFS, synchronized folders, and Git tracking.

## Pick up and hand off work

1. Read `loom ready --json`, then `loom show ID --json` for the chosen task.
   `ready` contains only open tasks with no unfinished direct or transitive prerequisites.
2. Claim it with `loom claim ID --owner NAME` before making changes.
   Use a stable owner label for this writer and session.
   If the claim conflicts, inspect current state and select other authorized work; do not take over the claim.
3. Perform and verify the task, preserving other writers' changes.
4. Save a concise handoff with `loom note ID TEXT --owner NAME` when useful.
   Include completed work, verification, remaining issues, and artifact paths.
   A note replaces the current note; `loom events ID` retains earlier notes.
5. On completion, use `loom close ID --owner NAME --evidence REF`, then read back `loom show ID --json`.
   Use evidence that supports the result; Loom stores the reference without fetching or validating it.
   If work remains, leave a handoff and use `loom release ID --owner NAME --reason TEXT` when relinquishing ownership.

Replace `ID` with the returned task ID, `NAME` with the writer label, `TEXT` with a quoted note or reason, and `REF` with an evidence reference.
Claims do not expire and owner labels are not authentication or leases.
Before manually releasing an apparently abandoned claim, check that the recorded writer has stopped; use the recorded owner and explain the release.

## Maintain tasks and dependencies

Create work with `loom add TITLE --after ID --ref LINK`, omitting optional dependency and reference options as needed.
Capture returned IDs rather than inventing them; repeat `--after` or `--ref` for multiple values.
Use `loom dep add ID PREREQUISITE` or `loom dep remove ID PREREQUISITE`, adding `--owner NAME` for an in-progress task.
Dependencies must exist and cannot form cycles; done tasks must be reopened before dependency edits.
Use `loom reopen ID --reason TEXT` only when completed work needs reopening.
Reopening a prerequisite can block downstream readiness and closing, even through a done intermediate task.

Global `--db PATH` and `--json` work anywhere before the `--` sentinel.
JSON results are direct task objects, task arrays, or event arrays; `init` returns an object with a `database` field.
JSON errors appear on standard error as an `error` object with `code` and `message`, with a nonzero exit status.
Treat failed writes as failures, inspect current state before retrying, and distinguish recorded task status from actual verification evidence.
