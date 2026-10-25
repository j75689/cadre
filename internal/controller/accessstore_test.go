package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
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
	recorder := events.NewFakeRecorder(10)
	api.NewServer(manager.New(memory.NewBackend().Open(), manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)),
		api.WithAccess(auth.ModeOIDC, &authz.Policy{AdminUsers: []string{root.Username}}),
		api.WithAgentAccesses(controller.AgentAccessStore(e.c, e.c, recorder))).Register(mux)
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
	// Rotating: a new token, in this one answer; the Secret and the status
	// have it at once, so the old one stops and the reconciler finds
	// nothing to do; an Event names who rotated it.
	code, out = do("POST", "/bot/token", "")
	var rotated api.AgentAccessToken
	if err := json.Unmarshal([]byte(out), &rotated); code != http.StatusOK || err != nil || rotated.Token == "" || rotated.Token == tok || rotated.TokenSecret != "bot.access-token" {
		t.Fatalf("rotate: %d %s", code, out)
	}
	agents := controller.AgentAccesses(e.c, e.c, nil, nil)
	if _, err := agents.Authenticate(ctx, rotated.Token); err != nil {
		t.Errorf("the new token: %v", err)
	}
	if _, err := agents.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("the old token: %v", err)
	}
	time.Sleep(time.Second) // a few resyncs
	var s corev1.Secret
	var aa v1alpha1.AgentAccess
	if err := errors.Join(e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "bot.access-token"}, &s), e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "bot"}, &aa)); err != nil ||
		string(s.Data["token"]) != rotated.Token || aa.Status.TokenHash != token.Hash(rotated.Token) {
		t.Fatalf("after the rotation: Secret %q, hash %q, %v", s.Data["token"], aa.Status.TokenHash, err)
	}
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "TokenRotated") || !strings.Contains(ev, "rotated by root@example.test") || strings.Contains(ev, rotated.Token) {
			t.Errorf("event %q", ev)
		}
	default:
		t.Error("no Event for the rotation")
	}
	// A ServiceAccount's has no token to rotate.
	if code, _ := do("PUT", "/ci", `{"spec":{"role":"viewer","serviceAccount":{"name":"ci"}}}`); code != http.StatusCreated {
		t.Fatalf("make ci: %d", code)
	}
	if code, out := do("POST", "/ci/token", ""); code != http.StatusBadRequest || !strings.Contains(out, "ServiceAccount") {
		t.Errorf("rotating a ServiceAccount's: %d %s", code, out)
	}
	if code, _ := do("DELETE", "/bot", ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do("DELETE", "/bot", ""); code != http.StatusNotFound {
		t.Errorf("delete again: %d", code)
	}
	shown := 0
	for _, b := range bodies {
		if strings.Contains(b, tok) || strings.Contains(b, token.Hash(tok)) || strings.Contains(b, token.Hash(rotated.Token)) {
			t.Fatalf("an answer carries a token minted by the reconciler, or a hash: %s", b)
		}
		if strings.Contains(b, rotated.Token) {
			shown++
		}
	}
	if shown != 1 {
		t.Errorf("the rotated token is in %d answers, want the rotate's alone", shown)
	}
}
