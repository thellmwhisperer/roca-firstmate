package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/chart"
	"github.com/thellmwhisperer/roca-firstmate/internal/nerve"
	"github.com/thellmwhisperer/roca-firstmate/internal/scribe"
	filewatch "github.com/thellmwhisperer/roca-firstmate/internal/watch"
)

type mirrorVerbFlags struct {
	dbPath      string
	homes       homeBinding
	sourceAgent string
	destination string
	asJSON      bool
}

func bindDBFlag(fs *flag.FlagSet, dbPath *string) {
	fs.StringVar(dbPath, "db", os.Getenv("ROCA_FIRSTMATE_DB"), "path to firstmate.db")
}

func bindHomeFlags(fs *flag.FlagSet, homes *homeBinding) {
	fs.Var(&homes.homes, "home", "path to a firstmate home (repeatable, paired with --home-id)")
	fs.Var(&homes.homeIDs, "home-id", "stable local identity (repeatable, paired with --home)")
	fs.Var(&homes.labels, "label", "human-readable home and seat label (repeatable)")
	fs.Var(&homes.kinds, "kind", "home kind: primary or secondmate (repeatable)")
}

func bindMirrorVerbFlags(fs *flag.FlagSet, values *mirrorVerbFlags) {
	bindDBFlag(fs, &values.dbPath)
	bindHomeFlags(fs, &values.homes)
	fs.StringVar(&values.sourceAgent, "source-agent", "firstmate", "source agent recorded in the cursor")
	fs.StringVar(&values.destination, "destination", "companion", "wakeup destination: captain, companion, or machine")
	fs.BoolVar(&values.asJSON, "json", false, "print the complete envelope")
}

func (values *mirrorVerbFlags) normalizeDB() {
	if strings.TrimSpace(values.dbPath) == "" {
		values.dbPath = "firstmate.db"
	}
}

func parseIngestHomes(binding homeBinding) (homeRequest, error) {
	req, err := resolveHomes(binding, os.Getenv("FIRSTMATE_HOME"), os.Getenv("FIRSTMATE_HOME_ID"), false)
	if err != nil {
		if errors.Is(err, errHomeRequired) {
			return homeRequest{}, errHomeFreshness
		}
		return homeRequest{}, err
	}
	if len(req.Pairs) == 0 {
		return homeRequest{}, errHomeFreshness
	}
	return req, nil
}

func parseOptionalHomes(binding homeBinding) (homeRequest, error) {
	return resolveHomes(binding, os.Getenv("FIRSTMATE_HOME"), os.Getenv("FIRSTMATE_HOME_ID"), true)
}

func newIngester(ctx context.Context, db *sql.DB, pair homePair, sourceAgent string) (*scribe.Ingester, error) {
	return scribe.New(ctx, db, scribe.Config{
		Home: pair.Path, HomeID: pair.ID, Label: pair.Label,
		Kind: pair.Kind, SourceAgent: sourceAgent,
	})
}

func refreshMirrors(ctx context.Context, db *sql.DB, pairs []homePair, sourceAgent string) ([]scribe.Summary, error) {
	summaries := make([]scribe.Summary, 0, len(pairs))
	for _, pair := range pairs {
		ingester, err := newIngester(ctx, db, pair, sourceAgent)
		if err != nil {
			return nil, err
		}
		summary, err := ingester.Backfill(ctx)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func openDatabaseWithMirrors(ctx context.Context, dbPath string, pairs []homePair, sourceAgent string) (*sql.DB, []scribe.Summary, error) {
	db, err := openDatabase(dbPath)
	if err != nil {
		return nil, nil, err
	}
	if len(pairs) == 0 {
		return db, nil, nil
	}
	summaries, err := refreshMirrors(ctx, db, pairs, sourceAgent)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, summaries, nil
}

type attachEnvelope struct {
	Status    string         `json:"status"`
	Seat      nerve.Seat     `json:"seat"`
	Freshness scribe.Summary `json:"freshness"`
	Chart     chart.Result   `json:"chart"`
	Handoff   *nerve.Handoff `json:"handoff"`
	Help      []string       `json:"help"`
}

type attachMultiEnvelope struct {
	Status    string           `json:"status"`
	Seats     []nerve.Seat     `json:"seats"`
	Freshness []scribe.Summary `json:"freshness"`
	Chart     chart.Result     `json:"chart"`
	Handoff   *nerve.Handoff   `json:"handoff"`
	Help      []string         `json:"help"`
}

type seatVerbFlags struct {
	workspace string
	seatID    string
}

func bindSeatVerbFlags(fs *flag.FlagSet, values *seatVerbFlags) {
	fs.StringVar(&values.workspace, "workspace", "", "workspace to register (default current directory; only its hash is stored)")
	fs.StringVar(&values.seatID, "seat-id", "", "explicit stable seat identity (default derived from workspace)")
}

func (values *seatVerbFlags) normalize() error {
	if strings.TrimSpace(values.workspace) != "" {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	values.workspace = cwd
	return nil
}

func registerSeats(ctx context.Context, db *sql.DB, pairs []homePair, destination string, values seatVerbFlags) ([]nerve.Seat, error) {
	if len(pairs) > 1 && strings.TrimSpace(values.seatID) != "" {
		return nil, errors.New("--seat-id cannot be combined with multiple --home values")
	}
	seats := make([]nerve.Seat, 0, len(pairs))
	for _, pair := range pairs {
		seat, err := nerve.RegisterSeat(ctx, db, nerve.SeatConfig{
			SeatID: values.seatID, HomeID: pair.ID, Workspace: values.workspace,
			Label: pair.Label, Destination: destination, Now: time.Now().UTC(),
		})
		if err != nil {
			return nil, err
		}
		seats = append(seats, seat)
	}
	return seats, nil
}

func registerSeatsForIDs(ctx context.Context, db *sql.DB, homeIDs []string, destination string, values seatVerbFlags) ([]nerve.Seat, error) {
	pairs := make([]homePair, 0, len(homeIDs))
	for _, id := range homeIDs {
		pairs = append(pairs, homePair{ID: id, Label: id})
	}
	return registerSeats(ctx, db, pairs, destination, values)
}

func runAttach(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := mirrorVerbFlags{}
	bindMirrorVerbFlags(fs, &values)
	seatValues := seatVerbFlags{}
	bindSeatVerbFlags(fs, &seatValues)
	fs.Usage = func() { usage(stderr) }
	if err := parseVerbArgs(fs, args, stderr); err != nil {
		return verbParseCode(err)
	}
	values.normalizeDB()
	req, err := parseIngestHomes(values.homes)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		usage(stderr)
		return exitUsage
	}
	if err := seatValues.normalize(); err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	db, freshness, err := openDatabaseWithMirrors(ctx, values.dbPath, req.Pairs, values.sourceAgent)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	defer db.Close()
	seats, err := registerSeats(ctx, db, req.Pairs, values.destination, seatValues)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	chartResult, err := chart.GetOrCreate(db, time.Now().UTC())
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	handoff, err := nerve.LastHandoff(ctx, db)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	help := []string{
		"Attaching is subscribing; keep this command connected for wakeup lines",
		"Run `roca-firstmate tick` from cron for silence and orphan recovery",
	}
	if err := renderAttachResult(stdout, values.asJSON, seats, freshness, chartResult, handoff, help); err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	if err := nerve.Follow(ctx, db, values.dbPath, values.destination, seatIDs(seats), nil, stdout); err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	return exitOK
}

func runFollow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := mirrorVerbFlags{}
	bindMirrorVerbFlags(fs, &values)
	seatValues := seatVerbFlags{}
	bindSeatVerbFlags(fs, &seatValues)
	fs.Usage = func() { usage(stderr) }
	if err := parseVerbArgs(fs, args, stderr); err != nil {
		return verbParseCode(err)
	}
	values.normalizeDB()
	req, err := parseOptionalHomes(values.homes)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		usage(stderr)
		return exitUsage
	}
	if err := seatValues.normalize(); err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	db, _, err := openDatabaseWithMirrors(ctx, values.dbPath, req.Pairs, values.sourceAgent)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	defer db.Close()
	targets := req.Pairs
	if len(targets) == 0 {
		ids := req.Filters
		if len(ids) == 0 {
			ids, err = listRegisteredHomeIDs(ctx, db)
			if err != nil {
				printNerveError(stdout, err)
				return exitError
			}
		}
		if len(ids) == 0 {
			printNerveError(stdout, errors.New("no registered homes; pass --home PATH --home-id ID"))
			return exitError
		}
		seats, err := registerSeatsForIDs(ctx, db, ids, values.destination, seatValues)
		if err != nil {
			printNerveError(stdout, err)
			return exitError
		}
		if err := nerve.Follow(ctx, db, values.dbPath, values.destination, seatIDs(seats), req.Filters, stdout); err != nil {
			printNerveError(stdout, err)
			return exitError
		}
		return exitOK
	}
	seats, err := registerSeats(ctx, db, targets, values.destination, seatValues)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	if err := nerve.Follow(ctx, db, values.dbPath, values.destination, seatIDs(seats), nil, stdout); err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	return exitOK
}

func runTick(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tick", flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := mirrorVerbFlags{}
	bindMirrorVerbFlags(fs, &values)
	silenceAfter := fs.Duration("silence-after", 5*time.Minute, "silence generation interval")
	fs.Usage = func() { usage(stderr) }
	if err := parseVerbArgs(fs, args, stderr); err != nil {
		return verbParseCode(err)
	}
	values.normalizeDB()
	req, err := parseIngestHomes(values.homes)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		usage(stderr)
		return exitUsage
	}
	db, freshness, err := openDatabaseWithMirrors(ctx, values.dbPath, req.Pairs, values.sourceAgent)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	defer db.Close()
	result, err := nerve.Tick(ctx, db, time.Now().UTC(), *silenceAfter, stdout)
	if err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	if err := renderTick(stdout, values.asJSON, freshness, result); err != nil {
		printNerveError(stdout, err)
		return exitError
	}
	return exitOK
}

func runChart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := mirrorVerbFlags{}
	bindMirrorVerbFlags(fs, &values)
	fs.Usage = func() { usage(stderr) }
	if err := parseVerbArgs(fs, args, stderr); err != nil {
		return verbParseCode(err)
	}
	values.normalizeDB()
	req, err := parseOptionalHomes(values.homes)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		usage(stderr)
		return exitUsage
	}
	if _, err := os.Stat(values.dbPath); err != nil {
		fmt.Fprintf(stdout, "error: %s\n", err)
		fmt.Fprintf(stdout, "help[1]:\n  - %s\n",
			chartQuote("Run `roca-firstmate chart --db <path-to-firstmate.db>` to point at a database"))
		return exitError
	}

	db, _, err := openDatabaseWithMirrors(ctx, values.dbPath, req.Pairs, values.sourceAgent)
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
	result, err = chart.FilterHomes(db, result, req.Filters)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return exitError
	}
	if values.asJSON {
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

func parseVerbArgs(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return err
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		usage(stderr)
		return errUnexpectedArg
	}
	return nil
}

var errUnexpectedArg = errors.New("unexpected argument")

func verbParseCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if errors.Is(err, errUnexpectedArg) {
		return exitUsage
	}
	return exitUsage
}

func seatIDs(seats []nerve.Seat) []string {
	ids := make([]string, len(seats))
	for i, seat := range seats {
		ids[i] = seat.SeatID
	}
	return ids
}

func listRegisteredHomeIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT home_id FROM homes ORDER BY home_id`)
	if err != nil {
		return nil, fmt.Errorf("list homes: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func renderAttachResult(w io.Writer, asJSON bool, seats []nerve.Seat, freshness []scribe.Summary, chartResult chart.Result, handoff *nerve.Handoff, help []string) error {
	if len(seats) == 1 && len(freshness) == 1 {
		return renderAttach(w, asJSON, attachEnvelope{
			Status: "attached", Seat: seats[0], Freshness: freshness[0],
			Chart: chartResult, Handoff: handoff, Help: help,
		})
	}
	if asJSON {
		return json.NewEncoder(w).Encode(attachMultiEnvelope{
			Status: "attached", Seats: seats, Freshness: freshness,
			Chart: chartResult, Handoff: handoff, Help: help,
		})
	}
	fmt.Fprintf(w, "status: attached\n")
	fmt.Fprintf(w, "seats[%d]{status,seat_id,home_id,label,destination,attached_at,lease_until}:\n", len(seats))
	for _, seat := range seats {
		fmt.Fprintf(w, "  %s,%s,%s,%s,%s,%s,%s\n",
			seat.Status, seat.SeatID, seat.HomeID, quote(seat.Label),
			seat.Destination, seat.AttachedAt, seat.LeaseUntil)
	}
	fmt.Fprintf(w, "freshness[%d]{home_id,scanned,inserted,unchanged,wakeups}:\n", len(freshness))
	for _, summary := range freshness {
		fmt.Fprintf(w, "  %s,%d,%d,%d,%d\n",
			summary.HomeID, summary.Scanned, summary.Inserted, summary.Unchanged, summary.Wakeups)
	}
	fmt.Fprint(w, chart.RenderTOON(chartResult))
	if handoff == nil {
		fmt.Fprintln(w, "handoff[0]{home_id,relative_path,version,observed_at,content}:")
	} else {
		fmt.Fprintln(w, "handoff[1]{home_id,relative_path,version,observed_at,content}:")
		fmt.Fprintf(w, "  %s,%s,%d,%s,%s\n", handoff.HomeID,
			quote(handoff.RelativePath), handoff.Version, handoff.ObservedAt,
			quote(boundedOneLine(handoff.Content, 2000)))
	}
	destination := "companion"
	if len(seats) > 0 {
		destination = seats[0].Destination
	}
	fmt.Fprintf(w, "subscription: %s\nhelp[%d]:\n", destination, len(help))
	for _, line := range help {
		fmt.Fprintf(w, "  - %s\n", quote(line))
	}
	return nil
}

func renderAttach(w io.Writer, asJSON bool, result attachEnvelope) error {
	if asJSON {
		return json.NewEncoder(w).Encode(result)
	}
	fmt.Fprintf(w, "status: %s\n", result.Status)
	fmt.Fprintf(w, "seat{status,seat_id,home_id,label,destination,attached_at,lease_until}:\n")
	fmt.Fprintf(w, "  %s,%s,%s,%s,%s,%s,%s\n",
		result.Seat.Status, result.Seat.SeatID, result.Seat.HomeID, quote(result.Seat.Label),
		result.Seat.Destination, result.Seat.AttachedAt, result.Seat.LeaseUntil)
	fmt.Fprintf(w, "freshness{scanned,inserted,unchanged,wakeups}:\n  %d,%d,%d,%d\n",
		result.Freshness.Scanned, result.Freshness.Inserted, result.Freshness.Unchanged, result.Freshness.Wakeups)
	fmt.Fprint(w, chart.RenderTOON(result.Chart))
	if result.Handoff == nil {
		fmt.Fprintln(w, "handoff[0]{home_id,relative_path,version,observed_at,content}:")
	} else {
		fmt.Fprintln(w, "handoff[1]{home_id,relative_path,version,observed_at,content}:")
		fmt.Fprintf(w, "  %s,%s,%d,%s,%s\n", result.Handoff.HomeID,
			quote(result.Handoff.RelativePath), result.Handoff.Version, result.Handoff.ObservedAt,
			quote(boundedOneLine(result.Handoff.Content, 2000)))
	}
	fmt.Fprintf(w, "subscription: %s\nhelp[%d]:\n", result.Seat.Destination, len(result.Help))
	for _, line := range result.Help {
		fmt.Fprintf(w, "  - %s\n", quote(line))
	}
	return nil
}

func renderTick(w io.Writer, asJSON bool, freshness []scribe.Summary, result nerve.TickResult) error {
	if asJSON {
		if len(freshness) <= 1 {
			var one scribe.Summary
			if len(freshness) == 1 {
				one = freshness[0]
			}
			return json.NewEncoder(w).Encode(struct {
				Freshness scribe.Summary   `json:"freshness"`
				Tick      nerve.TickResult `json:"tick"`
			}{Freshness: one, Tick: result})
		}
		return json.NewEncoder(w).Encode(struct {
			Freshness []scribe.Summary `json:"freshness"`
			Tick      nerve.TickResult `json:"tick"`
		}{Freshness: freshness, Tick: result})
	}
	fmt.Fprintf(w, "status: %s\n", result.Status)
	if len(freshness) <= 1 {
		var one scribe.Summary
		if len(freshness) == 1 {
			one = freshness[0]
		}
		fmt.Fprintf(w, "freshness{scanned,inserted,unchanged,wakeups}:\n  %d,%d,%d,%d\n",
			one.Scanned, one.Inserted, one.Unchanged, one.Wakeups)
	} else {
		fmt.Fprintf(w, "freshness[%d]{home_id,scanned,inserted,unchanged,wakeups}:\n", len(freshness))
		for _, summary := range freshness {
			fmt.Fprintf(w, "  %s,%d,%d,%d,%d\n",
				summary.HomeID, summary.Scanned, summary.Inserted, summary.Unchanged, summary.Wakeups)
		}
	}
	fmt.Fprintf(w, "silence_wakeups: %d\norphan_wakeups: %d\n", result.SilenceWakeups, result.OrphanWakeups)
	fmt.Fprintf(w, "destinations[%d]{destination}:\n", len(result.Destinations))
	for _, destination := range result.Destinations {
		fmt.Fprintf(w, "  %s\n", destination)
	}
	fmt.Fprintf(w, "help[%d]:\n", len(result.Help))
	for _, line := range result.Help {
		fmt.Fprintf(w, "  - %s\n", quote(line))
	}
	return nil
}

func printNerveError(w io.Writer, err error) {
	fmt.Fprintf(w, "error: %v\n", err)
	fmt.Fprintln(w, "help[1]:")
	fmt.Fprintln(w, "  - \"Run `roca-firstmate --help` to inspect the Nerve v1 contract\"")
}

func boundedOneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}

type homeWatch struct {
	ingester watchIngester
	source   filewatch.Source
}

type watchMsg struct {
	idx        int
	generation uint64
	path       string
	err        error
	eof        bool
}

func renderWatchStarts(w io.Writer, asJSON bool, backend string, summaries []scribe.Summary) error {
	if len(summaries) <= 1 {
		var one scribe.Summary
		if len(summaries) == 1 {
			one = summaries[0]
		}
		return renderWatchStart(w, asJSON, backend, one)
	}
	if asJSON {
		return json.NewEncoder(w).Encode(struct {
			Status  string           `json:"status"`
			Backend string           `json:"backend"`
			Homes   []scribe.Summary `json:"homes"`
		}{Status: "watching", Backend: backend, Homes: summaries})
	}
	fmt.Fprintf(w, "status: watching\nbackend: %s\n", backend)
	fmt.Fprintf(w, "homes[%d]{home_id,scanned,inserted,unchanged,wakeups}:\n", len(summaries))
	for _, summary := range summaries {
		fmt.Fprintf(w, "  %s,%d,%d,%d,%d\n",
			summary.HomeID, summary.Scanned, summary.Inserted, summary.Unchanged, summary.Wakeups)
	}
	help := summaries[0].Help
	fmt.Fprintf(w, "help[%d]:\n", len(help))
	for _, line := range help {
		fmt.Fprintf(w, "  - %s\n", quote(line))
	}
	return nil
}

func watchBackend(watches []homeWatch) string {
	if len(watches) == 0 {
		return ""
	}
	backend := watches[0].source.Backend()
	for _, watch := range watches[1:] {
		if watch.source.Backend() != backend {
			return backend + "," + watch.source.Backend()
		}
	}
	return backend
}
