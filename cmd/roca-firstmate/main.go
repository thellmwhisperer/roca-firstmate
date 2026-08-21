// Command roca-firstmate runs Scribe's mirror/watch loop and the on-demand AXI
// chart for a firstmate.db.
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

	"github.com/thellmwhisperer/roca-firstmate/internal/chart"
	"github.com/thellmwhisperer/roca-firstmate/internal/scribe"
	filewatch "github.com/thellmwhisperer/roca-firstmate/internal/watch"
	_ "modernc.org/sqlite"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "chart" {
		if len(args) > 0 && args[0] == "chart" {
			args = args[1:]
		}
		return runChart(args, stdout, stderr)
	}
	if args[0] == "scribe" {
		return runScribe(ctx, args[1:], stdout, stderr, false)
	}
	if args[0] == "watch" {
		return runScribe(ctx, args[1:], stdout, stderr, true)
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
	home         string
	homeID       string
	label        string
	kind         string
	sourceAgent  string
	asJSON       bool
	pollInterval time.Duration
}

func runScribe(ctx context.Context, args []string, stdout, stderr io.Writer, continuous bool) int {
	name := "scribe"
	if continuous {
		name = "watch"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := scribeFlags{}
	fs.StringVar(&values.dbPath, "db", os.Getenv("ROCA_FIRSTMATE_DB"), "path to firstmate.db")
	fs.StringVar(&values.home, "home", os.Getenv("FIRSTMATE_HOME"), "path to the firstmate home")
	fs.StringVar(&values.homeID, "home-id", "", "stable local identity for this home")
	fs.StringVar(&values.label, "label", "", "human-readable home label (default home-id)")
	fs.StringVar(&values.kind, "kind", "primary", "home kind: primary or secondmate")
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
	if strings.TrimSpace(values.home) == "" || strings.TrimSpace(values.homeID) == "" {
		fmt.Fprintln(stderr, "error: --home and --home-id are required")
		usage(stderr)
		return exitUsage
	}
	db, err := openDatabase(values.dbPath)
	if err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	defer db.Close()
	ingester, err := scribe.New(ctx, db, scribe.Config{
		Home: values.home, HomeID: values.homeID, Label: values.label,
		Kind: values.kind, SourceAgent: values.sourceAgent,
	})
	if err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	if !continuous {
		summary, err := ingester.Backfill(ctx)
		if err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		if err := renderScribe(stdout, values.asJSON, summary); err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		return exitOK
	}

	source, err := filewatch.New(ingester.DataRoot(), values.pollInterval)
	if err != nil {
		printScribeError(stdout, fmt.Errorf("watch: %w", err))
		return exitError
	}
	defer source.Close()
	summary, err := ingester.Backfill(ctx)
	if err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	if err := renderWatchStart(stdout, values.asJSON, source.Backend(), summary); err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	events := source.Events()
	errors := source.Errors()
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case err, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
			printScribeError(stdout, err)
			return exitError
		case path, ok := <-events:
			if !ok {
				return exitError
			}
			if filepath.Clean(path) == filepath.Clean(ingester.DataRoot()) {
				recovery, err := ingester.Backfill(ctx)
				if err != nil {
					printScribeError(stdout, err)
					return exitError
				}
				if err := renderScribe(stdout, values.asJSON, recovery); err != nil {
					printScribeError(stdout, err)
					return exitError
				}
				continue
			}
			event, err := ingester.IngestPath(ctx, path)
			if err != nil {
				printScribeError(stdout, err)
				return exitError
			}
			if !event.Inserted {
				continue
			}
			if err := renderScribe(stdout, values.asJSON, event); err != nil {
				printScribeError(stdout, err)
				return exitError
			}
		}
	}
}

func openDatabase(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve firstmate.db: %w", err)
	}
	location := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	db, err := sql.Open("sqlite", location+"?mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open firstmate.db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open firstmate.db: %w", err)
	}
	return db, nil
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

func runChart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("ROCA_FIRSTMATE_DB"), "path to firstmate.db")
	asJSON := fs.Bool("json", false, "print the complete envelope")
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
	if strings.TrimSpace(*dbPath) == "" {
		*dbPath = "firstmate.db"
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(stdout, "error: %s\n", err)
		fmt.Fprintf(stdout, "help[1]:\n  - %s\n",
			chartQuote("Run `roca-firstmate chart --db <path-to-firstmate.db>` to point at a database"))
		return exitError
	}

	db, err := openDatabase(*dbPath)
	if err != nil {
		fmt.Fprintf(stdout, "error: open firstmate.db: %v\n", err)
		return exitError
	}
	defer db.Close()

	result, err := chart.GetOrCreate(db, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return exitError
	}
	if *asJSON {
		raw, err := chart.RenderJSON(result)
		if err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
			return exitError
		}
		fmt.Fprintln(stdout, string(raw))
		return exitOK
	}
	fmt.Fprint(stdout, chart.RenderTOON(result))
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprint(w, `roca-firstmate mirrors a firstmate home and serves its on-demand chart.

Usage:
  roca-firstmate chart [--db PATH] [--json]
  roca-firstmate scribe --home PATH --home-id ID [--db PATH] [--json]
  roca-firstmate watch --home PATH --home-id ID [--db PATH] [--json]
  roca-firstmate --help

scribe is the total one-shot backfill and nightly fingerprint safety net.
watch performs that backfill, then uses FSEvents on macOS with polling fallback.
Every changed Markdown file under home/data becomes one new version row and
one SQL-triggered wakeup. No inference runs in this chain.

Nothing runs at session-start. There is no init ceremony.

Flags:
  --db string   path to firstmate.db (or ROCA_FIRSTMATE_DB; default firstmate.db)
  --json        print the complete envelope

Scribe/watch flags:
  --home string          firstmate home (or FIRSTMATE_HOME; required)
  --home-id string       stable local identity (required)
  --label string         human-readable label (default home-id)
  --kind string          primary or secondmate (default primary)
  --source-agent string  cursor provenance (default firstmate)
  --poll-interval value  fallback interval (default 30s)
`)
}

func chartQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func quote(s string) string { return chartQuote(s) }
