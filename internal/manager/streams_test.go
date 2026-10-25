package manager

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/j75689/cadre/internal/storage"
)

// Every stream the Task Manager writes is a valid stream for any task: a
// namespace, task, run or path cannot make one that uses the segment
// entries live under (storage.EntrySegment), even one named like it.
func TestStreamsAreValidForAnyName(t *testing.T) {
	if len(validation.IsDNS1123Label(storage.EntrySegment)) == 0 {
		t.Fatalf("%q is a DNS label: a namespace or a task could be named it", storage.EntrySegment)
	}
	const runID = "01JB0000000000000000000001"
	// "e" is a name a namespace or task may have: the segment's letter.
	for _, name := range []string{"team-a", "e"} {
		key := storage.Key{Namespace: name, Name: name}
		for what, stream := range map[string]string{
			"thread":     ThreadStream(key),
			"runs":       RunsStream(key),
			"events":     EventsStream(key, runID),
			"transcript": TranscriptStream(key, runID),
			"file":       fileStream(key, "e"),
			"files":      filesIndexStream(key),
			"snapshots":  filesSnapshotStream(key),
			"blob":       blobStream(key, blobName(runID, "e")),
		} {
			if err := storage.ValidateStream(stream); err != nil {
				t.Errorf("the %s stream of %s/%s: %v", what, name, name, err)
			}
		}
	}
}
