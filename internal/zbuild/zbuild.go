// Package zbuild produces reproducible source packages and their manifests.
//
// Reproducibility rules enforced here (see specifications/RELEASE_PROCESS.md):
//   - only tracked files are packaged (git ls-files), never working-tree scratch
//   - paths, modes and contents are recorded in a deterministic order
//   - zip entry timestamps are normalised to 1980-01-01 so two identical trees
//     produce byte-identical archives on any host
//   - the archive never contains git internals, build output, chain data or keys
package zbuild

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExcludedPrefixes are never packaged even if they are tracked. "releases/" is
// packaging output: including it would make the package contain its own manifest,
// which cannot be hashed before it exists. Excluding it keeps the archive a pure
// function of the source tree.
var ExcludedPrefixes = []string{".git/", "bin/", "data/", "releases/"}

// ExcludedSuffixes are never packaged.
var ExcludedSuffixes = []string{".zip", ".log", ".key.json"}

// fixedModTime is the zip epoch: 1980-01-01T00:00:00Z in MS-DOS format.
var fixedModTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// FileEntry describes one packaged file.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

// Manifest is the machine-readable description of a package.
type Manifest struct {
	Package      string      `json:"package"`
	Version      string      `json:"version"`
	CreatedUTC   string      `json:"created_utc"`
	GoVersion    string      `json:"go_version"`
	ModulePath   string      `json:"module_path"`
	FileCount    int         `json:"file_count"`
	TotalBytes   int64       `json:"total_bytes"`
	Archive      FileEntry   `json:"archive"`
	Files        []FileEntry `json:"files"`
	BuildCommand string      `json:"build_command"`
	LegacyNote   string      `json:"legacy_note,omitempty"`
}

// TrackedFiles returns the repository-relative paths of tracked files, sorted.
// It shells out to git so the package reflects exactly what is committed.
func TrackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("zbuild: git ls-files: %w", err)
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || excluded(p) {
			continue
		}
		files = append(files, p)
	}
	sort.Strings(files)
	return files, nil
}

func excluded(p string) bool {
	for _, pre := range ExcludedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	for _, suf := range ExcludedSuffixes {
		if strings.HasSuffix(p, suf) {
			return true
		}
	}
	return false
}

// HashFile returns the hex SHA-256 of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Create makes a deterministic zip of the tracked files under root and writes it
// to outPath. It returns the manifest without the archive entry filled in (the
// caller adds that after hashing, since a zip cannot hash itself).
func Create(root, outPath, version string) (*Manifest, error) {
	files, err := TrackedFiles(root)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("zbuild: no tracked files under %s", root)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, err
	}
	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(f)
	m := &Manifest{Version: version, GoVersion: goVersion(root), ModulePath: "znetwork"}
	for _, rel := range files {
		src := filepath.Join(root, filepath.FromSlash(rel))
		st, err := os.Stat(src)
		if err != nil {
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return nil, fmt.Errorf("zbuild: stat %s: %w", rel, err)
		}
		if !st.Mode().IsRegular() {
			continue
		}
		entry := FileEntry{Path: rel, Size: st.Size(), Mode: fmt.Sprintf("%04o", st.Mode().Perm())}
		hdr := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: fixedModTime}
		hdr.SetMode(st.Mode().Perm())
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return nil, err
		}
		in, err := os.Open(src)
		if err != nil {
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return nil, err
		}
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(w, h), in); err != nil {
			in.Close()
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return nil, err
		}
		in.Close()
		entry.SHA256 = hex.EncodeToString(h.Sum(nil))
		m.Files = append(m.Files, entry)
		m.TotalBytes += entry.Size
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return nil, err
	}
	m.FileCount = len(m.Files)
	m.Package = filepath.Base(outPath)
	m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
	sum, err := HashFile(outPath)
	if err != nil {
		return nil, err
	}
	st, _ := os.Stat(outPath)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	m.Archive = FileEntry{Path: m.Package, Size: size, SHA256: sum, Mode: "0644"}
	m.BuildCommand = "go build ./... && go test ./..."
	return m, nil
}

// WriteManifest writes a manifest as indented JSON.
func WriteManifest(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// WriteSHA256SUMS writes a "sha256  path" file covering the given entries.
func WriteSHA256SUMS(path string, entries []FileEntry) error {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s  %s\n", e.SHA256, filepath.Base(e.Path))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// VerifySHA256SUMS re-hashes every listed file and reports mismatches.
func VerifySHA256SUMS(dir, sumsPath string) (int, error) {
	raw, err := os.ReadFile(sumsPath)
	if err != nil {
		return 0, err
	}
	checked := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return checked, fmt.Errorf("zbuild: malformed checksum line %q", line)
		}
		got, err := HashFile(filepath.Join(dir, parts[1]))
		if err != nil {
			return checked, err
		}
		if got != parts[0] {
			return checked, fmt.Errorf("zbuild: checksum mismatch for %s", parts[1])
		}
		checked++
	}
	return checked, nil
}

func goVersion(root string) string {
	cmd := exec.Command("go", "version")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
