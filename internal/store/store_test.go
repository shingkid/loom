package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func database(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".loom", "loom.db")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path
}
func add(t *testing.T, s *Store, title string, after ...string) Task {
	t.Helper()
	task, err := s.Add(title, after, nil)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("error = %v; want code %s", err, want)
	}
}
func finish(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(id, "alice", "test://passed"); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationAndPersistence(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.db")
	_, err := Open(absent)
	code(t, err, "not_found")
	if _, err = os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created absent database: %v", err)
	}
	s, path := database(t)
	task, err := s.Add("persist", nil, []string{"issue://1"})
	if err != nil {
		t.Fatal(err)
	}
	code(t, Init(path), "conflict")
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, task) {
		t.Fatalf("readback = %#v, want %#v", got, task)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	if task.Refs == nil || task.Dependencies == nil || task.BlockedBy == nil {
		t.Fatal("slices must be non-null")
	}
	empty, err := s.Ready()
	if err != nil || len(empty) != 1 {
		t.Fatalf("ready: %v %v", empty, err)
	}
	if _, err = s.Get("missing"); err == nil {
		t.Fatal("missing task succeeded")
	} else {
		code(t, err, "not_found")
	}
	if _, err = s.Events("missing"); err == nil {
		t.Fatal("missing events succeeded")
	} else {
		code(t, err, "not_found")
	}
	if _, err = s.db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	code(t, err, "storage")
}
func TestInvalidDatabaseAndInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opened invalid database")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "not sqlite" {
		t.Fatal("invalid database changed")
	}
	s, _ := database(t)
	_, err = s.Add(" ", nil, nil)
	code(t, err, "invalid")
	_, err = s.Add("missing dependency", []string{"missing"}, nil)
	code(t, err, "not_found")
	tasks, err := s.List()
	if err != nil || len(tasks) != 0 || tasks == nil {
		t.Fatalf("invalid adds persisted: %v %v", tasks, err)
	}
	task := add(t, s, "task")
	_, err = s.Claim(task.ID, "")
	code(t, err, "invalid")
	_, err = s.Note(task.ID, "", " ")
	code(t, err, "invalid")
	_, err = s.Reopen(task.ID, "why")
	code(t, err, "conflict")
	_, err = s.Release(task.ID, "alice", "reason")
	code(t, err, "conflict")
	_, err = s.Complete(task.ID, "alice", "evidence")
	code(t, err, "conflict")
}
func TestLifecycleOwnershipAndAudit(t *testing.T) {
	s, _ := database(t)
	task := add(t, s, "lifecycle")
	if _, err := s.Note(task.ID, "", "first"); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(task.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != "in_progress" || claimed.Owner != "alice" {
		t.Fatalf("claim: %#v", claimed)
	}
	_, err = s.Claim(task.ID, "bob")
	code(t, err, "conflict")
	_, err = s.Note(task.ID, "bob", "bad")
	code(t, err, "conflict")
	_, err = s.Release(task.ID, "bob", "bad")
	code(t, err, "conflict")
	_, err = s.Complete(task.ID, "bob", "bad")
	code(t, err, "conflict")
	_, err = s.Release(task.ID, "alice", "")
	code(t, err, "invalid")
	_, err = s.Complete(task.ID, "alice", "")
	code(t, err, "invalid")
	released, err := s.Release(task.ID, "alice", "handoff")
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "open" || released.Owner != "" {
		t.Fatalf("release: %#v", released)
	}
	finish(t, s, task.ID)
	done, err := s.Note(task.ID, "", "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "done" || done.Evidence != "test://passed" {
		t.Fatalf("done: %#v", done)
	}
	_, err = s.Reopen(task.ID, "")
	code(t, err, "invalid")
	opened, err := s.Reopen(task.ID, "more work")
	if err != nil {
		t.Fatal(err)
	}
	if opened.Status != "open" || opened.Owner != "" || opened.Evidence != "" || opened.Note != "reviewed" {
		t.Fatalf("reopen: %#v", opened)
	}
	events, err := s.Events(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for i, event := range events {
		if event.TaskID != task.ID || event.CreatedAt == "" || event.Data == nil {
			t.Fatalf("event: %#v", event)
		}
		if i > 0 && event.Seq <= events[i-1].Seq {
			t.Fatal("unordered events")
		}
		kinds = append(kinds, event.Kind)
	}
	want := []string{"added", "noted", "claimed", "released", "claimed", "completed", "noted", "reopened"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("event kinds %v, want %v", kinds, want)
	}
	if events[5].Data["evidence"] != "test://passed" || events[5].Actor != "alice" {
		t.Fatalf("evidence lost: %#v", events[5])
	}
	for _, query := range []string{"DELETE FROM events", "UPDATE events SET actor='tampered'"} {
		if _, err = s.db.Exec(query); err == nil {
			t.Fatal("audit mutation allowed")
		}
	}
}
func TestTransitiveBlockingAfterReopen(t *testing.T) {
	s, _ := database(t)
	a := add(t, s, "a")
	b := add(t, s, "b", a.ID)
	c := add(t, s, "c", b.ID)
	got, err := s.Get(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.BlockedBy) != 2 {
		t.Fatalf("blockers: %v", got.BlockedBy)
	}
	_, err = s.Claim(c.ID, "alice")
	code(t, err, "blocked")
	finish(t, s, a.ID)
	finish(t, s, b.ID)
	if _, err = s.Claim(c.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reopen(a.ID, "regression"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.BlockedBy, []string{a.ID}) {
		t.Fatalf("transitive blocker through done b missing: %v", got.BlockedBy)
	}
	_, err = s.Complete(c.ID, "alice", "evidence")
	code(t, err, "blocked")
	if _, err = s.Release(c.ID, "alice", "waiting"); err != nil {
		t.Fatal(err)
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != a.ID {
		t.Fatalf("ready: %v", ready)
	}
	finish(t, s, a.ID)
	finish(t, s, c.ID)
}
func TestDependencyGuardsAndCycles(t *testing.T) {
	s, _ := database(t)
	a := add(t, s, "a")
	b := add(t, s, "b", a.ID)
	c := add(t, s, "c", b.ID)
	_, err := s.AddDependency(a.ID, c.ID, "")
	code(t, err, "invalid")
	_, err = s.AddDependency(a.ID, a.ID, "")
	code(t, err, "invalid")
	_, err = s.AddDependency(b.ID, a.ID, "")
	code(t, err, "conflict")
	_, err = s.RemoveDependency(c.ID, a.ID, "")
	code(t, err, "conflict")
	_, err = s.AddDependency(a.ID, "missing", "")
	code(t, err, "not_found")
	if _, err = s.Claim(a.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	independent := add(t, s, "independent")
	_, err = s.AddDependency(a.ID, independent.ID, "bob")
	code(t, err, "conflict")
	if _, err = s.AddDependency(a.ID, independent.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	_, err = s.RemoveDependency(a.ID, independent.ID, "bob")
	code(t, err, "conflict")
	if _, err = s.RemoveDependency(a.ID, independent.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Complete(a.ID, "alice", "proof"); err != nil {
		t.Fatal(err)
	}
	_, err = s.AddDependency(a.ID, independent.ID, "")
	code(t, err, "conflict")
	_, err = s.RemoveDependency(a.ID, independent.ID, "")
	code(t, err, "conflict")
	events, err := s.Events(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("rejected mutations recorded events: %v", events)
	}
}
func TestMutationAndAuditRollbackTogether(t *testing.T) {
	s, _ := database(t)
	a := add(t, s, "a")
	b := add(t, s, "b")
	if _, err := s.db.Exec("CREATE TRIGGER fail_audit BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'simulated failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(a.ID, "alice"); err == nil {
		t.Fatal("claim should fail with audit write")
	}
	if _, err := s.AddDependency(a.ID, b.ID, ""); err == nil {
		t.Fatal("dependency should fail with audit write")
	}
	if _, err := s.Add("never saved", nil, nil); err == nil {
		t.Fatal("add should fail with audit write")
	}
	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" || got.Owner != "" || len(got.Dependencies) != 0 || got.UpdatedAt != a.UpdatedAt {
		t.Fatalf("partial mutation persisted: %#v", got)
	}
	events, err := s.Events(a.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit: %v %v", events, err)
	}
	tasks, err := s.List()
	if err != nil || len(tasks) != 2 {
		t.Fatalf("partial add: %v %v", tasks, err)
	}
}
func TestCompetingClaimsAndCycles(t *testing.T) {
	s, path := database(t)
	task := add(t, s, "race")
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, handle := range []*Store{s, second} {
		wg.Add(1)
		go func(i int, handle *Store) {
			defer wg.Done()
			<-start
			_, err := handle.Claim(task.ID, []string{"alice", "bob"}[i])
			results <- err
		}(i, handle)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			code(t, err, "conflict")
		}
	}
	if success != 1 {
		t.Fatalf("claim successes: %d", success)
	}
	events, err := s.Events(task.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("claim audit: %v %v", events, err)
	}
	a := add(t, s, "a")
	b := add(t, s, "b")
	start = make(chan struct{})
	results = make(chan error, 2)
	for i, handle := range []*Store{s, second} {
		wg.Add(1)
		go func(i int, handle *Store) {
			defer wg.Done()
			<-start
			ids := []string{a.ID, b.ID}
			_, err := handle.AddDependency(ids[i], ids[1-i], "")
			results <- err
		}(i, handle)
	}
	close(start)
	wg.Wait()
	close(results)
	success = 0
	for err := range results {
		if err == nil {
			success++
		} else {
			code(t, err, "invalid")
		}
	}
	if success != 1 {
		t.Fatalf("opposing dependency successes: %d", success)
	}
}
