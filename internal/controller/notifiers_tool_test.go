package controller_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/task"
	"github.com/j75689/cadre/internal/testenv"
)

// chat is an MCP server with one tool, post, that records its calls and
// answers with an error while failing is set.
type chat struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []postArgs
	failing atomic.Bool
}

type postArgs struct {
	Notification manager.Payload `json:"notification"`
	Channel      string          `json:"channel"`
}

func newChat(t *testing.T) *chat {
	c := &chat{}
	server := sdk.NewServer(&sdk.Implementation{Name: "chat", Version: "v0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "post"}, func(_ context.Context, _ *sdk.CallToolRequest, in postArgs) (*sdk.CallToolResult, any, error) {
		c.mu.Lock()
		c.calls = append(c.calls, in)
		c.mu.Unlock()
		res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "posted"}}}
		res.IsError = c.failing.Load()
		return res, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	c.Server = httptest.NewServer(handler)
	t.Cleanup(c.Close)
	return c
}

func (c *chat) posted() []postArgs {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]postArgs(nil), c.calls...)
}

func TestNotifierToolServer(t *testing.T) {
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond}, controller.SkipNameValidation()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	store, err := kubernetes.Open(ctx, kubernetes.Config{REST: cfg, S3: testenv.S3(t), Cache: mgr.GetCache()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, c: c}
	const ns = "team-tools"
	e.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, &v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)})
	room := newChat(t)
	toolNotifier := func(name, server string) *v1alpha1.Notifier {
		return &v1alpha1.Notifier{ObjectMeta: meta2(ns, name), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyQuestion},
			ToolServer: &v1alpha1.NotifierToolServer{Name: server, Tool: "post", Arguments: map[string]string{"channel": "#oncall"}}}}
	}

	// Ready, and why not.
	later := toolNotifier("later", "chat")
	e.create(later)
	e.ready(later, metav1.ConditionFalse, "ToolServerMissing")
	e.create(&v1alpha1.ToolServer{ObjectMeta: meta2(ns, "chat"), Spec: v1alpha1.ToolServerSpec{URL: room.URL}})
	e.ready(later, metav1.ConditionTrue, "Ready") // the ToolServer's arrival brings it back
	locked := &v1alpha1.ToolServer{ObjectMeta: meta2(ns, "locked"), Spec: v1alpha1.ToolServerSpec{URL: room.URL,
		Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "nope", Key: "k"}}}}
	asking := &v1alpha1.ToolServer{ObjectMeta: meta2(ns, "asking"), Spec: v1alpha1.ToolServerSpec{URL: room.URL, Permissions: map[string]v1alpha1.Permission{"post": v1alpha1.PermissionAsk}}}
	e.create(locked, asking)
	viaLocked, viaAsking := toolNotifier("via-locked", "locked"), toolNotifier("via-asking", "asking")
	e.create(viaLocked, viaAsking)
	e.ready(viaLocked, metav1.ConditionFalse, "ToolServerNotReady")
	e.ready(viaAsking, metav1.ConditionFalse, "ToolNotAllowed")
	for _, n := range []*v1alpha1.Notifier{viaLocked, viaAsking} {
		if err := c.Delete(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	// Delivery: the tool and a webhook on one event both receive it.
	var hooked atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hooked.Add(1) }))
	t.Cleanup(hook.Close)
	viaHook := &v1alpha1.Notifier{ObjectMeta: meta2(ns, "via-hook"), Spec: v1alpha1.NotifierSpec{On: []v1alpha1.NotifyEvent{v1alpha1.NotifyQuestion}, Webhook: &v1alpha1.Webhook{URL: hook.URL}}}
	e.create(viaHook)
	e.ready(viaHook, metav1.ConditionTrue, "Ready")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := manager.New(store, manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude"), Notifiers: controller.Notifiers(mgr),
		Tools: mcp.NewForwarder(controller.ToolServerSources(mgr), log)})
	ask := func(name string) storage.Key {
		t.Helper()
		tk, err := m.CreateTask(ctx, ns, name, v1alpha1.TaskSpec{Title: "Ask " + name, Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}})
		if err != nil {
			t.Fatal(err)
		}
		cl := claim(t, m)
		key := storage.Key{Namespace: ns, Name: tk.Name}
		if _, _, err := m.Finish(ctx, key, manager.FinishRequest{RunID: cl.RunID, Outcome: task.OutcomeWaiting, Reason: v1alpha1.WaitingQuestion}); err != nil {
			t.Fatal(err)
		}
		return key
	}
	ask("x")
	if err := m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	got := room.posted()
	if len(got) != 1 || got[0].Channel != "#oncall" || got[0].Notification.Event != v1alpha1.NotifyQuestion || got[0].Notification.Task.Name != "x" {
		t.Fatalf("posted %+v", got)
	}
	if hooked.Load() != 1 {
		t.Errorf("the webhook got %d", hooked.Load())
	}

	// A tool answering with an error has not taken it; the next pass sends
	// it again, to the tool only.
	room.failing.Store(true)
	key := ask("y")
	_ = m.DeliverNotifications(ctx)
	var tk v1alpha1.Task
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: key.Name}, &tk); err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, it := range tk.Status.Outbox {
		if it.Kind == v1alpha1.OutboxNotify {
			pending++
			if len(it.Notify.Delivered) != 1 || it.Notify.Delivered[0] != "via-hook" {
				t.Errorf("delivered %v", it.Notify.Delivered)
			}
		}
	}
	if pending != 1 {
		t.Fatalf("%d notifications pending after an isError", pending)
	}
	eventually(t, 10*time.Second, func() (bool, string) {
		var n v1alpha1.Notifier
		_ = c.Get(ctx, client.ObjectKeyFromObject(later), &n)
		return n.Status.LastError != nil && n.Status.LastError.Message == "tool post on ToolServer chat answered with an error", "lastError"
	})
	room.failing.Store(false)
	if err := m.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if n, h := len(room.posted()), hooked.Load(); n != 3 || h != 2 {
		t.Errorf("tool %d, webhook %d deliveries; want 3 and 2", n, h)
	}
}
