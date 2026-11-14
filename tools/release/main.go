// Command release builds rradmon for every supported platform and packs
// each build into an archive in dist/, with a SHA256SUMS file.
//
//	go run ./tools/release                 # version from git describe
//	go run ./tools/release -version 0.4.0  # or set it
//	go run ./tools/release -targets linux/arm64,windows/amd64
//
// Builds are pure Go (CGO_ENABLED=0), so every target cross-compiles from
// any machine with Go installed.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type target struct {
	os, arch, arm string // arm: GOARM for linux/arm
	name          string // platform part of the archive name
}

var targets = []target{
	{os: "windows", arch: "amd64", name: "windows-amd64"},
	{os: "windows", arch: "arm64", name: "windows-arm64"},
	{os: "linux", arch: "amd64", name: "linux-amd64"},
	{os: "linux", arch: "arm64", name: "linux-arm64"},         // Raspberry Pi 3/4/5 on a 64-bit OS
	{os: "linux", arch: "arm", arm: "6", name: "linux-armv6"}, // any Raspberry Pi on a 32-bit OS, Pi Zero included
	{os: "darwin", arch: "amd64", name: "macos-amd64"},        // Intel
	{os: "darwin", arch: "arm64", name: "macos-arm64"},        // Apple silicon
}

// Files packed next to the binary
var extras = []string{"README.md", "LICENSE"}

func main() {
	version := flag.String("version", "", "version to stamp (default: git describe)")
	only := flag.String("targets", "", "comma-separated os/arch list (default: all)")
	out := flag.String("out", "dist", "output directory")
	flag.Parse()
	if err := run(*version, *only, *out); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(version, only, out string) error {
	if _, err := os.Stat("cmd/rradmon"); err != nil {
		return fmt.Errorf("run this from the repository root")
	}
	if version == "" {
		v, err := git("describe", "--tags", "--always", "--dirty")
		if err != nil {
			return fmt.Errorf("git describe: %v (or pass -version)", err)
		}
		version = strings.TrimPrefix(v, "v")
	}
	if strings.ContainsAny(version, ` /\"'`) {
		return fmt.Errorf("bad version %q", version)
	}
	if strings.HasSuffix(version, "-dirty") {
		fmt.Fprintln(os.Stderr, "release: warning: the working tree has uncommitted changes")
	}
	// Archive entries carry the commit time, so the same commit packs the same way
	stamp := time.Now()
	if s, err := git("log", "-1", "--format=%ct"); err == nil {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			stamp = time.Unix(n, 0)
		}
	}

	sel := targets
	if only != "" {
		sel = nil
		for _, want := range strings.Split(only, ",") {
			found := false
			for _, t := range targets {
				if t.os+"/"+t.arch == strings.TrimSpace(want) || t.name == strings.TrimSpace(want) {
					sel, found = append(sel, t), true
				}
			}
			if !found {
				return fmt.Errorf("unknown target %q", want)
			}
		}
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "rradmon-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	sums := map[string]string{}
	for _, t := range sel {
		base := "rradmon-" + version + "-" + t.name
		bin := "rradmon"
		if t.os == "windows" {
			bin += ".exe"
		}
		binPath := filepath.Join(work, base, bin)
		fmt.Printf("%-14s ", t.name)
		cmd := exec.Command("go", "build", "-trimpath",
			"-ldflags", "-s -w -buildid= -X main.version="+version,
			"-o", binPath, "./cmd/rradmon")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+t.os, "GOARCH="+t.arch, "GOARM="+t.arm)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %v", t.name, err)
		}
		files := []entry{{name: base + "/" + bin, src: binPath, exec: true}}
		for _, x := range extras {
			files = append(files, entry{name: base + "/" + x, src: x})
		}
		var archive string
		if t.os == "windows" {
			archive = filepath.Join(out, base+".zip")
			err = writeZip(archive, files, stamp)
		} else {
			archive = filepath.Join(out, base+".tar.gz")
			err = writeTarGz(archive, files, stamp)
		}
		if err != nil {
			return err
		}
		sum, size, err := hashFile(archive)
		if err != nil {
			return err
		}
		sums[filepath.Base(archive)] = sum
		fmt.Printf("%s  %.1f MB\n", filepath.Base(archive), float64(size)/1e6)
	}

	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[n], n)
	}
	sumFile := filepath.Join(out, "rradmon-"+version+"-SHA256SUMS")
	if err := os.WriteFile(sumFile, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", sumFile)
	return nil
}

type entry struct {
	name, src string
	exec      bool
}

func writeZip(path string, files []entry, stamp time.Time) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, e := range files {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: stamp}
		mode := os.FileMode(0o644)
		if e.exec {
			mode = 0o755
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			f.Close()
			return err
		}
		if err := copyFile(w, e.src); err != nil {
			f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeTarGz(path string, files []entry, stamp time.Time) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range files {
		st, err := os.Stat(e.src)
		if err != nil {
			f.Close()
			return err
		}
		mode := int64(0o644)
		if e.exec {
			mode = 0o755
		}
		h := &tar.Header{Name: e.name, Mode: mode, Size: st.Size(), ModTime: stamp, Typeflag: tar.TypeReg,
			Uname: "root", Gname: "root", Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			f.Close()
			return err
		}
		if err := copyFile(tw, e.src); err != nil {
			f.Close()
			return err
		}
	}
	for _, c := range []io.Closer{tw, gz} {
		if err := c.Close(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

func copyFile(w io.Writer, src string) error {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil)), n, err
}

func git(args ...string) (string, error) {
	b, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(b)), err
}
