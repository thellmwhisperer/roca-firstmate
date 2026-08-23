package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

const (
	packageFilename    = "plugin.json"
	checksumsFilename  = "checksums.txt"
	databaseFilename   = "firstmate.db"
	executableFilename = "roca-firstmate"
	pluginName         = "roca-firstmate"
)

// Options is the packager input. RepoRoot must contain plugin.json.
type Options struct {
	RepoRoot string
	Binary   string
	OutDir   string
	Version  string
}

type pluginIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Binary  string `json:"binary"`
}

// Package writes an installable plugin directory: plugin.json, the empty
// custodial firstmate.db, the plugin executable, and checksums.txt covering
// exactly those files.
func Package(opts Options) error {
	if strings.TrimSpace(opts.RepoRoot) == "" || strings.TrimSpace(opts.Binary) == "" || strings.TrimSpace(opts.OutDir) == "" {
		return fmt.Errorf("repo root, binary, and output directory are required")
	}
	raw, err := os.ReadFile(filepath.Join(opts.RepoRoot, packageFilename))
	if err != nil {
		return fmt.Errorf("read %s: %w", packageFilename, err)
	}
	manifest, err := decodeIdentity(raw)
	if err != nil {
		return err
	}
	if manifest.Name != pluginName {
		return fmt.Errorf("%s names plugin %q, want %s", packageFilename, manifest.Name, pluginName)
	}
	if manifest.Binary != executableFilename {
		return fmt.Errorf("%s declares binary %q, want %s", packageFilename, manifest.Binary, executableFilename)
	}
	version := strings.TrimPrefix(strings.TrimSpace(opts.Version), "v")
	if version != "" && version != manifest.Version {
		raw, err = rewriteVersion(raw, version)
		if err != nil {
			return err
		}
	}
	if err := prepareOutput(opts.OutDir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(opts.OutDir, packageFilename), raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", packageFilename, err)
	}
	if err := copyFile(opts.Binary, filepath.Join(opts.OutDir, executableFilename), 0o700); err != nil {
		return err
	}
	if err := writeEmptyDatabase(filepath.Join(opts.OutDir, databaseFilename)); err != nil {
		return err
	}
	return writeChecksums(opts.OutDir, payloadNames())
}

func payloadNames() []string {
	names := []string{packageFilename, databaseFilename, executableFilename}
	slices.Sort(names)
	return names
}

func decodeIdentity(raw []byte) (pluginIdentity, error) {
	var manifest pluginIdentity
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return pluginIdentity{}, fmt.Errorf("parse %s: %w", packageFilename, err)
	}
	return manifest, nil
}

func rewriteVersion(raw []byte, version string) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", packageFilename, err)
	}
	encoded, err := json.Marshal(version)
	if err != nil {
		return nil, err
	}
	document["version"] = encoded
	out, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func prepareOutput(out string) error {
	absolute, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolve package output: %w", err)
	}
	if filepath.Dir(absolute) == absolute {
		return fmt.Errorf("package output may not be a filesystem root")
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return fmt.Errorf("create package output: %w", err)
	}
	allowed := map[string]bool{
		packageFilename: true, checksumsFilename: true,
		databaseFilename: true, executableFilename: true,
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return fmt.Errorf("inspect package output: %w", err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !allowed[entry.Name()] || !info.Mode().IsRegular() {
			return fmt.Errorf("package output contains an unmanaged entry %s", filepath.Join(absolute, entry.Name()))
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(absolute, entry.Name())); err != nil {
			return fmt.Errorf("clear packaged file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func writeEmptyDatabase(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		return fmt.Errorf("create %s: %w", databaseFilename, err)
	}
	defer db.Close()
	if err := schema.Ensure(db); err != nil {
		return fmt.Errorf("apply schema to %s: %w", databaseFilename, err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close %s: %w", databaseFilename, err)
	}
	return nil
}

func writeChecksums(dir string, names []string) error {
	var body strings.Builder
	for _, name := range names {
		sum, err := fileDigest(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&body, "%s  %s\n", sum, name)
	}
	if err := os.WriteFile(filepath.Join(dir, checksumsFilename), []byte(body.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", checksumsFilename, err)
	}
	return nil
}

func fileDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open binary: %w", err)
	}
	defer input.Close()
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create packaged binary: %w", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := output.Chmod(mode); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close packaged binary: %w", err)
	}
	return nil
}

// Archive writes dir's package-root files to dest as a .tar.gz. Nested paths
// are refused: La Roca extracts only regular files at the archive root.
func Archive(dir, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("package directory contains nested path %s", entry.Name())
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	for _, name := range names {
		if err := writeTarFile(tw, dir, name); err != nil {
			tw.Close()
			gz.Close()
			file.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		file.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func writeTarFile(tw *tar.Writer, dir, name string) error {
	if filepath.Base(name) != name {
		return fmt.Errorf("archive entry %q is not a package-root file", name)
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""
	if name == executableFilename {
		header.Mode = 0o700
	} else {
		header.Mode = 0o600
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(tw, file)
	return err
}

func extractTarGz(archive, dest string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Base(header.Name)
		if name != header.Name && header.Name != "./"+name {
			return fmt.Errorf("entry %q is not a package-root file", header.Name)
		}
		path := filepath.Join(dest, name)
		mode := os.FileMode(header.Mode)
		if mode&0o111 != 0 {
			mode = 0o700
		} else {
			mode = 0o600
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, reader); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}
