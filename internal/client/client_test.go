package client

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The token file is read before every request, so a rotated projected
// token is picked up at once.
func TestTokenFileIsReadEveryTime(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "token")
	c := &http.Client{Transport: TokenFile(path, nil)}
	for _, tok := range []string{"first\n", "second"} {
		if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatalf("sent %v", seen)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("a missing token file must fail the request, not send it bare")
	}
}

func TestRunnerHTTP(t *testing.T) {
	if hc, ok := RunnerHTTP(""); ok || hc != nil {
		if _, err := os.Stat(DefaultTokenFile); err != nil {
			t.Fatal("without a token file a runner sends no token")
		}
	}
	if hc, ok := RunnerHTTP(filepath.Join(t.TempDir(), "token")); !ok || hc == nil {
		t.Fatal("a token file named outright is used")
	}
}
