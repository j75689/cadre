package mcp_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/kubernetes"
	"github.com/j75689/cadre/internal/task"
	"github.com/j75689/cadre/internal/testenv"
)

// session opens an MCP session on the run's endpoint.
func (e *env) session(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	s, err := client.Connect(ctx, e.transport(token), nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// call invokes a tool and decodes its structured result into out; it
// returns the result's text when the tool reports an error.
func call(t *testing.T, s *sdk.ClientSession, name string, args any, out any) (errText string) {
	t.Helper()
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var texts []string
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
		return strings.Join(texts, " ")
	}
	if out != nil {
		data, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s result: %v", name, err)
		}
	}
	return ""
}

var key = storage.Key{Namespace: ns, Name: "t"}

func TestRunTools(t *testing.T) { runTools(t, start(t)) }

// The same, on the kubernetes driver: the files and their index are new log
// streams, so they are checked on a real store too when CADRE_TEST_S3_* is
// set (SeaweedFS in CI).
func TestRunToolsOnTheKubernetesDriver(t *testing.T) {
	cfg := testenv.APIServer(t)
	admin, err := client.New(cfg, client.Options{})
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
	runTools(t, startOn(t, drv))
}

func runTools(t *testing.T, e *env) {
	t.Helper()
	c := e.claim(t)
	s := e.session(t, c.MCPToken)

	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"task.create_subtask", "task.files.list", "task.files.read", "task.files.write", "thread.ask", "thread.post"}; !slices.Equal(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}

	var posted struct{ Seq int64 }
	if msg := call(t, s, "thread.post", map[string]any{"body": "Looking at the login test."}, &posted); msg != "" || posted.Seq != 1 {
		t.Fatalf("thread.post: %+v %q", posted, msg)
	}
	thread, _ := e.m.ReadThread(ctx, key, 0, 0)
	if len(thread) != 1 || thread[0].Author.Kind != manager.AuthorAgent || thread[0].Author.Name != "coder" {
		t.Fatalf("thread: %+v", thread)
	}

	var sub manager.SubtaskCreated
	if msg := call(t, s, "task.create_subtask", map[string]any{"title": "Write the regression test"}, &sub); msg != "" || sub.Name == "" || sub.Number < 2 {
		t.Fatalf("create_subtask: %+v %q", sub, msg)
	}
	child, err := e.m.GetTask(ctx, storage.Key{Namespace: ns, Name: sub.Name})
	if err != nil || child.Spec.ParentRef == nil || child.Spec.ParentRef.Name != "t" || child.Spec.Assignee != nil || child.Status.Number != sub.Number {
		t.Fatalf("subtask: %+v, %v", child, err)
	}
	if msg := call(t, s, "task.create_subtask", map[string]any{"title": "Review", "assignee": map[string]any{"kind": "Agent", "name": "coder"}}, &sub); msg != "" {
		t.Fatalf("create_subtask with an assignee: %q", msg)
	}
	if msg := call(t, s, "task.create_subtask", map[string]any{"title": " "}, nil); !strings.HasPrefix(msg, "Invalid call") {
		t.Fatalf("create_subtask without a title: %q", msg)
	}

	var info manager.FileInfo
	for i, content := range []string{"first draft", "second draft"} {
		if msg := call(t, s, "task.files.write", map[string]any{"path": "notes/plan.md", "content": content}, &info); msg != "" || info.Version != int64(i+1) {
			t.Fatalf("write %d: %+v %q", i+1, info, msg)
		}
	}
	call(t, s, "task.files.write", map[string]any{"path": "out.txt", "content": "x"}, nil)
	var file manager.File
	if msg := call(t, s, "task.files.read", map[string]any{"path": "notes/plan.md"}, &file); msg != "" || file.Content != "second draft" || file.Version != 2 {
		t.Fatalf("read: %+v %q", file, msg)
	}
	var list struct{ Files []manager.FileInfo }
	if msg := call(t, s, "task.files.list", map[string]any{}, &list); msg != "" || len(list.Files) != 2 ||
		list.Files[0] != (manager.FileInfo{Path: "notes/plan.md", Version: 2, Size: len("second draft")}) {
		t.Fatalf("list: %+v %q", list, msg)
	}
	if msg := call(t, s, "task.files.read", map[string]any{"path": "missing"}, nil); msg != "Not found." {
		t.Fatalf("read a missing file: %q", msg)
	}
	if msg := call(t, s, "task.files.write", map[string]any{"path": "big", "content": strings.Repeat("x", manager.MaxFileSize+1)}, nil); !strings.HasPrefix(msg, "Invalid call") {
		t.Fatalf("write over the limit: %q", msg)
	}

	// thread.ask ends the run: the task waits for a human, the token dies,
	// and a finish from the runner is a run mismatch.
	var asked struct {
		Seq     int64
		Message string
	}
	if msg := call(t, s, "thread.ask", map[string]any{"question": "Staging or production?"}, &asked); msg != "" || !strings.Contains(asked.Message, "do not call finish") {
		t.Fatalf("thread.ask: %+v %q", asked, msg)
	}
	tk, _ := e.m.GetTask(ctx, key)
	if tk.Status.Phase != v1alpha1.PhaseWaiting || tk.Status.Waiting.Reason != v1alpha1.WaitingQuestion || tk.Status.Waiting.ThreadSeq != asked.Seq {
		t.Fatalf("after thread.ask: %+v", tk.Status)
	}
	if got := e.status(t, "Bearer "+c.MCPToken); got != 404 {
		t.Fatalf("token after thread.ask: status %d, want 404", got)
	}
	if _, _, err := e.m.Finish(ctx, key, manager.FinishRequest{RunID: c.RunID, Outcome: task.OutcomeDone}); !errors.Is(err, task.ErrRunMismatch) {
		t.Fatalf("finish after thread.ask: %v, want run mismatch", err)
	}
}

// A run that loses its task between resolving its token and running a tool
// is told plainly to stop.
func TestALostRunIsToldToStop(t *testing.T) {
	e := start(t)
	c := e.claim(t)
	if _, err := e.m.RunPost(ctx, key, "01JB0000000000000000000000", "x"); !errors.Is(err, task.ErrRunMismatch) {
		t.Fatalf("post from another run: %v", err)
	}
	r := mcp.FailForTest(slog.New(slog.NewTextHandler(io.Discard, nil)), task.ErrRunMismatch)
	if !r.IsError || !strings.Contains(r.Content[0].(*sdk.TextContent).Text, "no longer active") || !strings.Contains(r.Content[0].(*sdk.TextContent).Text, "Stop") {
		t.Fatalf("lost-run tool error: %+v", r)
	}
	_ = c
}
