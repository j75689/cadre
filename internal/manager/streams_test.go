package manager

import (
	"strings"
	"testing"

	"github.com/j75689/cadre/internal/storage"
)

// No stream is nested under a path that is an entry of another stream: a
// filesystem-backed object store cannot hold a file and a directory of the
// same name (see storage.Log). Every stream the Task Manager writes is listed
// here with an example of its entry keys.
func TestStreamsAreNotNestedUnderEntries(t *testing.T) {
	key := storage.Key{Namespace: "team-a", Name: "fix-login"}
	const runID = "01JB0000000000000000000001"
	streams := []struct {
		name    string
		stream  string
		example string // an entry key the stream holds
	}{
		{"thread", ThreadStream(key), storage.SeqKey(1)},
		{"runs", RunsStream(key), runID},
		{"events", EventsStream(key, runID), storage.SeqKey(1)},
	}
	for _, a := range streams {
		if err := storage.ValidateStream(a.stream); err != nil {
			t.Errorf("%s stream %q: %v", a.name, a.stream, err)
		}
		entry := a.stream + "/" + a.example
		for _, b := range streams {
			if b.stream == entry || strings.HasPrefix(b.stream, entry+"/") {
				t.Errorf("%s stream %q is nested under entry %q of the %s stream", b.name, b.stream, entry, a.name)
			}
		}
	}
}
