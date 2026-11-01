package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

// filterTasks are the board the filter tests list, in board order: one
// priority, so by number. done ones are cancelled.
var filterTasks = []struct {
	name, title string
	assignee    *v1alpha1.Assignee
	labels      []string
	done        bool
}{
	{"login-bug", "Fix login", &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}, []string{"bug", "auth"}, false},
	{"signup", "Signup flow", &v1alpha1.Assignee{Kind: v1alpha1.AssigneeUser, Name: "alice"}, []string{"feature", "auth"}, false},
	{"docs", "Write docs", &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}, []string{"docs"}, true},
	{"coder-guide", "Guide for new hires", nil, []string{"docs", "bug"}, false},
	{"shout", "LOGIN page copy", &v1alpha1.Assignee{Kind: v1alpha1.AssigneeUser, Name: "Alice"}, nil, true},
}

func TestListFilters(t *testing.T) {
	listFilters(t, memory.NewBackend().Open(), nil)
}

func TestListFiltersOnTheKubernetesDriver(t *testing.T) {
	cfg := testenv.APIServer(t)
	admin, err := ctrlclient.New(cfg, ctrlclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	drv, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	// The index lags the store: wait until it has the board as seeded.
	listFilters(t, drv, func(m *manager.Manager) {
		q := storage.Query{Namespace: ns, Phases: []v1alpha1.Phase{v1alpha1.PhaseCancelled}}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			all, err1 := m.CountTasks(ctx, storage.Query{Namespace: ns})
			done, err2 := m.CountTasks(ctx, q)
			if err1 == nil && err2 == nil && all == len(filterTasks) && done == 2 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("index has %d tasks, %d cancelled (%v, %v)", all, done, err1, err2)
			}
		}
	})
}

// listFilters seeds filterTasks on store and lists them through the API
// with each filter alone, with phases, with paging and by number.
func listFilters(t *testing.T, store storage.Driver, settle func(*manager.Manager)) {
	t.Helper()
	m := manager.New(store, manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude")})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	for _, tk := range filterTasks {
		spec := v1alpha1.TaskSpec{Title: tk.title, Assignee: tk.assignee, Labels: tk.labels, Description: "secret plans"}
		if _, err := m.CreateTask(ctx, ns, tk.name, spec); err != nil {
			t.Fatal(err)
		}
		if tk.done {
			if _, err := m.Cancel(ctx, storage.Key{Namespace: ns, Name: tk.name}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if settle != nil {
		settle(m)
	}

	for _, tc := range []struct {
		query string
		want  string
		total int
	}{
		{"", "login-bug signup docs coder-guide shout", 5},
		{"assignee=&q=", "login-bug signup docs coder-guide shout", 5},
		{"q=%20%20", "login-bug signup docs coder-guide shout", 5},
		// assignee: exact, either kind.
		{"assignee=coder", "login-bug docs", 2},
		{"assignee=alice", "signup", 1},
		{"assignee=ali", "", 0},
		{"assignee=coder&phase=Todo", "login-bug", 1},
		{"assignee=coder&phase=Todo&phase=Cancelled", "login-bug docs", 2},
		// label: every one, exact.
		{"label=auth", "login-bug signup", 2},
		{"label=auth&label=bug", "login-bug", 1},
		{"label=bug&label=bug", "login-bug coder-guide", 2},
		{"label=au", "", 0},
		{"label=docs&phase=Cancelled", "docs", 1},
		{"label=docs&phase=Todo&phase=Cancelled", "docs coder-guide", 2},
		// q: trimmed, any case, over title, name, assignee, labels and
		// #number, never the description.
		{"q=login", "login-bug shout", 2},
		{"q=%20%20Coder%20", "login-bug docs coder-guide", 3},
		{"q=%233", "docs", 1},
		{"q=AUTH", "login-bug signup", 2},
		{"q=hires", "coder-guide", 1},
		{"q=secret", "", 0},
		{"q=login&phase=Todo", "login-bug", 1},
		{"q=login&phase=Todo&phase=Cancelled", "login-bug shout", 2},
		// All of them.
		{"assignee=coder&label=docs&q=write", "docs", 1},
		{"assignee=coder&label=docs&q=fix", "", 0},
		// total is the whole selection, whatever the page.
		{"label=auth&limit=1", "login-bug", 2},
		{"label=auth&offset=1&limit=1", "signup", 2},
		{"label=auth&offset=5", "", 2},
		{"q=o&phase=Todo&phase=Cancelled&offset=1&limit=2", "signup docs", 5},
		{"assignee=coder&phase=Cancelled&limit=1", "docs", 1},
		// By number, the other filters apply to the one task.
		{"number=3&label=docs", "docs", 1},
		{"number=3&label=bug", "", 0},
		{"number=3&assignee=coder&q=write", "docs", 1},
		{"number=3&q=fix", "", 0},
		{"number=3&offset=1", "", 1},
	} {
		res, err := http.Get(srv.URL + api.Base + "/namespaces/" + ns + "/tasks?" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var list api.TaskList
		err = json.NewDecoder(res.Body).Decode(&list)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d, %v", tc.query, res.StatusCode, err)
			continue
		}
		var got []string
		for _, tk := range list.Items {
			got = append(got, tk.Name)
		}
		if strings.Join(got, " ") != tc.want || list.Total != tc.total {
			t.Errorf("%s: %v, total %d; want %q, total %d", tc.query, got, list.Total, tc.want, tc.total)
		}
	}

	// The Go client sends each filter.
	c := client.New(srv.URL, nil)
	for _, tc := range []struct {
		q    client.TaskQuery
		want string
	}{
		{client.TaskQuery{Assignee: "alice"}, "signup"},
		{client.TaskQuery{Labels: []string{"auth", "bug"}}, "login-bug"},
		{client.TaskQuery{Text: "HIRES"}, "coder-guide"},
		{client.TaskQuery{Assignee: "coder", Labels: []string{"docs"}, Text: "write", Phases: []v1alpha1.Phase{v1alpha1.PhaseCancelled}}, "docs"},
	} {
		list, err := c.ListTasks(ctx, ns, tc.q)
		if err != nil || len(list.Items) != 1 || list.Items[0].Name != tc.want || list.Total != 1 {
			t.Errorf("client %+v: %+v, %v; want %s", tc.q, list, err, tc.want)
		}
	}
}

// What a list may not ask is a 400 invalid, saying why.
func TestListFiltersRefused(t *testing.T) {
	_, m, base := startURL(t)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	labels := func(n int) string {
		var ls []string
		for i := range n {
			ls = append(ls, "label=l"+string(rune('a'+i)))
		}
		return strings.Join(ls, "&")
	}
	// é is two bytes: the bound is on characters.
	long := url.QueryEscape(strings.Repeat("é", api.MaxTextRunes))
	for _, tc := range []struct {
		query string
		says  string // empty: the list is served
	}{
		{"assignee=a&assignee=b", "assignee given 2 times: at most once"},
		{"assignee=a&assignee=", "assignee given 2 times: at most once"},
		{"q=a&q=b", "q given 2 times: at most once"},
		{"q=" + long + "e", "q is 201 characters: at most 200"},
		{"q=" + long, ""},
		{"q=%20" + long + "%20", ""},
		{labels(api.MaxLabels + 1), "label given 11 times: at most 10"},
		{labels(api.MaxLabels), ""},
		{"label=", "label must not be empty"},
		{"label=a&label=", "label must not be empty"},
	} {
		res, err := http.Get(base + api.Base + "/namespaces/" + ns + "/tasks?" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var body httpjson.ErrorResponse
		_ = json.NewDecoder(res.Body).Decode(&body)
		_ = res.Body.Close()
		switch {
		case tc.says == "" && res.StatusCode != http.StatusOK:
			t.Errorf("%.40s: %d %+v, want 200", tc.query, res.StatusCode, body)
		case tc.says != "" && (res.StatusCode != http.StatusBadRequest || body.Error.Code != httpjson.CodeInvalid || body.Error.Message != tc.says):
			t.Errorf("%.40s: %d %+v, want 400 invalid %q", tc.query, res.StatusCode, body, tc.says)
		}
	}
}

// Every role that reads a board, and a server without logins, gets the
// same items and total for a filtered list.
func TestListFiltersAreTheSameForEveryRole(t *testing.T) {
	store := memory.NewBackend().Open()
	e := startRolesOn(t, store, nil)
	member := e.as("dana@example.test", "backend")
	for _, spec := range []string{
		`{"name":"a1","spec":{"title":"Fix login","labels":["auth","bug"],"assignee":{"kind":"User","name":"dana@example.test"}}}`,
		`{"name":"a2","spec":{"title":"Login copy","labels":["auth"]}}`,
		`{"name":"a3","spec":{"title":"Other","labels":["auth"],"priority":"urgent"}}`,
	} {
		if code, out := e.do(member, "POST", "/namespaces/team-a/tasks", spec); code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, out)
		}
	}
	mux := http.NewServeMux()
	api.NewServer(manager.New(store, manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	open := httptest.NewServer(mux)
	t.Cleanup(open.Close)

	callers := map[string]string{
		"viewer":             e.as("eve@example.test", "readers"),
		"member":             member,
		"admin":              e.as("lead@example.test"),
		"bootstrap admin":    e.as("root@example.test"),
		"member AgentAccess": "bot.member",
		"viewer AgentAccess": "root.agent",
	}
	for _, query := range []string{"label=auth&q=login&limit=1", "label=auth&offset=1", "assignee=dana@example.test", "q=x"} {
		path := "/namespaces/team-a/tasks?" + query
		res, err := http.Get(open.URL + api.Base + path)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		var list api.TaskList
		if err := json.Unmarshal(want, &list); err != nil || res.StatusCode != http.StatusOK || list.Total == 0 {
			t.Fatalf("%s without logins: %d %s", query, res.StatusCode, want)
		}
		for _, who := range slices.Sorted(maps.Keys(callers)) {
			if code, out := e.do(callers[who], "GET", path, ""); code != http.StatusOK || out != string(want) {
				t.Errorf("%s as %s: %d %s, want %s", query, who, code, out, want)
			}
		}
	}
}
