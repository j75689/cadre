package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

var archiveStart = time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)

// archivedBody is the stored snapshot of u2: compact, with provider JSON
// in a transcript that an HTML-escaping encoder would rewrite.
const archivedBody = `{"format":1,"uid":"u2","namespace":"team-a","name":"b","number":2,"title":"b <i>","phase":"Done","archivedAt":"2026-11-01T06:00:00Z","archiveAfter":"1h0m0s","task":{"metadata":{"name":"b"}},"thread":[],"runs":[{"id":"r1","mode":"step"}],"events":{"r1":[]},"transcripts":{"r1":[{"step":0,"kind":"start","request":{"system":"a <b> & c"}}]},"files":[]}`

// archived is a store with team-a's snapshots of u1, u2 and u3, archived
// an hour apart in that order, u3 and u1 in the same second, and the body
// of u2.
func archived(t *testing.T) storage.Driver {
	t.Helper()
	store := memory.NewBackend().Open()
	for i, uid := range []string{"u1", "u2", "u3"} {
		at := archiveStart.Add(time.Duration(i) * time.Hour)
		sum, err := json.Marshal(manager.SnapshotSummary{Format: 1, UID: uid, Namespace: "team-a", Name: string(rune('a' + i)), Number: int64(i + 1),
			Title: uid, Phase: v1alpha1.PhaseDone})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(ctx, manager.ArchiveStream("team-a"), fmt.Sprintf("%020d-%s", at.UnixNano(), uid), sum); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Append(ctx, manager.SnapshotStream("team-a", "u2"), "snapshot", []byte(archivedBody)); err != nil {
		t.Fatal(err)
	}
	return store
}

// uids are the uids of an ArchiveList's items, in order.
func uids(t *testing.T, out string) string {
	t.Helper()
	var list api.ArchiveList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got []string
	for _, it := range list.Items {
		got = append(got, it.UID)
	}
	return fmt.Sprintf("%v %d/%d/%d", got, list.Offset, list.Limit, list.Total)
}

func TestArchiveList(t *testing.T) {
	e := startRolesWith(t, archived(t), nil, &authz.Policy{AdminUsers: []string{"root@example.test"}})
	viewer := e.as("eve@example.test", "readers")
	for _, c := range []struct {
		query string
		code  int
		says  string
	}{
		{"", 200, "[u3 u2 u1] 0/100/3"},
		{"?limit=2", 200, "[u3 u2] 0/2/3"},
		{"?offset=1&limit=1", 200, "[u2] 1/1/3"},
		{"?offset=2", 200, "[u1] 2/100/3"},
		{"?offset=3", 200, "[] 3/100/3"},
		{"?limit=9999", 200, "[u3 u2 u1] 0/500/3"},
		{"?limit=-1", 400, "limit must be a non-negative integer"},
		{"?offset=x", 400, "offset must be a non-negative integer"},
		{"?limit=1&limit=2", 400, "limit given 2 times"},
		{"?offset=1&offset=2", 400, "offset given 2 times"},
	} {
		code, out := e.do(viewer, "GET", "/namespaces/team-a/archive"+c.query, "")
		if code != c.code {
			t.Errorf("%q: %d %s, want %d", c.query, code, out, c.code)
			continue
		}
		if code == 200 {
			out = uids(t, out)
		}
		if !strings.Contains(out, c.says) {
			t.Errorf("%q: %s, want %q", c.query, out, c.says)
		}
	}
	if _, out := e.do(viewer, "GET", "/namespaces/team-a/archive?offset=3", ""); !strings.Contains(out, `"items":[]`) {
		t.Errorf("past the end: %s, want items []", out)
	}
	if _, out := e.do(viewer, "GET", "/namespaces/team-b/archive", ""); !strings.Contains(out, "no role there") {
		t.Errorf("team-b without a role: %s", out)
	}
	if _, out := e.do(e.as("root@example.test"), "GET", "/namespaces/team-b/archive", ""); out != `{"items":[],"offset":0,"limit":100,"total":0}`+"\n" {
		t.Errorf("a Team with no snapshots: %s", out)
	}
}

func TestArchiveGet(t *testing.T) {
	e := startRolesWith(t, archived(t), nil, &authz.Policy{AdminUsers: []string{"root@example.test"}})
	viewer := e.as("eve@example.test", "readers")
	// The stored bytes, then the newline every answer ends with.
	if code, out := e.do(viewer, "GET", "/namespaces/team-a/archive/u2", ""); code != 200 || out != archivedBody+"\n" {
		t.Errorf("%d %s, want the stored body verbatim", code, out)
	}
	for _, c := range []struct {
		name, path string
		code       int
		says       string
	}{
		{"an unknown uid", "/namespaces/team-a/archive/u9", 404, `"not_found"`},
		{"a listed uid without a body", "/namespaces/team-a/archive/u1", 404, `"not_found"`},
		{"a path", "/namespaces/team-a/archive/..%2F", 400, "letters, digits and dashes"},
		{"a dot", "/namespaces/team-a/archive/u.2", 400, "letters, digits and dashes"},
		{"65 characters", "/namespaces/team-a/archive/" + strings.Repeat("a", 65), 400, "letters, digits and dashes"},
	} {
		if code, out := e.do(viewer, "GET", c.path, ""); code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: %d %s, want %d %s", c.name, code, out, c.code, c.says)
		}
	}
	if code, out := e.do(e.as("root@example.test"), "GET", "/namespaces/Team_A/archive/u2", ""); code != 400 || !strings.Contains(out, `"invalid"`) {
		t.Errorf("a non-DNS namespace: %d %s, want 400 invalid", code, out)
	}
	if code, out := e.do(viewer, "GET", "/namespaces/team-a/archive/"+strings.Repeat("a", 64), ""); code != 404 {
		t.Errorf("64 characters: %d %s, want 404", code, out)
	}
}

// The archive is read as the board is: by its viewers and up, and without
// logins by anyone.
func TestArchiveByRole(t *testing.T) {
	e := startRolesWith(t, archived(t), nil, &authz.Policy{AdminUsers: []string{"Root@Example.test"}, FoldUsernames: true})
	viewer := e.as("eve@example.test", "readers")
	member := e.as("dana@example.test", "backend", "readers")
	root := e.as("root@example.test")
	for _, path := range []string{"/namespaces/team-a/archive", "/namespaces/team-a/archive/u2"} {
		for _, c := range []struct {
			name, token, path string
			code              int
			says              string
		}{
			{"no login", "", path, 401, `"unauthenticated"`},
			{"a viewer", viewer, path, 200, `"uid":"u2"`},
			{"a member", member, path, 200, `"uid":"u2"`},
			{"an admin", e.as("lead@example.test"), path, 200, `"uid":"u2"`},
			{"a viewer AgentAccess", "root.agent", path, 200, `"uid":"u2"`},
			{"a member AgentAccess", "bot.member", path, 200, `"uid":"u2"`},
			{"a stranger", e.as("mallory@example.test", "elsewhere"), path, 403, `"no_role"`},
			{"a namespace without a Team", viewer, strings.Replace(path, "team-a", "team-c", 1), 403, `"no_role"`},
			{"a bootstrap admin without a Team", root, strings.Replace(path, "team-a", "team-c", 1), 404, `"not_found"`},
		} {
			if code, out := e.do(c.token, "GET", c.path, ""); code != c.code || !strings.Contains(out, c.says) {
				t.Errorf("%s %s: %d %s, want %d %s", c.name, c.path, code, out, c.code, c.says)
			}
		}
	}
	m := manager.New(archived(t), manager.Config{})
	if _, err := m.InitTeam(ctx, "team-a", v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithAccess(auth.ModeNone, nil)).Register(mux)
	for _, path := range []string{"/namespaces/team-a/archive", "/namespaces/team-a/archive/u2", "/namespaces/team-c/archive"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, api.Base+path, nil))
		want := 200
		if strings.Contains(path, "team-c") {
			want = 404
		}
		if w.Code != want {
			t.Errorf("--auth=none %s: %d %s, want %d", path, w.Code, w.Body, want)
		}
	}
}
