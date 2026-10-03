// Package store persists Loom tasks and their audit history in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string           { return e.Message }
func problem(code, message string) error { return &Error{code, message} }

type Task struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Status       string   `json:"status"`
	Owner        string   `json:"owner"`
	Note         string   `json:"note"`
	Refs         []string `json:"refs"`
	Dependencies []string `json:"dependencies"`
	BlockedBy    []string `json:"blocked_by"`
	Evidence     string   `json:"evidence"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}
type Event struct {
	Seq       int64          `json:"seq"`
	TaskID    string         `json:"task_id"`
	Kind      string         `json:"kind"`
	Actor     string         `json:"actor"`
	Data      map[string]any `json:"data"`
	CreatedAt string         `json:"created_at"`
}
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE tasks (
 id TEXT PRIMARY KEY, title TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('open','in_progress','done')),
 owner TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', refs TEXT NOT NULL,
 evidence TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 CHECK((status='in_progress' AND owner<>'') OR (status<>'in_progress' AND owner=''))
);
CREATE TABLE dependencies (
 task_id TEXT NOT NULL REFERENCES tasks(id), prerequisite TEXT NOT NULL REFERENCES tasks(id),
 PRIMARY KEY(task_id,prerequisite), CHECK(task_id<>prerequisite)
);
CREATE TABLE events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id),
 kind TEXT NOT NULL, actor TEXT NOT NULL, data TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX events_task ON events(task_id,seq);
CREATE TRIGGER events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'events are append-only'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'events are append-only'); END;
PRAGMA user_version=1;
`

func connect(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// One connection per handle makes connection-scoped PRAGMAs deterministic.
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// Init reserves a new file exclusively. An existing file is never changed.
func Init(path string) (err error) {
	if strings.TrimSpace(path) == "" {
		return problem("invalid", "database path is required")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return problem("conflict", "database already exists")
	}
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	db, err := connect(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if _, err = db.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err = db.Exec(schema); err != nil {
		_, rollbackErr := db.Exec("ROLLBACK")
		return errors.Join(err, rollbackErr)
	}
	_, err = db.Exec("COMMIT")
	return err
}
func Open(path string) (s *Store, err error) {
	if strings.TrimSpace(path) == "" {
		return nil, problem("invalid", "database path is required")
	}
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, problem("not_found", "database does not exist; run loom init")
	}
	if err != nil {
		return nil, err
	}
	db, err := connect(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version != 1 {
		return nil, problem("storage", fmt.Sprintf("unsupported database schema version %d", version))
	}
	// Preparing these reads also rejects incomplete or unrelated versioned databases.
	for _, query := range []string{"SELECT id,title,status,owner,note,refs,evidence,created_at,updated_at FROM tasks LIMIT 0", "SELECT task_id,prerequisite FROM dependencies LIMIT 0", "SELECT seq,task_id,kind,actor,data,created_at FROM events LIMIT 0"} {
		rows, e := db.Query(query)
		if e != nil {
			return nil, e
		}
		if e = rows.Close(); e != nil {
			return nil, e
		}
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const taskColumns = "id,title,status,owner,note,refs,evidence,created_at,updated_at"

func load(ctx context.Context, q queryer, id string) (Task, error) {
	t := Task{Refs: []string{}, Dependencies: []string{}, BlockedBy: []string{}}
	var refs string
	err := q.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", id).Scan(&t.ID, &t.Title, &t.Status, &t.Owner, &t.Note, &refs, &t.Evidence, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, problem("not_found", "task not found: "+id)
	}
	if err != nil {
		return t, err
	}
	if err = json.Unmarshal([]byte(refs), &t.Refs); err != nil {
		return t, err
	}
	if t.Refs == nil {
		t.Refs = []string{}
	}
	rows, err := q.QueryContext(ctx, "SELECT prerequisite FROM dependencies WHERE task_id=? ORDER BY prerequisite", id)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var dep string
		if err = rows.Scan(&dep); err != nil {
			break
		}
		t.Dependencies = append(t.Dependencies, dep)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return t, err
	}
	rows, err = q.QueryContext(ctx, `WITH RECURSIVE ancestors(id) AS (
 SELECT prerequisite FROM dependencies WHERE task_id=?
 UNION SELECT d.prerequisite FROM dependencies d JOIN ancestors a ON d.task_id=a.id
 ) SELECT t.id FROM tasks t JOIN ancestors a ON t.id=a.id WHERE t.status<>'done' ORDER BY t.id`, id)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var dep string
		if err = rows.Scan(&dep); err != nil {
			break
		}
		t.BlockedBy = append(t.BlockedBy, dep)
	}
	return t, errors.Join(err, rows.Err(), rows.Close())
}

// read takes a snapshot so task state and its computed blockers agree.
func (s *Store) read(fn func(context.Context, *sql.Tx) error) (err error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			e := tx.Rollback()
			if !errors.Is(e, sql.ErrTxDone) {
				err = errors.Join(err, e)
			}
		}
	}()
	if err = fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Get(id string) (t Task, err error) {
	err = s.read(func(ctx context.Context, tx *sql.Tx) error { var e error; t, e = load(ctx, tx, id); return e })
	return
}
func (s *Store) List() (tasks []Task, err error) {
	tasks = []Task{}
	err = s.read(func(ctx context.Context, tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, "SELECT id FROM tasks ORDER BY created_at,id")
		if e != nil {
			return e
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		if e = errors.Join(e, rows.Err(), rows.Close()); e != nil {
			return e
		}
		for _, id := range ids {
			t, e := load(ctx, tx, id)
			if e != nil {
				return e
			}
			tasks = append(tasks, t)
		}
		return nil
	})
	return
}
func (s *Store) Ready() ([]Task, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	ready := []Task{}
	for _, t := range all {
		if t.Status == "open" && len(t.BlockedBy) == 0 {
			ready = append(ready, t)
		}
	}
	return ready, nil
}

// write holds SQLite's reserved writer lock before reading lifecycle or graph state.
func (s *Store) write(fn func(context.Context, *sql.Conn) error) (err error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, e := conn.ExecContext(ctx, "ROLLBACK")
			err = errors.Join(err, e)
		}
	}()
	if err = fn(ctx, conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return
}
func appendEvent(ctx context.Context, c *sql.Conn, id, kind, actor string, data map[string]any, now string) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "INSERT INTO events(task_id,kind,actor,data,created_at) VALUES(?,?,?,?,?)", id, kind, actor, string(encoded), now)
	return err
}
func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func required(value, label string) error {
	if strings.TrimSpace(value) == "" {
		return problem("invalid", label+" is required")
	}
	return nil
}
func unique(values []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		if err := required(v, "value"); err != nil {
			return nil, err
		}
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out, nil
}
func (s *Store) Add(title string, after, refs []string) (t Task, err error) {
	if err = required(title, "title"); err != nil {
		return
	}
	after, err = unique(after)
	if err != nil {
		return
	}
	refs, err = unique(refs)
	if err != nil {
		return
	}
	b := make([]byte, 8)
	if _, err = rand.Read(b); err != nil {
		return
	}
	id := "loom-" + hex.EncodeToString(b)
	err = s.write(func(ctx context.Context, c *sql.Conn) error {
		for _, dep := range after {
			if _, e := load(ctx, c, dep); e != nil {
				return e
			}
		}
		now := timestamp()
		encoded, e := json.Marshal(refs)
		if e != nil {
			return e
		}
		if _, e = c.ExecContext(ctx, "INSERT INTO tasks(id,title,status,refs,created_at,updated_at) VALUES(?,?,'open',?,?,?)", id, title, string(encoded), now, now); e != nil {
			return e
		}
		for _, dep := range after {
			if _, e = c.ExecContext(ctx, "INSERT INTO dependencies VALUES(?,?)", id, dep); e != nil {
				return e
			}
		}
		if e = appendEvent(ctx, c, id, "added", "", map[string]any{"title": title, "refs": refs, "dependencies": after}, now); e != nil {
			return e
		}
		t, e = load(ctx, c, id)
		return e
	})
	return
}
func ownerCheck(t Task, owner string) error {
	if t.Status == "in_progress" && t.Owner != owner {
		return problem("conflict", "task is claimed by another owner")
	}
	return nil
}
func (s *Store) mutate(id, kind, actor string, data map[string]any, fn func(context.Context, *sql.Conn, Task) error) (t Task, err error) {
	err = s.write(func(ctx context.Context, c *sql.Conn) error {
		before, e := load(ctx, c, id)
		if e != nil {
			return e
		}
		if e = fn(ctx, c, before); e != nil {
			return e
		}
		now := timestamp()
		if _, e = c.ExecContext(ctx, "UPDATE tasks SET updated_at=? WHERE id=?", now, id); e != nil {
			return e
		}
		if e = appendEvent(ctx, c, id, kind, actor, data, now); e != nil {
			return e
		}
		t, e = load(ctx, c, id)
		return e
	})
	return
}
func (s *Store) Claim(id, owner string) (Task, error) {
	if err := required(owner, "owner"); err != nil {
		return Task{}, err
	}
	return s.mutate(id, "claimed", owner, map[string]any{"owner": owner}, func(ctx context.Context, c *sql.Conn, t Task) error {
		if t.Status != "open" {
			return problem("conflict", "only open tasks can be claimed")
		}
		if len(t.BlockedBy) > 0 {
			return problem("blocked", "task has unfinished prerequisites")
		}
		_, err := c.ExecContext(ctx, "UPDATE tasks SET status='in_progress',owner=? WHERE id=?", owner, id)
		return err
	})
}
func (s *Store) Release(id, owner, reason string) (Task, error) {
	if err := required(owner, "owner"); err != nil {
		return Task{}, err
	}
	if err := required(reason, "reason"); err != nil {
		return Task{}, err
	}
	return s.mutate(id, "released", owner, map[string]any{"reason": reason}, func(ctx context.Context, c *sql.Conn, t Task) error {
		if t.Status != "in_progress" {
			return problem("conflict", "only claimed tasks can be released")
		}
		if err := ownerCheck(t, owner); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, "UPDATE tasks SET status='open',owner='' WHERE id=?", id)
		return err
	})
}
func (s *Store) Note(id, owner, text string) (Task, error) {
	if err := required(text, "note"); err != nil {
		return Task{}, err
	}
	return s.mutate(id, "noted", owner, map[string]any{"note": text}, func(ctx context.Context, c *sql.Conn, t Task) error {
		if err := ownerCheck(t, owner); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, "UPDATE tasks SET note=? WHERE id=?", text, id)
		return err
	})
}
func (s *Store) Complete(id, owner, evidence string) (Task, error) {
	if err := required(owner, "owner"); err != nil {
		return Task{}, err
	}
	if err := required(evidence, "evidence"); err != nil {
		return Task{}, err
	}
	return s.mutate(id, "completed", owner, map[string]any{"evidence": evidence}, func(ctx context.Context, c *sql.Conn, t Task) error {
		if t.Status != "in_progress" {
			return problem("conflict", "only claimed tasks can be completed")
		}
		if err := ownerCheck(t, owner); err != nil {
			return err
		}
		if len(t.BlockedBy) > 0 {
			return problem("blocked", "task has unfinished prerequisites")
		}
		_, err := c.ExecContext(ctx, "UPDATE tasks SET status='done',owner='',evidence=? WHERE id=?", evidence, id)
		return err
	})
}
func (s *Store) Reopen(id, reason string) (Task, error) {
	if err := required(reason, "reason"); err != nil {
		return Task{}, err
	}
	return s.mutate(id, "reopened", "", map[string]any{"reason": reason}, func(ctx context.Context, c *sql.Conn, t Task) error {
		if t.Status != "done" {
			return problem("conflict", "only done tasks can be reopened")
		}
		_, err := c.ExecContext(ctx, "UPDATE tasks SET status='open',owner='',evidence='' WHERE id=?", id)
		return err
	})
}
func (s *Store) AddDependency(id, prerequisite, owner string) (Task, error) {
	return s.dependency(id, prerequisite, owner, true)
}
func (s *Store) RemoveDependency(id, prerequisite, owner string) (Task, error) {
	return s.dependency(id, prerequisite, owner, false)
}
func (s *Store) dependency(id, dep, owner string, add bool) (Task, error) {
	kind := "dependency_removed"
	if add {
		kind = "dependency_added"
	}
	return s.mutate(id, kind, owner, map[string]any{"prerequisite": dep}, func(ctx context.Context, c *sql.Conn, t Task) error {
		if t.Status == "done" {
			return problem("conflict", "reopen a done task before changing dependencies")
		}
		if err := ownerCheck(t, owner); err != nil {
			return err
		}
		if _, err := load(ctx, c, dep); err != nil {
			return err
		}
		found := sort.SearchStrings(t.Dependencies, dep)
		exists := found < len(t.Dependencies) && t.Dependencies[found] == dep
		if add {
			if exists {
				return problem("conflict", "dependency already exists")
			}
			var cycle int
			err := c.QueryRowContext(ctx, `WITH RECURSIVE ancestors(id) AS (
    SELECT ? UNION SELECT d.prerequisite FROM dependencies d JOIN ancestors a ON d.task_id=a.id
   ) SELECT EXISTS(SELECT 1 FROM ancestors WHERE id=?)`, dep, id).Scan(&cycle)
			if err != nil {
				return err
			}
			if cycle != 0 {
				return problem("invalid", "dependency would create a cycle")
			}
			_, err = c.ExecContext(ctx, "INSERT INTO dependencies VALUES(?,?)", id, dep)
			return err
		}
		if !exists {
			return problem("conflict", "dependency does not exist")
		}
		_, err := c.ExecContext(ctx, "DELETE FROM dependencies WHERE task_id=? AND prerequisite=?", id, dep)
		return err
	})
}
func (s *Store) Events(id string) (events []Event, err error) {
	events = []Event{}
	err = s.read(func(ctx context.Context, tx *sql.Tx) error {
		if _, e := load(ctx, tx, id); e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, "SELECT seq,task_id,kind,actor,data,created_at FROM events WHERE task_id=? ORDER BY seq", id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var event Event
			var data string
			if e = rows.Scan(&event.Seq, &event.TaskID, &event.Kind, &event.Actor, &data, &event.CreatedAt); e != nil {
				break
			}
			if e = json.Unmarshal([]byte(data), &event.Data); e != nil {
				break
			}
			events = append(events, event)
		}
		return errors.Join(e, rows.Err(), rows.Close())
	})
	return
}
