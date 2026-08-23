// Command roca-firstmate runs Nerve's attach/follow/tick verbs, the on-demand
// AXI chart, and Scribe's mirror/watch maintenance commands.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/scribe"
	"github.com/thellmwhisperer/roca-firstmate/schema"
	"modernc.org/sqlite"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

type databasePathError struct {
	err error
}

func (e *databasePathError) Error() string { return e.err.Error() }
func (e *databasePathError) Unwrap() error { return e.err }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, nil, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "attach" {
		if len(args) > 0 {
			args = args[1:]
		}
		return runAttach(ctx, args, stdout, stderr)
	}
	if args[0] == "chart" {
		return runChart(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "follow" {
		return runFollow(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "tick" {
		return runTick(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "scribe" {
		return runScribe(ctx, args[1:], nil, stdout, stderr, false)
	}
	if args[0] == "watch" {
		return runScribe(ctx, args[1:], stdin, stdout, stderr, true)
	}
	if args[0] == "place" {
		return runPlace(args[1:], stdout, stderr)
	}
	if args[0] == "--help" || args[0] == "-h" {
		usage(stdout)
		return exitOK
	}
	fmt.Fprintf(stderr, "error: unknown command %q\n", args[0])
	usage(stderr)
	return exitUsage
}

type scribeFlags struct {
	dbPath       string
	homes        homeBinding
	sourceAgent  string
	asJSON       bool
	pollInterval time.Duration
}

func runScribe(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, continuous bool) int {
	name := "scribe"
	if continuous {
		name = "watch"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := scribeFlags{}
	bindDBFlag(fs, &values.dbPath)
	bindHomeFlags(fs, &values.homes)
	fs.StringVar(&values.sourceAgent, "source-agent", "firstmate", "source agent recorded in the cursor")
	fs.BoolVar(&values.asJSON, "json", false, "print JSON (NDJSON while watching)")
	fs.DurationVar(&values.pollInterval, "poll-interval", 30*time.Second, "polling fallback interval")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		usage(stderr)
		return exitUsage
	}
	if strings.TrimSpace(values.dbPath) == "" {
		values.dbPath = "firstmate.db"
	}
	req, err := resolveHomes(values.homes, os.Getenv("FIRSTMATE_HOME"), os.Getenv("FIRSTMATE_HOME_ID"), false)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		usage(stderr)
		return exitUsage
	}
	if len(req.Pairs) == 0 {
		fmt.Fprintln(stderr, "error: --home and --home-id are required")
		usage(stderr)
		return exitUsage
	}
	if continuous {
		return runWatch(ctx, values.dbPath, req.Pairs, values, stdin, stdout, stderr)
	}
	db, err := openDatabase(values.dbPath)
	if err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	defer db.Close()
	summaries := make([]scribe.Summary, 0, len(req.Pairs))
	for _, pair := range req.Pairs {
		ingester, err := newIngester(ctx, db, pair, values.sourceAgent)
		if err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		summary, err := ingester.Backfill(ctx)
		if err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		summaries = append(summaries, summary)
	}
	if err := renderScribeSummaries(stdout, values.asJSON, summaries); err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	return exitOK
}

func openDatabase(path string) (*sql.DB, error) {
	return openDatabaseContext(context.Background(), path, 5*time.Second)
}

func openWatchDatabase(ctx context.Context, path string) (*sql.DB, error) {
	return openDatabaseContext(ctx, path, defaultWatchShutdownTimeout)
}

func openDatabaseContext(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	if err := validateDatabasePath(path); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve firstmate.db: %w", err)
	}
	location := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	busyMillis := max(1, busyTimeout.Milliseconds())
	dsn := fmt.Sprintf("%s?mode=rw&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_txlock=immediate", location, busyMillis)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open firstmate.db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open firstmate.db: %w", err)
	}
	if err := schema.MigrateContext(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open firstmate.db: %w", err)
	}
	return db, nil
}

func validateDatabasePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return &databasePathError{err: err}
	}
	if !info.Mode().IsRegular() {
		return &databasePathError{err: fmt.Errorf("firstmate.db must be a regular file")}
	}
	if _, err := filepath.Abs(path); err != nil {
		return &databasePathError{err: fmt.Errorf("resolve firstmate.db: %w", err)}
	}
	return nil
}

func retryableDatabaseOpenError(err error) bool {
	var pathErr *databasePathError
	if errors.As(err, &pathErr) {
		return true
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return retryableSQLiteCode(sqliteErr.Code())
}

func retryableSQLiteCode(code int) bool {
	switch code & 0xff {
	case 5, 6, 7, 10, 14, 15:
		return true
	default:
		return false
	}
}

func renderScribeSummaries(w io.Writer, asJSON bool, summaries []scribe.Summary) error {
	if len(summaries) == 1 {
		return renderScribe(w, asJSON, summaries[0])
	}
	if asJSON {
		help := []string{}
		if len(summaries) > 0 {
			help = summaries[0].Help
		}
		return json.NewEncoder(w).Encode(struct {
			Status string           `json:"status"`
			Homes  []scribe.Summary `json:"homes"`
			Help   []string         `json:"help"`
		}{Status: "mirrored", Homes: summaries, Help: help})
	}
	for _, summary := range summaries {
		if err := renderScribe(w, false, summary); err != nil {
			return err
		}
	}
	return nil
}

func renderScribe(w io.Writer, asJSON bool, value any) error {
	if asJSON {
		return json.NewEncoder(w).Encode(value)
	}
	switch result := value.(type) {
	case scribe.Summary:
		fmt.Fprintf(w, "status: %s\nhome_id: %s\nscanned: %d\ninserted: %d\nunchanged: %d\n",
			result.Status, result.HomeID, result.Scanned, result.Inserted, result.Unchanged)
		fmt.Fprintf(w, "families[5]{family,inserted}:\n  working_set,%d\n  archives,%d\n  task_state,%d\n  operational_docs,%d\n  task_artifacts,%d\n",
			result.WorkingSet, result.Archives, result.TaskState, result.OperationalDocs, result.TaskArtifacts)
		fmt.Fprintf(w, "wakeups: %d\nhelp[%d]:\n", result.Wakeups, len(result.Help))
		for _, line := range result.Help {
			fmt.Fprintf(w, "  - %s\n", quote(line))
		}
	case scribe.Event:
		fmt.Fprintf(w, "event: %s\nhome_id: %s\nrelative_path: %s\nfamily: %s\nversion: %d\nwakeups: %d\n",
			result.Status, result.HomeID, quote(result.RelativePath), result.Family, result.Version, result.Wakeups)
	default:
		return fmt.Errorf("render unsupported Scribe result %T", value)
	}
	return nil
}

func renderWatchStart(w io.Writer, asJSON bool, backend string, summary scribe.Summary) error {
	if asJSON {
		return json.NewEncoder(w).Encode(struct {
			Status   string         `json:"status"`
			Backend  string         `json:"backend"`
			Backfill scribe.Summary `json:"backfill"`
		}{Status: "watching", Backend: backend, Backfill: summary})
	}
	fmt.Fprintf(w, "status: watching\nbackend: %s\n", backend)
	fmt.Fprintf(w, "backfill{scanned,inserted,unchanged,wakeups}:\n  %d,%d,%d,%d\n",
		summary.Scanned, summary.Inserted, summary.Unchanged, summary.Wakeups)
	fmt.Fprintf(w, "families[5]{family,inserted}:\n  working_set,%d\n  archives,%d\n  task_state,%d\n  operational_docs,%d\n  task_artifacts,%d\n",
		summary.WorkingSet, summary.Archives, summary.TaskState, summary.OperationalDocs, summary.TaskArtifacts)
	fmt.Fprintf(w, "help[%d]:\n", len(summary.Help))
	for _, line := range summary.Help {
		fmt.Fprintf(w, "  - %s\n", quote(line))
	}
	return nil
}

func printScribeError(w io.Writer, err error) {
	fmt.Fprintf(w, "error: %v\n", err)
	fmt.Fprintln(w, "help[1]:")
	fmt.Fprintln(w, "  - \"Run `roca-firstmate scribe --help` to inspect the mirror contract\"")
}

func usage(w io.Writer) {
	fmt.Fprint(w, `roca-firstmate mirrors firstmate homes and routes deterministic wakeups.

Usage:
  roca-firstmate [attach] --home PATH --home-id ID [--db PATH] [--destination companion]
  roca-firstmate chart [--home PATH --home-id ID] [--home-id ID] [--db PATH] [--json]
  roca-firstmate follow [--home PATH --home-id ID] [--home-id ID] [--db PATH] [--destination companion]
  roca-firstmate tick --home PATH --home-id ID [--db PATH] [--silence-after 5m]
  roca-firstmate scribe --home PATH --home-id ID [--db PATH] [--json]
  roca-firstmate watch --home PATH --home-id ID [--db PATH] [--json]
  roca-firstmate place [--dir PATH]
  roca-firstmate --help

Repeat --home PATH --home-id ID as positional pairs. Unbalanced flags exit 2.
chart and follow take optional --home-id filters; omitting them reads every
registered home. One process owns firstmate.db.

attach is the complete default gesture: ingest-on-read freshness, opaque seat
registration, chart get-or-create, latest handoff, and WAL subscription.
Attaching is subscribing. follow uses an opaque seat and prints one JSON line
per delivery attempt before confirming its generation in SQLite. tick reconciles
fingerprints, runs the persisted silence clock, drains orphan wakeups, and dies.
There is no default daemon and no KeepAlive process.

scribe is the total one-shot backfill and nightly fingerprint safety net.
watch performs that backfill, then uses one FSEvents subscription per home on
macOS with polling fallback. Every changed Markdown file under a home's data
becomes one new version row and one SQL-triggered wakeup tagged with home_id.
A session-owned parent (stdin/stdout, no port, no pid file) can raise watch as
a child that dies when stdin closes. Seat leases keep one holder per home;
stand-down children inherit when the holder exits. place copies this executable
into a plugin directory so a session parent can resolve it without PATH luck.

Every plugin verb fingerprint-sweeps Markdown before answering. There is no init ceremony.

Flags:
  --db string           path to firstmate.db (or ROCA_FIRSTMATE_DB; default firstmate.db)
  --home string         firstmate home (repeatable, paired with --home-id)
  --home-id string      stable local identity (repeatable; chart/follow filter when unpaired)
  --label string        explicit human-readable home and seat label
  --destination string  captain, companion, or machine (default companion)
  --json                print the complete envelope

Attach flags:
  --workspace string  workspace seat (default cwd; only an opaque hash is stored)
  --seat-id string    explicit seat identity (default derived from workspace)

Follow flags:
  --workspace string  workspace seat (default cwd; only an opaque hash is stored)
  --seat-id string    explicit seat identity (default derived from workspace)

Scribe/watch flags:
  --home string          firstmate home (repeatable, paired with --home-id)
  --home-id string       stable local identity (repeatable)
  --label string         human-readable label (default home-id)
  --kind string          primary or secondmate (default primary; repeatable)
  --source-agent string  cursor provenance (default firstmate)
  --poll-interval value  fallback interval (default 30s)

Place flags:
  --dir string  plugin directory that should hold the roca-firstmate executable
`)
}

func chartQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func quote(s string) string { return chartQuote(s) }
