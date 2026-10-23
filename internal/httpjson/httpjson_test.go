package httpjson_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Every error maps to its documented status and code, and nothing internal
// leaks into the message unless it describes the request.
func TestFail(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		task.ErrRunMismatch:        {409, httpjson.CodeRunMismatch},
		task.ErrStepMismatch:       {409, httpjson.CodeStepMismatch},
		task.ErrInvalidTransition:  {409, httpjson.CodeInvalidTransition},
		storage.ErrAlreadyExists:   {409, httpjson.CodeAlreadyExists},
		storage.ErrNotFound:        {404, httpjson.CodeNotFound},
		storage.ErrInvalid:         {400, httpjson.CodeInvalid},
		storage.ErrStreamCollision: {503, httpjson.CodeUnavailable},
		manager.ErrInvalid:         {400, httpjson.CodeInvalid},
		manager.ErrBusy:            {503, httpjson.CodeUnavailable},
		fmt.Errorf("etcd timeout"): {503, httpjson.CodeUnavailable},
	} {
		rec := httptest.NewRecorder()
		httpjson.Fail(rec, log, fmt.Errorf("get https://10.0.0.1/apis/x: bucket secret-bucket: %w", err))
		var er httpjson.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&er)
		if rec.Code != want.status || er.Error.Code != want.code {
			t.Errorf("%v: %d %s, want %d %s", err, rec.Code, er.Error.Code, want.status, want.code)
		}
		leaks := strings.Contains(er.Error.Message, "10.0.0.1") || strings.Contains(er.Error.Message, "secret-bucket")
		if leaks && want.status != 400 {
			t.Errorf("%v: the response leaks internal details: %q", err, er.Error.Message)
		}
	}
}
