// Package storage defines the driver interface. The core depends on three
// operations only: single-object conditional write (Objects), append-only log
// (Log) and rebuildable index (Index). Every driver implements all three and
// must pass the suite in storage/conformance.
package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// Errors every driver returns, wrapped or not; test with errors.Is.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict: the object changed since it was read")
	ErrAlreadyExists = errors.New("already exists")
	// ErrInvalid means the store refused an object as malformed, as the
	// Kubernetes API server does when a CRD validation rule fails.
	ErrInvalid = errors.New("invalid object")
)

// Driver is a storage backend.
type Driver interface {
	Objects
	Log
	Index
	// Close stops background work such as watches. It is idempotent.
	Close() error
}

// Key names a namespaced object.
type Key struct {
	Namespace string
	Name      string
}

func (k Key) String() string { return k.Namespace + "/" + k.Name }

// Objects is the single-object conditional write.
//
// An object's version is metadata.resourceVersion: an opaque string the core
// never parses. A write succeeds only if the object's stored version still
// equals the version on the object passed in; on success the passed object's
// version is updated in place.
type Objects interface {
	GetTask(ctx context.Context, key Key) (*v1alpha1.Task, error)
	// CreateTask writes metadata and spec. Status is ignored, as by the
	// Kubernetes API; set it with UpdateTaskStatus. It fails with
	// ErrAlreadyExists if the name is taken, which makes a create with a
	// deterministic name idempotent.
	CreateTask(ctx context.Context, t *v1alpha1.Task) error
	// UpdateTaskStatus writes status only, conditionally on the version.
	UpdateTaskStatus(ctx context.Context, t *v1alpha1.Task) error

	GetTeam(ctx context.Context, namespace string) (*v1alpha1.Team, error)
	// CreateTeam writes metadata and spec; status is ignored.
	CreateTeam(ctx context.Context, team *v1alpha1.Team) error
	// UpdateTeamStatus writes status only, conditionally on the version.
	UpdateTeamStatus(ctx context.Context, team *v1alpha1.Team) error
}

// Entry is one log entry.
type Entry struct {
	Key  string
	Data []byte
}

// Log is the append-only log: threads, run history, audit. A stream is a
// sequence of immutable entries ordered by key.
type Log interface {
	// Append writes one entry. It fails with ErrAlreadyExists if the key is
	// taken, so concurrent appenders racing for a key get exactly one winner
	// and a retried append of the same entry is detectable. The stream must
	// pass ValidateStream and the key ValidateKey, or it fails with
	// ErrInvalidName.
	Append(ctx context.Context, stream, key string, data []byte) error
	// Read returns up to limit entries with keys greater than after, in
	// ascending key order. An empty after reads from the start, and a limit
	// of zero or less means no limit. Entries of streams nested under this
	// one are never returned.
	Read(ctx context.Context, stream, after string, limit int) ([]Entry, error)
}

// ErrInvalidName is returned by Log methods for a malformed stream or key.
var ErrInvalidName = errors.New("invalid log stream or key")

// ValidateStream checks a stream name: one or more non-empty segments
// separated by "/". Streams nest by name ("ns/t/thread" and
// "ns/t/thread/files" are two streams), but Read on one never returns the
// other's entries.
func ValidateStream(stream string) error {
	if stream == "" || slices.Contains(strings.Split(stream, "/"), "") {
		return fmt.Errorf("stream %q: %w", stream, ErrInvalidName)
	}
	return nil
}

// ValidateKey checks an entry key: non-empty and without "/".
func ValidateKey(key string) error {
	if key == "" || strings.Contains(key, "/") {
		return fmt.Errorf("key %q: %w", key, ErrInvalidName)
	}
	return nil
}

// SeqKey formats a sequence number as a log key that sorts numerically.
func SeqKey(seq int64) string { return fmt.Sprintf("%020d", seq) }

// Query selects Tasks from the index.
type Query struct {
	Namespace string           // required
	Phases    []v1alpha1.Phase // any of these; empty means all
	Offset    int              // not negative
	Limit     int              // not negative; zero means no limit
}

// ErrInvalidQuery is returned by ListTasks for a query Validate rejects.
var ErrInvalidQuery = errors.New("invalid query")

// Validate rejects a query without a namespace or with a negative offset or
// limit. Every driver calls it first in ListTasks.
func (q Query) Validate() error {
	switch {
	case q.Namespace == "":
		return fmt.Errorf("namespace is required: %w", ErrInvalidQuery)
	case q.Offset < 0 || q.Limit < 0:
		return fmt.Errorf("offset %d, limit %d: must not be negative: %w", q.Offset, q.Limit, ErrInvalidQuery)
	}
	return nil
}

// Index is the rebuildable index for board queries.
//
// It is eventually consistent: it may lag the objects by an informer's
// latency, and it is rebuilt from a full list whenever a driver starts. No
// transition may read it to decide anything; it serves queries only, and
// correctness always rests on Objects' conditional writes.
type Index interface {
	// ListTasks returns matching Tasks ordered by priority (urgent first),
	// then by number, then by name. It fails with ErrInvalidQuery if
	// q.Validate does.
	ListTasks(ctx context.Context, q Query) ([]v1alpha1.Task, error)
	// ListTeams returns every Team, ordered by namespace. The namespaces
	// that have one are the namespaces Cadre serves.
	ListTeams(ctx context.Context) ([]v1alpha1.Team, error)
}
