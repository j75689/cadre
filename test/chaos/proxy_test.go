package chaos_test

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The proxy says nothing of a response a kill cut short, and logs any other
// error it meets relaying one.
func TestProxyErrorLog(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		logged     bool
	}{
		// What a killed instance leaves of a watch stream.
		{"cut", "5\r\nhello\r\n", false},
		{"malformed", "zz\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"+tc.body)
			}))
			defer backend.Close()
			var mu sync.Mutex
			var lines []string
			px := newProxy(&lockedRand{r: rand.New(rand.NewPCG(1, 1))}, func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, fmt.Sprintf(format, args...))
			})
			px.set("server-1", backend.URL)
			// The proxy aborts the response either way: what reaches the
			// client is beside the point.
			if res, err := http.Get(px.URL); err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			// Close waits for the proxy's handler, and its log.
			px.Close()
			mu.Lock()
			defer mu.Unlock()
			if logged := len(lines) == 1 && strings.HasPrefix(lines[0], "proxy: httputil: ReverseProxy read error during body copy: "); logged != tc.logged || len(lines) > 1 {
				t.Errorf("logged %q, want a line: %v", lines, tc.logged)
			}
		})
	}
}
