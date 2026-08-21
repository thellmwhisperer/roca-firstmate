// Command roca-firstmate is the on-demand AXI chart for a firstmate.db.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/chart"
	_ "modernc.org/sqlite"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "chart" {
		if len(args) > 0 && args[0] == "chart" {
			args = args[1:]
		}
		return runChart(args, stdout, stderr)
	}
	if args[0] == "--help" || args[0] == "-h" {
		usage(stdout)
		return exitOK
	}
	fmt.Fprintf(stderr, "error: unknown command %q\n", args[0])
	usage(stderr)
	return exitUsage
}

func runChart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("ROCA_FIRSTMATE_DB"), "path to firstmate.db")
	asJSON := fs.Bool("json", false, "print the complete envelope")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
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

	db, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=rw&_pragma=foreign_keys(1)")
	if err != nil {
		fmt.Fprintf(stdout, "error: open firstmate.db: %v\n", err)
		return exitError
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fmt.Fprintf(stdout, "error: open firstmate.db: %v\n", err)
		return exitError
	}

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
	fmt.Fprint(w, `roca-firstmate chart: on-demand AXI get-or-create of the firstmate chart.

Usage:
  roca-firstmate chart [--db PATH] [--json]
  roca-firstmate --help

If the chart does not exist, it is generated from firstmate.db.
If it exists and the watermark still holds, the stored chart is shown.
If new rows exist, the chart is regenerated.

Nothing runs at session-start. There is no init ceremony.

Flags:
  --db string   path to firstmate.db (or ROCA_FIRSTMATE_DB; default firstmate.db)
  --json        print the complete envelope
`)
}

func chartQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
