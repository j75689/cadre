package controller_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/token"
)

// AgentAccesses through the API, kept in the cluster: made, edited from
// their version, refused as the CRD refuses them, listed and deleted; and
// the token Cadre mints for one is in no answer, nor its hash.
func TestAgentAccessesThroughTheAPI(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	const ns = "team-api"
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, &v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)})
	mux := http.NewServeMux()
	// With logins, as AgentAccesses are served only then: every request
	// is a bootstrap admin's.
	root := &auth.Identity{Username: "root@example.test"}
	api.NewServer(manager.New(memory.NewBackend().Open(), manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)),
		api.WithAccess(auth.ModeOIDC, &authz.Policy{AdminUsers: []string{root.Username}}),
		api.WithAgentAccesses(controller.AgentAccessStore(e.c, e.c))).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), root)))
	}))
	t.Cleanup(srv.Close)
	var bodies []string
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+api.Base+"/namespaces/"+ns+"/agentaccesses"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		bodies = append(bodies, string(out))
		return resp.StatusCode, string(out)
	}

	code, out := do("PUT", "/bot", `{"spec":{"role":"member"}}`)
	var made api.AgentAccessView
	if err := json.Unmarshal([]byte(out), &made); code != http.StatusCreated || err != nil || made.ResourceVersion == "" {
		t.Fatalf("make: %d %s", code, out)
	}
	// Cadre mints its token.
	var tok string
	eventually(t, 10*time.Second, func() (bool, string) {
		var s corev1.Secret
		if err := e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "bot.access-token"}, &s); err != nil {
			return false, err.Error()
		}
		tok = string(s.Data["token"])
		return tok != "", "no token yet"
	})
	if code, out := do("PUT", "/bot", `{"resourceVersion":"1","spec":{"role":"viewer"}}`); code != http.StatusConflict || !strings.Contains(out, `"conflict"`) {
		t.Errorf("an edit from a stale version: %d %s", code, out)
	}
	var fresh api.AgentAccessList
	_, out = do("GET", "", "")
	if err := json.Unmarshal([]byte(out), &fresh); err != nil || len(fresh.Items) != 1 {
		t.Fatalf("list: %s", out)
	}
	if code, out := do("PUT", "/bot", `{"resourceVersion":"`+fresh.Items[0].ResourceVersion+`","spec":{"role":"viewer"}}`); code != http.StatusOK || !strings.Contains(out, `"role":"viewer"`) {
		t.Errorf("an edit from its version: %d %s", code, out)
	}
	for name, body := range map[string]string{"/boss": `{"spec":{"role":"admin"}}`, "/bot.v2": `{"spec":{"role":"member"}}`} {
		if code, out := do("PUT", name, body); code != http.StatusBadRequest || !strings.Contains(out, `"invalid"`) {
			t.Errorf("%s, which the CRD refuses: %d %s", name, code, out)
		}
	}
	eventually(t, 10*time.Second, func() (bool, string) {
		_, out := do("GET", "", "")
		return strings.Contains(out, `"ready":true`) && strings.Contains(out, `"tokenSecret":"bot.access-token"`), out
	})
	if code, _ := do("DELETE", "/bot", ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do("DELETE", "/bot", ""); code != http.StatusNotFound {
		t.Errorf("delete again: %d", code)
	}
	for _, b := range bodies {
		if strings.Contains(b, tok) || strings.Contains(b, token.Hash(tok)) {
			t.Fatalf("an answer carries the token or its hash: %s", b)
		}
	}
}
