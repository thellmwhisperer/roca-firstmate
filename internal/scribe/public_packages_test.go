package scribe_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thellmwhisperer/la-roca/pkg/corpuswriter"
	"github.com/thellmwhisperer/la-roca/pkg/incrementality"
	"github.com/thellmwhisperer/la-roca/pkg/ingestprovenance"
	"github.com/thellmwhisperer/la-roca/pkg/parsers"
)

func TestLaRocaPublicPackageContractsCompileForTheExternalPlugin(t *testing.T) {
	parsed := parsers.ParseMemoryFile([]byte("---\ntype: decision\n---\nfabricated\n"))
	if parsed.Type != "decision" {
		t.Fatalf("parser type = %q", parsed.Type)
	}
	if got := ingestprovenance.HarnessForSource("codex"); got != ingestprovenance.CodexCLI {
		t.Fatalf("canonical harness = %q", got)
	}
	if _, err := incrementality.Fingerprint("../../testdata/homes/northwind-harbor/data/captain.md"); err != nil {
		t.Fatalf("public fingerprint: %v", err)
	}
	var writer func(context.Context, *sql.Tx, corpuswriter.Records) (corpuswriter.Counts, error)
	writer = corpuswriter.Write
	if writer == nil {
		t.Fatal("public corpus writer is nil")
	}
}
