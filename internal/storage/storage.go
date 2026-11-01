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
	// ErrStreamCollision means a log entry could not be written because its
	// path is taken by a nested stream: a layout fault on the server side,
	// not a malformed request (see Log).
	ErrStreamCollision = errors.New("log entry collides with a nested stream")
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
	// UpdateTeamSpec writes spec only, conditionally on the version: status
	// is left as it is, and a stale version fails with ErrConflict.
	UpdateTeamSpec(ctx context.Context, team *v1alpha1.Team) error
}

// Entry is one log entry.
type Entry struct {
	Key  string
	Data []byte
}

// KeyInfo describes a log entry without its data.
type KeyInfo struct {
	Key  string
	Size int64
}

// Log is the append-only log: threads, run history, audit. A stream is a
// sequence of immutable entries ordered by key.
//
// Layout: a driver that keeps entries at paths puts a stream's entries
// under the reserved segment EntrySegment, <stream>/~e/<key>, and no stream
// name may use it, so a stream and an entry never share a path whatever the
// names. Object stores backed by a filesystem, such as SeaweedFS, cannot
// hold a file and a directory of the same name: without it, stream "a/runs"
// with entry "R1" would rule out a stream "a/runs/R1/...".
type Log interface {
	// Append writes one entry. It fails with ErrAlreadyExists if the key is
	// taken, so concurrent appenders racing for a key get exactly one winner
	// and a retried append of the same entry is detectable. ErrAlreadyExists
	// means an entry with that key exists and Read returns it; a driver that
	// cannot write the key for any other reason must say so with another
	// error, or an idempotent caller would take a lost write for a done one.
	// The stream must pass ValidateStream and the key ValidateKey, or it
	// fails with ErrInvalidName.
	Append(ctx context.Context, stream, key string, data []byte) error
	// Read returns up to limit entries with keys greater than after, in
	// ascending key order. An empty after reads from the start, and a limit
	// of zero or less means no limit. Entries of streams nested under this
	// one are never returned.
	Read(ctx context.Context, stream, after string, limit int) ([]Entry, error)
	// Keys is Read without the data: the keys and sizes of up to limit
	// entries after after, in ascending key order. It never reads entry
	// contents, so finding the latest entry of a stream is cheap.
	Keys(ctx context.Context, stream, after string, limit int) ([]KeyInfo, error)
}

// ErrInvalidName is returned by Log methods for a malformed stream or key.
var ErrInvalidName = errors.New("invalid log stream or key")

// EntrySegment is the path segment a stream's entries live under, which no
// stream name may use (see Log). Its "~" is outside every alphabet a
// stream's segments are made of (DNS-1123 names, ULIDs, base64url, digits),
// so no namespace, task, run or path can equal it.
const EntrySegment = "~e"

// ValidateStream checks a stream name: one or more non-empty segments
// separated by "/", none of them EntrySegment. Streams nest by name
// ("ns/t/thread" and "ns/t/thread/files" are two streams), but Read on one
// never returns the other's entries.
func ValidateStream(stream string) error {
	segments := strings.Split(stream, "/")
	if stream == "" || slices.Contains(segments, "") {
		return fmt.Errorf("stream %q: %w", stream, ErrInvalidName)
	}
	if slices.Contains(segments, EntrySegment) {
		return fmt.Errorf("stream %q: the segment %q is where entries live: %w", stream, EntrySegment, ErrInvalidName)
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
	Parent    string           // subtasks of this task (spec.parentRef.name); empty means any
	Number    int64            // the task with this status.number, unique in a namespace; zero means any
	Assignee  string           // tasks assigned to this name, of either kind; empty means any
	Labels    []string         // tasks that have every one of these labels; empty means any
	Text      string           // tasks whose title, name, assignee name, a label or "#<number>" contain it, trimmed, in any case (see Matches); empty means any
	Offset    int              // not negative
	Limit     int              // not negative; zero means no limit
}

// ErrInvalidQuery is returned by ListTasks for a query Validate rejects.
var ErrInvalidQuery = errors.New("invalid query")

// Validate rejects a query without a namespace or with a negative number,
// offset or limit. Every driver calls it first in ListTasks.
func (q Query) Validate() error {
	switch {
	case q.Namespace == "":
		return fmt.Errorf("namespace is required: %w", ErrInvalidQuery)
	case q.Offset < 0 || q.Limit < 0:
		return fmt.Errorf("offset %d, limit %d: must not be negative: %w", q.Offset, q.Limit, ErrInvalidQuery)
	case q.Number < 0:
		return fmt.Errorf("number %d: must not be negative: %w", q.Number, ErrInvalidQuery)
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
	// CountTasks returns how many Tasks ListTasks selects before its offset
	// and limit apply. It fails with ErrInvalidQuery if q.Validate does.
	CountTasks(ctx context.Context, q Query) (int, error)
	// ListTeams returns every Team, ordered by namespace. The namespaces
	// that have one are the namespaces Cadre serves.
	ListTeams(ctx context.Context) ([]v1alpha1.Team, error)
}

// TeamIndex is what a Driver may also offer: one Team from the index, by
// namespace, as fresh as ListTeams and without listing them all.
type TeamIndex interface {
	IndexedTeam(ctx context.Context, namespace string) (*v1alpha1.Team, error)
}

// Notifier is what a Driver may also offer: it hands every Task it sees
// change, as its index does, to fn. A notification is a hint, like the
// index: one may be late, duplicated or dropped, so nothing may depend on
// it but latency. fn must not block; it runs on the driver's watch.
type Notifier interface {
	NotifyTasks(fn func(*v1alpha1.Task))
}
