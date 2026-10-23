// Package client is a Go client for cadre-server's /api/v1 and /runner/v1.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
)

// Error is an error response from the server.
type Error struct {
	Status int
	httpjson.ErrorBody
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message) }

// IsCode reports whether err is a server error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client talks to one cadre-server.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the server at base, e.g. http://localhost:8080.
// A trailing slash is ignored.
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc}
}

// do sends a JSON request and decodes a JSON response into out. It returns
// the status, so callers can tell 204 apart.
func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		// Drained, so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		var er httpjson.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&er); err != nil || er.Error.Code == "" {
			return resp.StatusCode, &Error{Status: resp.StatusCode, ErrorBody: httpjson.ErrorBody{Code: "http", Message: resp.Status}}
		}
		return resp.StatusCode, &Error{Status: resp.StatusCode, ErrorBody: er.Error}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func nsPath(ns string) string { return api.Base + "/namespaces/" + url.PathEscape(ns) }

func taskPath(ns, name string) string { return nsPath(ns) + "/tasks/" + url.PathEscape(name) }

// CreateTask creates a task; an empty name lets the server pick one.
func (c *Client) CreateTask(ctx context.Context, ns, name string, spec v1alpha1.TaskSpec) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	_, err := c.do(ctx, http.MethodPost, nsPath(ns)+"/tasks", api.CreateTaskRequest{Name: name, Spec: spec}, &t)
	return &t, err
}

// ListTasks lists a page of the board.
func (c *Client) ListTasks(ctx context.Context, ns string, phases []v1alpha1.Phase, offset, limit int) (*api.TaskList, error) {
	q := url.Values{}
	for _, p := range phases {
		q.Add("phase", string(p))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var list api.TaskList
	_, err := c.do(ctx, http.MethodGet, nsPath(ns)+"/tasks?"+q.Encode(), nil, &list)
	return &list, err
}

// GetTask reads a task.
func (c *Client) GetTask(ctx context.Context, ns, name string) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	_, err := c.do(ctx, http.MethodGet, taskPath(ns, name), nil, &t)
	return &t, err
}

// CancelTask cancels a task.
func (c *Client) CancelTask(ctx context.Context, ns, name string) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	_, err := c.do(ctx, http.MethodPost, taskPath(ns, name)+"/cancel", nil, &t)
	return &t, err
}

// Comment writes to a task's thread and returns the entry.
func (c *Client) Comment(ctx context.Context, ns, name, author, body string) (*manager.ThreadEntry, error) {
	var entry manager.ThreadEntry
	_, err := c.do(ctx, http.MethodPost, taskPath(ns, name)+"/comments", api.CommentRequest{Author: author, Body: body}, &entry)
	return &entry, err
}

// Reply answers a waiting task.
func (c *Client) Reply(ctx context.Context, ns, name, author, body string) (*api.ReplyResponse, error) {
	var out api.ReplyResponse
	_, err := c.do(ctx, http.MethodPost, taskPath(ns, name)+"/reply", api.CommentRequest{Author: author, Body: body}, &out)
	return &out, err
}

// Approve approves (approve true) or denies the tool call a task waits on.
func (c *Client) Approve(ctx context.Context, ns, name, author string, approve bool, note string) (*api.ApproveResponse, error) {
	var out api.ApproveResponse
	_, err := c.do(ctx, http.MethodPost, taskPath(ns, name)+"/approve", api.ApproveRequest{Author: author, Approve: &approve, Note: note}, &out)
	return &out, err
}

// Thread reads up to limit thread entries after sequence number after.
func (c *Client) Thread(ctx context.Context, ns, name string, after int64, limit int) (*api.Thread, error) {
	var out api.Thread
	path := fmt.Sprintf("%s/thread?after=%d&limit=%d", taskPath(ns, name), after, limit)
	_, err := c.do(ctx, http.MethodGet, path, nil, &out)
	return &out, err
}

// Runs reads a task's run history.
func (c *Client) Runs(ctx context.Context, ns, name string) (*api.Runs, error) {
	var out api.Runs
	_, err := c.do(ctx, http.MethodGet, taskPath(ns, name)+"/runs", nil, &out)
	return &out, err
}

// Events reads one run's progress events.
func (c *Client) Events(ctx context.Context, ns, name, runID string) (*api.Events, error) {
	var out api.Events
	_, err := c.do(ctx, http.MethodGet, taskPath(ns, name)+"/runs/"+url.PathEscape(runID)+"/events", nil, &out)
	return &out, err
}

// InitTeam opts a namespace in.
func (c *Client) InitTeam(ctx context.Context, ns string, req api.TeamRequest) (*v1alpha1.Team, error) {
	var team v1alpha1.Team
	_, err := c.do(ctx, http.MethodPut, nsPath(ns)+"/team", req, &team)
	return &team, err
}

// ListTeams lists every Team.
func (c *Client) ListTeams(ctx context.Context) (*api.TeamList, error) {
	var out api.TeamList
	_, err := c.do(ctx, http.MethodGet, api.Base+"/teams", nil, &out)
	return &out, err
}

// Claim asks for a task; it returns nil when there is no work.
func (c *Client) Claim(ctx context.Context, req runner.ClaimRequest) (*runner.ClaimResponse, error) {
	var out runner.ClaimResponse
	status, err := c.do(ctx, http.MethodPost, runner.PathClaim, req, &out)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return &out, nil
}

// Heartbeat renews a run's lease.
func (c *Client) Heartbeat(ctx context.Context, req runner.HeartbeatRequest) (*runner.HeartbeatResponse, error) {
	var out runner.HeartbeatResponse
	_, err := c.do(ctx, http.MethodPost, runner.PathHeartbeat, req, &out)
	return &out, err
}

// Event reports a run's progress.
func (c *Client) Event(ctx context.Context, req runner.EventsRequest) error {
	_, err := c.do(ctx, http.MethodPost, runner.PathEvents, req, nil)
	return err
}

// Finish ends a run.
func (c *Client) Finish(ctx context.Context, req runner.FinishRequest) (*runner.FinishResponse, error) {
	var out runner.FinishResponse
	_, err := c.do(ctx, http.MethodPost, runner.PathFinish, req, &out)
	return &out, err
}
