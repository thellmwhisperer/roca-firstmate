package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/thellmwhisperer/roca-firstmate/internal/release"
)

func main() {
	binary := flag.String("binary", "", "built roca-firstmate executable to package")
	out := flag.String("out", "", "package output directory")
	version := flag.String("version", "", "package version; defaults to plugin.json")
	repo := flag.String("repo", ".", "repository root that holds plugin.json")
	archive := flag.String("archive", "", "optional .tar.gz path written from the package directory")
	flag.Parse()
	if err := run(*repo, *binary, *out, *version, *archive); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(repo, binary, out, version, archive string) error {
	if strings.TrimSpace(binary) == "" || strings.TrimSpace(out) == "" {
		return fmt.Errorf("binary and out are required")
	}
	if err := release.Package(release.Options{
		RepoRoot: repo, Binary: binary, OutDir: out, Version: version,
	}); err != nil {
		return err
	}
	if strings.TrimSpace(archive) == "" {
		return nil
	}
	return release.Archive(out, archive)
}
