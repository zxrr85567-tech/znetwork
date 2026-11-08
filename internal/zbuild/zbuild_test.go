package zbuild

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the packaging rules against a synthetic git repository so
// they do not depend on the state of the real working tree.

func initRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "x"},
	} {
		cmd := gitCmd(root, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func TestTrackedFilesSkipsExcluded(t *testing.T) {
	root := initRepo(t, map[string]string{
		"a.go":               "package a",
		"dir/b.go":           "package b",
		"data/chain.log":     "x",
		"bin/znd":            "x",
		"releases/v1.zip":    "x",
		"debug.log":          "x",
		"validator.key.json": "{}",
	})
	files, err := TrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, ",")
	for _, banned := range []string{"data/", "bin/", ".zip", ".log", "key.json"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("excluded %q is packaged: %v", banned, files)
		}
	}
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	if files[0] != "a.go" || files[1] != "dir/b.go" {
		t.Fatalf("files not sorted: %v", files)
	}
}

func TestCreateIsDeterministic(t *testing.T) {
	root := initRepo(t, map[string]string{
		"a.go":      "package a\n",
		"b/c.txt":   strings.Repeat("data ", 1000),
		"README.md": "# x\n",
	})
	out1 := filepath.Join(t.TempDir(), "one.zip")
	out2 := filepath.Join(t.TempDir(), "two.zip")
	m1, err := Create(root, out1, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	// A second package built later must be byte-identical: same content, same
	// normalised timestamps, same order.
	if _, err := Create(root, out2, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if m1.Archive.SHA256 == "" {
		t.Fatal("archive hash not recorded")
	}
	h2, err := HashFile(out2)
	if err != nil {
		t.Fatal(err)
	}
	if h2 != m1.Archive.SHA256 {
		t.Fatalf("packages differ: %s vs %s", m1.Archive.SHA256, h2)
	}
	if m1.FileCount != 3 || len(m1.Files) != 3 {
		t.Fatalf("manifest files = %d", m1.FileCount)
	}
	// Every file hash in the manifest must match the file on disk.
	for _, f := range m1.Files {
		got, err := HashFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if got != f.SHA256 {
			t.Fatalf("%s: manifest hash %s, disk hash %s", f.Path, f.SHA256, got)
		}
	}
}

func TestCreateArchiveContents(t *testing.T) {
	root := initRepo(t, map[string]string{"x/y.go": "package y\n"})
	out := filepath.Join(t.TempDir(), "pkg.zip")
	m, err := Create(root, out, "v9")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != 1 || zr.File[0].Name != "x/y.go" {
		t.Fatalf("entries = %v", zr.File)
	}
	// Timestamps are normalised so builds on different dates match.
	if !zr.File[0].Modified.Equal(fixedModTime) {
		t.Fatalf("entry time = %v, want %v", zr.File[0].Modified, fixedModTime)
	}
	if m.Package != "pkg.zip" || m.Version != "v9" || m.ModulePath != "znetwork" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Archive.Size != fileSize(t, out) {
		t.Fatal("archive size mismatch")
	}
}

func TestManifestAndSumsRoundTrip(t *testing.T) {
	root := initRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	dir := t.TempDir()
	archive := filepath.Join(dir, "znetwork.zip")
	m, err := Create(root, archive, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	// SHA256SUMS lists artifacts stored beside it: the archive plus the manifest.
	if err := WriteSHA256SUMS(filepath.Join(dir, "SHA256SUMS"),
		[]FileEntry{m.Archive, {Path: "manifest.json", SHA256: mustHash(t, filepath.Join(dir, "manifest.json"))}}); err != nil {
		t.Fatal(err)
	}
	n, err := VerifySHA256SUMS(dir, filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if n != 2 {
		t.Fatalf("verified %d files, want 2", n)
	}
	// Corruption must be detected.
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySHA256SUMS(dir, filepath.Join(dir, "SHA256SUMS")); err == nil {
		t.Fatal("tampered file passed verification")
	}
}

func TestCreateRejectsEmptyRepo(t *testing.T) {
	root := t.TempDir()
	cmd := gitCmd(root, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if _, err := Create(root, filepath.Join(t.TempDir(), "x.zip"), "v0"); err == nil {
		t.Fatal("empty repository produced a package")
	}
}

func mustHash(t *testing.T, p string) string {
	t.Helper()
	h, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}
