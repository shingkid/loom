// Loom keeps a durable work graph for cooperating local agents.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"loom/internal/store"
)

const version = "0.1.0"

const help = `Loom - durable work, connected across threads.

Usage: loom COMMAND [ARGUMENTS] [OPTIONS]

  init                           Create a database (never overwrite)
  add TITLE [--after ID] [--ref LINK]
                                 Add work; --after/--ref can repeat
  list                           Show all work and current blockers
  ready                          Show open work with no unfinished prerequisites
  show ID                        Show one task and its handoff note
  claim ID --owner NAME           Atomically reserve ready work
  release ID --owner NAME --reason TEXT
                                 Release a claim back to open work
  note ID TEXT [--owner NAME]     Replace the handoff note; retain its history
  close ID --owner NAME --evidence REF
                                 Record completion supported by a reference
  reopen ID --reason TEXT         Reopen completed work
  dep add ID PREREQUISITE [--owner NAME]
  dep remove ID PREREQUISITE [--owner NAME]
                                 Change prerequisites; cycles are rejected
  events ID                      Read the task's audit history
  version                        Show version

Global options (before or after the command):
  --db PATH    Use this database; otherwise LOOM_DB, then ancestor .loom/loom.db
  --json       Emit JSON; failures go to stderr as {"error":{"code","message"}}
  --help, -h   Show this help
  --           Treat remaining arguments literally

Use the same absolute --db path for chats and worktrees sharing a project.
Claims do not expire automatically. Evidence references are stored, not verified.
`

type options struct {
	values map[string][]string
	json   bool
	help   bool
}

func (o options) value(key string) string {
	if values := o.values[key]; len(values) != 0 {
		return values[0]
	}
	return ""
}

func invalid(message string) error { return &store.Error{Code: "invalid", Message: message} }

// parse accepts flags on either side of positional arguments without dependencies.
func parse(args []string) (options, []string, error) {
	o := options{values: map[string][]string{}}
	var positional []string
	literal := false
	var firstErr error
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if literal {
			positional = append(positional, arg)
			continue
		}
		if arg == "--" {
			literal = true
			continue
		}
		if arg == "--json" {
			o.json = true
			continue
		}
		if arg == "--help" || arg == "-h" {
			o.help = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		key, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		switch key {
		case "db", "owner", "reason", "evidence", "after", "ref":
			if !hasValue {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
					firstErr = errors.Join(firstErr, invalid("missing value for --"+key))
					continue
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				firstErr = errors.Join(firstErr, invalid("empty value for --"+key))
			}
			if len(o.values[key]) > 0 && key != "after" && key != "ref" {
				firstErr = errors.Join(firstErr, invalid("--"+key+" can only be supplied once"))
			}
			o.values[key] = append(o.values[key], value)
		default:
			firstErr = errors.Join(firstErr, invalid("unknown option: "+arg))
		}
	}
	return o, positional, firstErr
}

func validate(o options, args []string) error {
	if len(args) == 0 {
		return invalid("a command is required; use loom --help")
	}
	count := 1
	allowed := map[string]bool{"db": true}
	required := []string{}
	switch args[0] {
	case "init", "list", "ready", "version":
	case "add":
		count = 2
		allowed["after"], allowed["ref"] = true, true
	case "show", "events":
		count = 2
	case "claim":
		count = 2
		required = []string{"owner"}
	case "release":
		count = 2
		required = []string{"owner", "reason"}
	case "close":
		count = 2
		required = []string{"owner", "evidence"}
	case "reopen":
		count = 2
		required = []string{"reason"}
	case "note":
		count = 3
		allowed["owner"] = true
	case "dep":
		count = 4
		allowed["owner"] = true
		if len(args) < 2 || (args[1] != "add" && args[1] != "remove") {
			return invalid("use loom dep add ID PREREQUISITE or loom dep remove ID PREREQUISITE")
		}
	default:
		return invalid("unknown command: " + args[0])
	}
	if len(args) != count {
		return invalid(fmt.Sprintf("%s expects %d positional argument(s); quote titles and notes; use loom --help", args[0], count-1))
	}
	for _, key := range required {
		allowed[key] = true
		if o.value(key) == "" {
			return invalid("--" + key + " is required for " + args[0])
		}
	}
	keys := make([]string, 0, len(o.values))
	for key := range o.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !allowed[key] {
			return invalid("--" + key + " is not supported by " + args[0])
		}
	}
	return nil
}

func databasePath(explicit string, initializing bool) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("LOOM_DB")
	}
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if initializing {
		return filepath.Join(cwd, ".loom", "loom.db"), nil
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".loom", "loom.db")
		info, err := os.Stat(candidate)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", invalid("database is not a regular file: " + candidate)
			}
			return candidate, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return "", &store.Error{Code: "not_found", Message: "no Loom database found; run loom init or pass --db PATH"}
}

func execute(o options, args []string) (result any, err error) {
	if args[0] == "version" {
		return map[string]string{"version": version}, nil
	}
	path, err := databasePath(o.value("db"), args[0] == "init")
	if err != nil {
		return nil, err
	}
	if args[0] == "init" {
		if err := store.Init(path); err != nil {
			return nil, err
		}
		return map[string]string{"database": path}, nil
	}
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	switch args[0] {
	case "add":
		deps := []string{}
		for _, value := range o.values["after"] {
			for _, id := range strings.Split(value, ",") {
				deps = append(deps, strings.TrimSpace(id))
			}
		}
		return s.Add(args[1], deps, o.values["ref"])
	case "list":
		return s.List()
	case "ready":
		return s.Ready()
	case "show":
		return s.Get(args[1])
	case "claim":
		return s.Claim(args[1], o.value("owner"))
	case "release":
		return s.Release(args[1], o.value("owner"), o.value("reason"))
	case "note":
		return s.Note(args[1], o.value("owner"), args[2])
	case "close":
		return s.Complete(args[1], o.value("owner"), o.value("evidence"))
	case "reopen":
		return s.Reopen(args[1], o.value("reason"))
	case "dep":
		if args[1] == "add" {
			return s.AddDependency(args[2], args[3], o.value("owner"))
		}
		return s.RemoveDependency(args[2], args[3], o.value("owner"))
	case "events":
		return s.Events(args[1])
	}
	return nil, invalid("unsupported command")
}

func render(out io.Writer, result any, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	switch value := result.(type) {
	case map[string]string:
		if path, ok := value["database"]; ok {
			_, err := fmt.Fprintln(out, "Initialized", path)
			return err
		}
		_, err := fmt.Fprintln(out, "loom", value["version"])
		return err
	case store.Task:
		_, err := fmt.Fprintf(out, "%s  %s\nTitle: %s\nOwner: %s\nPrerequisites: %s\nBlocked by: %s\nNote: %s\nEvidence: %s\nReferences: %s\n",
			value.ID, value.Status, value.Title, value.Owner, strings.Join(value.Dependencies, ", "), strings.Join(value.BlockedBy, ", "), value.Note, value.Evidence, strings.Join(value.Refs, ", "))
		return err
	case []store.Task:
		if len(value) == 0 {
			_, err := fmt.Fprintln(out, "No tasks.")
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tOWNER\tTITLE\tBLOCKED BY"); err != nil {
			return err
		}
		for _, task := range value {
			clean := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", task.ID, task.Status, clean.Replace(task.Owner), clean.Replace(task.Title), strings.Join(task.BlockedBy, ",")); err != nil {
				return err
			}
		}
		return tw.Flush()
	case []store.Event:
		for _, event := range value {
			data, err := json.Marshal(event.Data)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "%d  %s  %s  %s  %s\n", event.Seq, event.CreatedAt, event.Kind, event.Actor, data); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unexpected output type %T", result)
}

func fail(out io.Writer, err error, asJSON bool) int {
	code := "storage"
	var problem *store.Error
	if errors.As(err, &problem) {
		code = problem.Code
	}
	if asJSON {
		_ = json.NewEncoder(out).Encode(map[string]any{"error": map[string]string{"code": code, "message": err.Error()}})
	} else {
		_, _ = fmt.Fprintf(out, "loom: %s: %s\n", code, err)
	}
	if code == "invalid" {
		return 2
	}
	return 1
}

func run(args []string, out, errOut io.Writer) int {
	o, positional, err := parse(args)
	if err != nil {
		return fail(errOut, err, o.json)
	}
	if o.help || len(args) == 0 {
		if o.json {
			err = render(out, map[string]string{"help": help}, true)
		} else {
			_, err = io.WriteString(out, help)
		}
		if err != nil {
			return fail(errOut, err, o.json)
		}
		return 0
	}
	if err = validate(o, positional); err != nil {
		return fail(errOut, err, o.json)
	}
	result, err := execute(o, positional)
	if err != nil {
		return fail(errOut, err, o.json)
	}
	if err = render(out, result, o.json); err != nil {
		return fail(errOut, err, o.json)
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
