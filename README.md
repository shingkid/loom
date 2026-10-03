# Loom

Loom is a local Go CLI for tracking work across coding sessions and Git worktrees.
It stores tasks, dependencies, ownership, handoff notes, and completion evidence in SQLite.
It is inspired by [Beads](https://github.com/gastownhall/beads) and built from scratch.

## Build and test

From this directory, use Go 1.24 or later to build the binary and run the tests:

```sh
go build -o bin/loom ./cmd/loom
go test -race ./...
```

Run `./bin/loom` directly, or put the binary in a directory on your `PATH`.
The build does not install global hooks or configure an agent.

## Try a complete workflow

This shell example requires `jq` to capture generated task IDs.
Run it from this directory after building.
It creates an isolated temporary database and a real example note, then leaves both available for inspection.

```sh
(
set -eu
LOOM_BIN="$PWD/bin/loom"
LOOM_DEMO=$(mktemp -d)
export LOOM_DB="$LOOM_DEMO/loom.db"
"$LOOM_BIN" init
WRITE_ID=$("$LOOM_BIN" --json add "Write example note" | jq -er '.id')
REVIEW_ID=$("$LOOM_BIN" --json add "Review example note" --after "$WRITE_ID" | jq -er '.id')
"$LOOM_BIN" ready
"$LOOM_BIN" claim "$WRITE_ID" --owner example-writer
printf 'Example note for review.\n' > "$LOOM_DEMO/note.txt"
"$LOOM_BIN" note "$WRITE_ID" "Note written; ready for review" --owner example-writer
"$LOOM_BIN" close "$WRITE_ID" --owner example-writer --evidence "$LOOM_DEMO/note.txt"
"$LOOM_BIN" ready
"$LOOM_BIN" claim "$REVIEW_ID" --owner example-reviewer
cat "$LOOM_DEMO/note.txt"
"$LOOM_BIN" close "$REVIEW_ID" --owner example-reviewer --evidence "$LOOM_DEMO/note.txt"
"$LOOM_BIN" show "$REVIEW_ID"
"$LOOM_BIN" events "$WRITE_ID"
printf 'Example artifacts: %s\n' "$LOOM_DEMO"
)
```

The first `ready` lists only the writing task.
After that task closes, the review task becomes ready.
Both tasks end with status `done`.
The example demonstrates lifecycle transitions; production completion evidence should substantiate the actual work and verification.

## Commands

In this syntax reference, uppercase values are placeholders, square brackets mark optional arguments, and `...` marks repetition.
Quote titles, notes, and reasons that contain spaces.

```text
loom init
loom add TITLE [--after ID ...] [--ref LINK ...]
loom list
loom ready
loom show ID
loom claim ID --owner NAME
loom release ID --owner NAME --reason TEXT
loom note ID TEXT [--owner NAME]
loom close ID --owner NAME --evidence REF
loom reopen ID --reason TEXT
loom dep add ID PREREQUISITE [--owner NAME]
loom dep remove ID PREREQUISITE [--owner NAME]
loom events ID
loom version
```

`ID` and `PREREQUISITE` are generated task IDs; `NAME` identifies a local writer.
`TITLE` names work, `TEXT` supplies a note or reason, and `LINK` and `REF` identify supporting material.
Repeat `--after` for multiple prerequisites, or use comma-separated IDs.
Repeat `--ref` for multiple references.
References and evidence are opaque strings; Loom does not fetch them or validate evidence URLs.

Global `--db PATH` and `--json` options can appear before or after the command, before the `--` sentinel.
Use `--` to stop option parsing when a positional value starts with a hyphen.
In JSON mode, task commands return a task object, `list` and `ready` return task arrays, and `events` returns an event array directly.
`init` returns `{"database":"PATH"}`.
Errors go to standard error as `{"error":{"code":"CODE","message":"MESSAGE"}}` in JSON mode and return a nonzero exit status.

## Lifecycle and ownership

Tasks have status `open`, `in_progress`, or `done`.
Blocking is computed from all unfinished prerequisites, including transitive prerequisites.
`ready` includes only open tasks with no unfinished prerequisites.
`claim` atomically changes a ready open task to `in_progress`; competing or repeated claims fail.

`close` requires the matching owner, completion evidence, and no unfinished prerequisites, then clears ownership.
`release` requires the matching owner and a reason, then returns the task to `open`.
`reopen` requires a reason and returns a done task to `open`, retaining previous completion evidence in its events.
Reopening a prerequisite can block downstream tasks even through an intermediate done task.

Notes and dependency edits on an in-progress task require its matching owner.
`note` replaces the current handoff note; earlier notes remain in the event history.
Dependencies must exist and cannot form cycles.
Reopen a done task before changing its dependencies.
Task changes and their audit events commit together; there is no delete command.

Owners are coordination labels, not authenticated identities or leases.
Claims never expire automatically, and Loom does not take over stale claims.
Before manually releasing an abandoned claim, check that its writer has stopped, then use the recorded owner and a concrete reason.

## Database location and sharing

For ordinary commands, resolution is `--db PATH`, then `LOOM_DB`, then the nearest ancestor's `.loom/loom.db`.
Missing databases are errors; commands never initialize one implicitly.
`init` uses `--db` or `LOOM_DB` when supplied, otherwise creates `.loom/loom.db` in the current directory.
It refuses to overwrite an existing database.

Across local worktrees, point every writer at the same absolute database path with `LOOM_DB` or `--db`.
For example, after `./bin/loom init` in the shared project directory, `export LOOM_DB="$PWD/.loom/loom.db"` lets child shells keep using that database after changing directories.
Separate terminal sessions must set the same value.

Keep the database on a local filesystem, outside NFS and synchronized folders.
Loom uses SQLite WAL mode and a busy timeout to coordinate local processes.
Do not synchronize the database through Git; exclude `.loom/` from version control.
For a live backup, use SQLite's `.backup` command rather than copying only the database file, which can omit uncheckpointed WAL changes.

## Optional agent skill

The packaged [Loom skill](skills/loom/SKILL.md) describes how an agent can claim work, leave a handoff, and verify completion.
Install that folder through your agent's skill mechanism only when you want the integration.
The repository does not install it globally or modify agent configuration.
