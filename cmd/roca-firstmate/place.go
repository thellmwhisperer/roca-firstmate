package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func runPlace(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("place", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	fs.StringVar(&dir, "dir", "", "plugin directory that should hold the executable")
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
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dbPath := strings.TrimSpace(os.Getenv("ROCA_FIRSTMATE_DB"))
		if dbPath == "" {
			fmt.Fprintln(stderr, "error: --dir is required unless ROCA_FIRSTMATE_DB is set")
			usage(stderr)
			return exitUsage
		}
		dir = filepath.Dir(dbPath)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	src, err := os.Executable()
	if err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	if resolved, resolveErr := filepath.EvalSymlinks(src); resolveErr == nil {
		src = resolved
	}
	dest := filepath.Join(dir, placedExecutableName())
	if same, err := sameFile(src, dest); err == nil && same {
		fmt.Fprintf(stdout, "status: placed\npath: %s\nhelp[1]:\n  - %s\n",
			quote(dest), quote("The plugin directory already holds this executable"))
		return exitOK
	}
	if err := copyExecutable(src, dest); err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	fmt.Fprintf(stdout, "status: placed\npath: %s\nhelp[1]:\n  - %s\n",
		quote(dest), quote("Raise `roca-firstmate watch` as a session-owned child from this path"))
	return exitOK
}

func placedExecutableName() string {
	if runtime.GOOS == "windows" {
		return "roca-firstmate.exe"
	}
	return "roca-firstmate"
}

func copyExecutable(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), "roca-firstmate-place-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func sameFile(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(leftInfo, rightInfo), nil
}
