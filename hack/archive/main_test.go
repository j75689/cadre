package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tree writes a small CLI tree, an executable and a licence, and returns its
// entries in the order given, which need not be sorted.
func tree(t *testing.T) []entry {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "cadre")
	lic := filepath.Join(dir, "LICENSE")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho cadre\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lic, []byte("Apache License\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := parseEntries([]string{"cadre=" + bin, "LICENSE=" + lic})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// touch moves every entry's file times, which must not reach the archive.
func touch(t *testing.T, entries []entry, at time.Time) {
	t.Helper()
	for _, e := range entries {
		if err := os.Chtimes(e.path, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSameTreeSameBytes(t *testing.T) {
	mtime := time.Unix(1700000000, 0)
	for _, out := range []string{"cadre.tar.gz", "cadre.zip"} {
		t.Run(out, func(t *testing.T) {
			entries := tree(t)
			var first, second bytes.Buffer
			touch(t, entries, time.Unix(1, 0))
			if err := writeArchive(&first, out, entries, mtime); err != nil {
				t.Fatal(err)
			}
			touch(t, entries, time.Now())
			if err := writeArchive(&second, out, entries, mtime); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Bytes(), second.Bytes()) {
				t.Fatalf("two archives of the same tree differ: %x vs %x", sha256.Sum256(first.Bytes()), sha256.Sum256(second.Bytes()))
			}
		})
	}
}

func TestTarGzEntries(t *testing.T) {
	mtime := time.Unix(1700000000, 0)
	var buf bytes.Buffer
	if err := writeArchive(&buf, "x.tar.gz", tree(t), mtime); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header carries name %q and time %v; want neither", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	var got []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %o %d:%d %q:%q %d", h.Name, h.Mode, h.Uid, h.Gid, h.Uname, h.Gname, h.ModTime.Unix()))
	}
	want := []string{
		`LICENSE 644 0:0 "":"" 1700000000`,
		`cadre 755 0:0 "":"" 1700000000`,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("entries:\n got %q\nwant %q", got, want)
	}
}

func TestZipEntries(t *testing.T) {
	mtime := time.Unix(1700000000, 0)
	var buf bytes.Buffer
	if err := writeArchive(&buf, "x.zip", tree(t), mtime); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range zr.File {
		got = append(got, fmt.Sprintf("%s %v %d", f.Name, f.Mode(), f.Modified.Unix()))
	}
	want := []string{"LICENSE -rw-r--r-- 1700000000", "cadre -rwxr-xr-x 1700000000"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("entries:\n got %q\nwant %q", got, want)
	}
}

func TestSums(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	b := write("cadre_1.0.0_linux_amd64.tar.gz", "b")
	a := write("cadre_1.0.0_darwin_arm64.tar.gz", "a")
	var buf bytes.Buffer
	if err := writeSums(&buf, []string{b, a}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x  cadre_1.0.0_darwin_arm64.tar.gz\n%x  cadre_1.0.0_linux_amd64.tar.gz\n", sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b")))
	if buf.String() != want {
		t.Errorf("sums:\n got %q\nwant %q", buf.String(), want)
	}
}

func TestRefusals(t *testing.T) {
	if _, err := parseEntries([]string{"cadre"}); err == nil {
		t.Error("an entry without NAME=PATH was taken")
	}
	if err := writeArchive(io.Discard, "x.tar.xz", tree(t), time.Unix(0, 0)); err == nil {
		t.Error("an unknown format was written")
	}
	if err := writeArchive(io.Discard, "x.tar.gz", []entry{{"d", t.TempDir()}}, time.Unix(0, 0)); err == nil {
		t.Error("a directory was archived as a file")
	}
}
