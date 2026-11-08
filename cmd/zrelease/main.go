// Command zrelease builds a deterministic source package and its manifests.
//
//	zrelease --version v1.0.0 --out releases
//
// It refuses to package a dirty working tree unless --allow-dirty is given, so a
// package always corresponds to a commit.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"znetwork/internal/zbuild"
)

func main() {
	version := flag.String("version", "", "release version, e.g. v1.0.0")
	out := flag.String("out", "releases", "output directory")
	allowDirty := flag.Bool("allow-dirty", false, "package even if the working tree has changes")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "zrelease: --version is required")
		os.Exit(2)
	}
	if !*allowDirty {
		if out2, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
			if len(strings.TrimSpace(string(out2))) != 0 {
				fmt.Fprintln(os.Stderr, "zrelease: working tree is dirty; commit first or pass --allow-dirty")
				os.Exit(1)
			}
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "zrelease:", err)
		os.Exit(1)
	}
	name := fmt.Sprintf("znetwork-%s.zip", *version)
	archive := filepath.Join(*out, name)
	m, err := zbuild.Create(".", archive, *version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zrelease: packaging:", err)
		os.Exit(1)
	}
	manifestPath := filepath.Join(*out, strings.TrimSuffix(name, ".zip")+".manifest.json")
	if err := zbuild.WriteManifest(manifestPath, m); err != nil {
		fmt.Fprintln(os.Stderr, "zrelease: manifest:", err)
		os.Exit(1)
	}
	mh, err := zbuild.HashFile(manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zrelease:", err)
		os.Exit(1)
	}
	sums := filepath.Join(*out, "SHA256SUMS")
	entries := []zbuild.FileEntry{m.Archive, {Path: manifestPath, SHA256: mh}}
	if err := zbuild.WriteSHA256SUMS(sums, entries); err != nil {
		fmt.Fprintln(os.Stderr, "zrelease: sums:", err)
		os.Exit(1)
	}
	n, err := zbuild.VerifySHA256SUMS(*out, sums)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zrelease: verification failed:", err)
		os.Exit(1)
	}
	fmt.Printf("package   %s (%d bytes)\n", archive, m.Archive.Size)
	fmt.Printf("sha256    %s\n", m.Archive.SHA256)
	fmt.Printf("manifest  %s\n", manifestPath)
	fmt.Printf("files     %d (%d source bytes)\n", m.FileCount, m.TotalBytes)
	fmt.Printf("verified  %d checksums against disk\n", n)
}
