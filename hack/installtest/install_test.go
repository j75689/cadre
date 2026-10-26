// Package installtest checks hack/install.sh against a release made by make
// cli-dist for this host and served locally.
package installtest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const release = "v0.0.0-installtest"

// served builds the CLI's release for this host into a directory, serves
// it as GitHub does, and returns the server's URL and the release's
// directory. The files are under /download/<tag>/ (CADRE_BASE_URL), and
// /latest redirects to /tag/<tag> (CADRE_LATEST_URL).
func served(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, release)
	cmd := exec.Command("make", "-s", "cli-dist", "RELEASE="+release, "CLI_PLATFORMS="+runtime.GOOS+"/"+runtime.GOARCH, "CLI_DIST="+dist)
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make cli-dist: %v\n%s", err, out)
	}
	mux := http.NewServeMux()
	mux.Handle("/download/", http.StripPrefix("/download", http.FileServer(http.Dir(root))))
	mux.Handle("/latest", http.RedirectHandler("/tag/"+release, http.StatusFound))
	mux.HandleFunc("/tag/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a release page")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, dist
}

// install runs install.sh into a fresh directory and returns it, its
// output and its error. version is CADRE_VERSION; "" leaves it unset, so
// the script looks for the latest release. env is added to the
// environment, after the rest.
func install(t *testing.T, srv, version string, env ...string) (string, string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("sh", filepath.Join("..", "install.sh"))
	cmd.Env = append(os.Environ(),
		"CADRE_BASE_URL="+srv+"/download",
		"CADRE_LATEST_URL="+srv+"/latest",
		"CADRE_INSTALL_DIR="+dir,
		"HOME="+t.TempDir(),
	)
	if version != "" {
		cmd.Env = append(cmd.Env, "CADRE_VERSION="+version)
	}
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return dir, out.String(), err
}

func TestInstall(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh installs on linux and darwin")
	}
	for _, tool := range []string{"sh", "make", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	base, dist := served(t)
	sums := filepath.Join(dist, "cadre_"+strings.TrimPrefix(release, "v")+"_checksums.txt")
	good, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}

	installs := func(t *testing.T, version string, env ...string) {
		dir, out, err := install(t, base, version, env...)
		if err != nil {
			t.Fatalf("install.sh: %v\n%s", err, out)
		}
		if !strings.Contains(out, "downloading cadre "+release+" ") {
			t.Errorf("install.sh did not download %s:\n%s", release, out)
		}
		if !strings.Contains(out, release+"\n") {
			t.Errorf("install.sh did not end with cadre version %s:\n%s", release, out)
		}
		if !strings.Contains(out, "is not on your PATH") {
			t.Errorf("install.sh did not say %s is not on PATH:\n%s", dir, out)
		}
		got, err := exec.Command(filepath.Join(dir, "cadre"), "version").Output()
		if err != nil || string(got) != release+"\n" {
			t.Errorf("installed cadre version: %q, %v", got, err)
		}
		staged, _ := filepath.Glob(filepath.Join(dir, ".cadre.*"))
		if len(staged) > 0 {
			t.Errorf("install.sh left %v behind", staged)
		}
	}
	t.Run("installs the version asked for", func(t *testing.T) { installs(t, strings.TrimPrefix(release, "v")) })
	t.Run("installs the latest release, found from its redirect", func(t *testing.T) { installs(t, "") })
	t.Run("installs the latest release with wget, where there is no curl", func(t *testing.T) {
		installs(t, "", "PATH="+withoutCurl(t))
	})

	refused := func(t *testing.T, sumsFile []byte, says string) {
		t.Helper()
		if err := os.WriteFile(sums, sumsFile, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(sums, good, 0o644) })
		dir, out, err := install(t, base, release)
		if err == nil {
			t.Fatalf("install.sh installed, where it should have said %q:\n%s", says, out)
		}
		if !strings.Contains(out, says) || !strings.Contains(out, "not installing") {
			t.Errorf("install.sh did not refuse saying %q:\n%s", says, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "cadre")); !os.IsNotExist(err) {
			t.Errorf("cadre is in %s after a refusal: %v", dir, err)
		}
	}

	t.Run("refuses a checksum that does not match", func(t *testing.T) {
		line := strings.SplitN(string(good), " ", 2)
		bad := []byte(strings.Repeat("0", len(line[0])) + " " + line[1])
		refused(t, bad, "does not match")
	})

	t.Run("refuses an archive the checksums do not list", func(t *testing.T) {
		refused(t, []byte(strings.Repeat("0", 64)+"  cadre_other.tar.gz\n"), "lists no checksum")
	})
}

// withoutCurl is a PATH of the tools install.sh uses, wget among them but
// not curl, so that its wget branch runs. Without wget, the test is
// skipped.
func withoutCurl(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("no wget")
	}
	dir := t.TempDir()
	tools := []string{"sh", "wget", "tar", "gzip", "sha256sum", "shasum", "awk", "sed", "tr", "tail", "uname", "mktemp", "rm", "cp", "chmod", "mv", "mkdir"}
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			// One of sha256sum and shasum is enough, and a tar that
			// reads gzip itself needs no gzip.
			continue
		}
		if err := os.Symlink(path, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
