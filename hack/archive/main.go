// Command archive writes the cadre CLI's release archives so that the same
// files give the same bytes on any host: entries sorted by name, owned by
// 0:0, every one with the same modification time, and a gzip header with
// no name and no time. It also writes the checksums file.
//
//	go run ./hack/archive -o cadre_1.2.3_linux_amd64.tar.gz -mtime 1700000000 cadre=bin/cadre LICENSE=LICENSE
//	go run ./hack/archive -sums cadre_1.2.3_checksums.txt dist/cadre_1.2.3_*
//
// An archive is a .tar.gz or a .zip, by the name given to -o; each entry is
// NAME=PATH, the name in the archive and the file it is read from. An
// executable file is 0755 in the archive, any other 0644.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	out := flag.String("o", "", "the archive to write, a .tar.gz or a .zip")
	mtime := flag.Int64("mtime", 0, "every entry's modification time, in seconds since the epoch (SOURCE_DATE_EPOCH)")
	sums := flag.String("sums", "", "write sha256sum lines for the files given into this file, instead of an archive")
	flag.Parse()
	var err error
	switch {
	case *sums != "" && *out == "":
		err = writeFile(*sums, func(w io.Writer) error { return writeSums(w, flag.Args()) })
	case *out != "" && *sums == "":
		var entries []entry
		if entries, err = parseEntries(flag.Args()); err == nil {
			err = writeFile(*out, func(w io.Writer) error { return writeArchive(w, *out, entries, time.Unix(*mtime, 0)) })
		}
	default:
		err = errors.New("give -o or -sums")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "archive:", err)
		os.Exit(1)
	}
}

// entry is a file in an archive: its name there, and where it is read from.
type entry struct {
	name, path string
}

func parseEntries(args []string) ([]entry, error) {
	if len(args) == 0 {
		return nil, errors.New("no files to archive")
	}
	entries := make([]entry, 0, len(args))
	for _, a := range args {
		name, path, ok := strings.Cut(a, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("%q: want NAME=PATH", a)
		}
		entries = append(entries, entry{name, path})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

// writeFile writes a file whole, or not at all.
func writeFile(path string, write func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeArchive writes entries, already sorted, as a .tar.gz or a .zip by the
// name out.
func writeArchive(w io.Writer, out string, entries []entry, mtime time.Time) error {
	switch {
	case strings.HasSuffix(out, ".tar.gz"):
		return writeTarGz(w, entries, mtime)
	case strings.HasSuffix(out, ".zip"):
		return writeZip(w, entries, mtime)
	}
	return fmt.Errorf("%s: want a .tar.gz or a .zip", out)
}

// read is an entry's content and its mode in the archive.
func read(e entry) ([]byte, int64, error) {
	fi, err := os.Stat(e.path)
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s: not a regular file", e.path)
	}
	data, err := os.ReadFile(e.path)
	mode := int64(0o644)
	if fi.Mode()&0o111 != 0 {
		mode = 0o755
	}
	return data, mode, err
}

func writeTarGz(w io.Writer, entries []entry, mtime time.Time) error {
	// The zero header: no name, no time, the same OS byte everywhere.
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		data, mode, err := read(e)
		if err != nil {
			return err
		}
		h := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Mode:     mode,
			Size:     int64(len(data)),
			ModTime:  mtime,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(w io.Writer, entries []entry, mtime time.Time) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, flate.BestCompression)
	})
	for _, e := range entries {
		data, mode, err := read(e)
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: mtime.UTC()}
		h.SetMode(os.FileMode(mode))
		f, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
	}
	return zw.Close()
}

// writeSums writes a line per file, "<sha256>  <base name>", sorted by name,
// as sha256sum does, so sha256sum -c and shasum -a 256 -c read it.
func writeSums(w io.Writer, paths []string) error {
	if len(paths) == 0 {
		return errors.New("no files to sum")
	}
	sorted := append([]string(nil), paths...)
	sort.Slice(sorted, func(i, j int) bool { return filepath.Base(sorted[i]) < filepath.Base(sorted[j]) })
	for _, p := range sorted {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(data), filepath.Base(p)); err != nil {
			return err
		}
	}
	return nil
}
