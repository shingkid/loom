package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shingkid/loom/internal/store"
)

func invoke(t *testing.T, db string, args ...string) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	args = append([]string{"--json", "--db", db}, args...)
	if code := run(args, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("run(%q): exit=%d stderr=%s", args, code, &errOut)
	}
	return out.Bytes()
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return value
}

func wantError(t *testing.T, code int, out, errOut []byte, want string) {
	t.Helper()
	if code == 0 || len(out) != 0 {
		t.Fatalf("expected failure without stdout: exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	var envelope struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(errOut, &envelope); err != nil {
		t.Fatalf("stderr is not JSON: %q: %v", errOut, err)
	}
	if envelope.Error.Code != want || envelope.Error.Message == "" {
		t.Fatalf("error=%+v, want code %q and a message", envelope.Error, want)
	}
}

func reject(t *testing.T, db, want string, args ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"--json", "--db", db}, args...), &out, &errOut)
	wantError(t, code, out.Bytes(), errOut.Bytes(), want)
}

func newDB(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "loom.db")
	invoke(t, db, "init")
	return db
}

func TestCLILifecycle(t *testing.T) {
	db := newDB(t)
	add := func(args ...string) store.Task {
		return decode[store.Task](t, invoke(t, db, append([]string{"add"}, args...)...))
	}
	a := add("Prerequisite", "--ref", "issue:1", "--ref", "doc:2")
	b := add("Dependent", "--after", a.ID)
	if !reflect.DeepEqual(a.Refs, []string{"issue:1", "doc:2"}) || !reflect.DeepEqual(b.BlockedBy, []string{a.ID}) {
		t.Fatalf("references or dependency missing: a=%+v b=%+v", a, b)
	}
	ready := decode[[]store.Task](t, invoke(t, db, "ready"))
	if len(ready) != 1 || ready[0].ID != a.ID {
		t.Fatalf("ready=%+v", ready)
	}
	reject(t, db, "blocked", "claim", b.ID, "--owner", "Jane")
	invoke(t, db, "claim", a.ID, "--owner", "Jane")
	for _, args := range [][]string{
		{"note", a.ID, "stolen note", "--owner", "Other"},
		{"release", a.ID, "--owner", "Other", "--reason", "handoff"},
		{"close", a.ID, "--owner", "Other", "--evidence", "pr:1"},
		{"dep", "add", a.ID, b.ID, "--owner", "Other"},
	} {
		reject(t, db, "conflict", args...)
	}
	invoke(t, db, "note", a.ID, "first note", "--owner", "Jane")
	invoke(t, db, "note", a.ID, "handoff note", "--owner", "Jane")
	released := decode[store.Task](t, invoke(t, db, "release", a.ID, "--owner", "Jane", "--reason", "handoff"))
	if released.Status != "open" || released.Owner != "" || released.Note != "handoff note" {
		t.Fatalf("release=%+v", released)
	}
	invoke(t, db, "claim", a.ID, "--owner", "Next")
	closed := decode[store.Task](t, invoke(t, db, "close", a.ID, "--owner", "Next", "--evidence", "pr:42"))
	if closed.Status != "done" || closed.Owner != "" || closed.Evidence != "pr:42" {
		t.Fatalf("close=%+v", closed)
	}
	ready = decode[[]store.Task](t, invoke(t, db, "ready"))
	if len(ready) != 1 || ready[0].ID != b.ID {
		t.Fatalf("ready after completion=%+v", ready)
	}
	reopened := decode[store.Task](t, invoke(t, db, "reopen", a.ID, "--reason", "more work"))
	if reopened.Status != "open" || reopened.Evidence != "" {
		t.Fatalf("reopen=%+v", reopened)
	}
	reject(t, db, "blocked", "claim", b.ID, "--owner", "Next")
	unblocked := decode[store.Task](t, invoke(t, db, "dep", "remove", b.ID, a.ID))
	if len(unblocked.Dependencies) != 0 || len(unblocked.BlockedBy) != 0 {
		t.Fatalf("dependency removal=%+v", unblocked)
	}
	blocked := decode[store.Task](t, invoke(t, db, "dep", "add", b.ID, a.ID))
	if !reflect.DeepEqual(blocked.Dependencies, []string{a.ID}) || !reflect.DeepEqual(blocked.BlockedBy, []string{a.ID}) {
		t.Fatalf("dependency addition=%+v", blocked)
	}
	if tasks := decode[[]store.Task](t, invoke(t, db, "list")); len(tasks) != 2 {
		t.Fatalf("list=%+v", tasks)
	}
	events := decode[[]store.Event](t, invoke(t, db, "events", a.ID))
	var kinds []string
	for i, event := range events {
		kinds = append(kinds, event.Kind)
		if event.TaskID != a.ID || (i > 0 && event.Seq <= events[i-1].Seq) {
			t.Fatalf("invalid event sequence: %+v", events)
		}
	}
	want := []string{"added", "claimed", "noted", "noted", "released", "claimed", "completed", "reopened"}
	if !reflect.DeepEqual(kinds, want) || events[2].Data["note"] != "first note" || events[6].Data["evidence"] != "pr:42" {
		t.Fatalf("event history=%+v", events)
	}
	// A fresh OS process must observe committed state, including the handoff.
	cmd := helperCommand(t, "--json", "--db", db, "show", a.ID)
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("fresh process: %v", err)
	}
	fresh := decode[store.Task](t, data)
	if fresh.ID != a.ID || fresh.Status != "open" || fresh.Note != "handoff note" {
		t.Fatalf("fresh process read=%+v", fresh)
	}
}

func TestCLIInvalidArguments(t *testing.T) {
	db := newDB(t)
	for _, args := range [][]string{
		{"bogus"}, {"show"}, {"list", "extra"}, {"add", "unquoted", "title"},
		{"dep", "bogus", "a", "b"}, {"claim", "a"}, {"close", "a", "--owner", "Jane"},
		{"list", "--owner", "Jane"}, {"list", "--unknown"}, {"list", "-x"},
		{"list", "--db"}, {"list", "--db", "duplicate"}, {"add", "x", "--ref", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) { reject(t, db, "invalid", args...) })
	}
	// Even when an invalid option precedes --json, the error uses JSON.
	var out, errOut bytes.Buffer
	code := run([]string{"-x", "--json", "version"}, &out, &errOut)
	wantError(t, code, out.Bytes(), errOut.Bytes(), "invalid")
}

func TestCLIGlobalFlagsAndLiteralArguments(t *testing.T) {
	db := newDB(t)
	for _, tc := range []struct {
		args  []string
		title string
	}{
		{[]string{"--json", "--db", db, "add", "before"}, "before"},
		{[]string{"add", "after", "--db", db, "--json"}, "after"},
		{[]string{"--db=" + db, "add", "--json", "--", "--json"}, "--json"},
		{[]string{"--json", "--db", db, "add", "--", "-x"}, "-x"},
	} {
		args := tc.args
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 || errOut.Len() != 0 {
			t.Fatalf("run(%q): code=%d stderr=%s", args, code, &errOut)
		}
		task := decode[store.Task](t, out.Bytes())
		if task.Title != tc.title {
			t.Fatalf("literal title=%q for %q", task.Title, args)
		}
	}
}

func TestCLIMissingDatabaseDoesNotCreate(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing.db")
	reject(t, db, "not_found", "list")
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("missing database was created or stat failed: %v", err)
	}
}

func TestDatabasePathPrecedence(t *testing.T) {
	root := t.TempDir()
	near := filepath.Join(root, "nested")
	leaf := filepath.Join(near, "leaf")
	if err := os.MkdirAll(leaf, 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, near} {
		if err := os.MkdirAll(filepath.Join(dir, ".loom"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".loom", "loom.db"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(leaf)
	t.Setenv("LOOM_DB", "environment.db")
	check := func(explicit string, init bool, want string) {
		t.Helper()
		got, err := databasePath(explicit, init)
		if err != nil || got != want {
			t.Fatalf("databasePath(%q,%v)=%q,%v; want %q", explicit, init, got, err, want)
		}
	}
	check("explicit.db", false, filepath.Join(leaf, "explicit.db"))
	check("", false, filepath.Join(leaf, "environment.db"))
	check("", true, filepath.Join(leaf, "environment.db"))
	t.Setenv("LOOM_DB", "")
	check("", false, filepath.Join(near, ".loom", "loom.db"))
	check("", true, filepath.Join(leaf, ".loom", "loom.db"))
}

const helperEnv = "LOOM_CLI_TEST_HELPER"

func helperCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestCLIHelperProcess$", "--"}, args...)...)
	// Remove every inherited occurrence before appending our override.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != helperEnv && key != "LOOM_DB" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, helperEnv+"=1")
	return cmd
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			// Parent closes all pipes only after starting all contenders.
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(run(os.Args[i+1:], os.Stdout, os.Stderr))
		}
	}
	os.Exit(99)
}

func TestCLICompetingProcessesClaimOnce(t *testing.T) {
	db := newDB(t)
	task := decode[store.Task](t, invoke(t, db, "add", "Claim once"))
	type contender struct {
		cmd         *exec.Cmd
		gate        io.WriteCloser
		out, errOut bytes.Buffer
	}
	contenders := make([]contender, 8)
	for i := range contenders {
		c := &contenders[i]
		c.cmd = helperCommand(t, "--json", "--db", db, "claim", task.ID, "--owner", fmt.Sprintf("worker-%d", i))
		c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.errOut
		var err error
		c.gate, err = c.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.gate.Close(); _ = c.cmd.Process.Kill() })
	}
	for i := range contenders {
		if err := contenders[i].gate.Close(); err != nil {
			t.Fatal(err)
		}
	}
	successes, winner := 0, ""
	for i := range contenders {
		c := &contenders[i]
		err := c.cmd.Wait()
		if err == nil {
			successes++
			claimed := decode[store.Task](t, c.out.Bytes())
			winner = fmt.Sprintf("worker-%d", i)
			if claimed.ID != task.ID || claimed.Status != "in_progress" || claimed.Owner != winner || c.errOut.Len() != 0 {
				t.Fatalf("invalid successful claim: %+v stderr=%s", claimed, &c.errOut)
			}
		} else {
			if c.cmd.ProcessState == nil {
				t.Fatalf("process did not exit: %v", err)
			}
			wantError(t, c.cmd.ProcessState.ExitCode(), c.out.Bytes(), c.errOut.Bytes(), "conflict")
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims=%d, want exactly one", successes)
	}
	persisted := decode[store.Task](t, invoke(t, db, "show", task.ID))
	if persisted.Owner != winner || persisted.Status != "in_progress" {
		t.Fatalf("persisted=%+v winner=%s", persisted, winner)
	}
	events := decode[[]store.Event](t, invoke(t, db, "events", task.ID))
	if len(events) != 2 || events[0].Kind != "added" || events[1].Kind != "claimed" || events[1].Actor != winner {
		t.Fatalf("claim history=%+v", events)
	}
}
