// Package provider holds what the builtin loop's model providers share:
// how a model API's error is reduced for the log and classified for the
// loop's retries.
package provider

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/runes"
)

// MaxMessage bounds how much of an API error's message is kept.
const MaxMessage = 300

// APIError is a model API's error reduced to what may be logged: the
// status, the error's type, code and message, and the request id. A
// response body, which a gateway may fill with anything, is never kept.
type APIError struct {
	API       string // "the Messages API", say
	Status    int    // 0 for an error event inside a stream
	Kind      string
	Code      string
	Param     string // the request field the error is about, when the API says
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(e.API)
	if e.Status != 0 {
		fmt.Fprintf(&b, " answered %d %s", e.Status, http.StatusText(e.Status))
	} else {
		b.WriteString(" sent an error in its stream")
	}
	var detail []string
	for _, s := range []string{e.Kind, e.Code} {
		if s != "" {
			detail = append(detail, s)
		}
	}
	if e.Message != "" {
		detail = append(detail, runes.Cut(e.Message, MaxMessage))
		if len(e.Message) > MaxMessage {
			detail[len(detail)-1] += "…"
		}
	}
	if len(detail) > 0 {
		b.WriteString(": " + strings.Join(detail, ": "))
	}
	if e.RequestID != "" {
		b.WriteString(" (request " + e.RequestID + ")")
	}
	return b.String()
}

// permanentKinds are the error types retrying cannot fix, whatever the
// status: an error event in a stream comes with the stream's 200.
var permanentKinds = map[string]bool{
	"invalid_request_error": true, "authentication_error": true, "permission_error": true, "not_found_error": true,
}

// Classify marks e Permanent when retrying cannot fix it: one of
// permanentKinds, or any 4xx but a timeout, a conflict and a rate limit.
// Server errors, overload and rate limits stay retriable.
func Classify(e *APIError) error {
	switch s := e.Status; {
	case permanentKinds[e.Kind]:
		return builtin.Permanent(e)
	case s == http.StatusRequestTimeout, s == http.StatusConflict, s == http.StatusTooManyRequests, s >= 500:
		return e
	case s >= 400:
		return builtin.Permanent(e)
	}
	return e
}
