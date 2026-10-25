package controller

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A JobRefused note is the reason without the Job's name, and fits an
// Event, cut at a character's start. (The Events themselves are tested in
// jobs_test.go.)
func TestRefusalNote(t *testing.T) {
	gr := schema.GroupResource{Group: "batch", Resource: "jobs"}
	err := apierrors.NewForbidden(gr, "coder-01jexample", errors.New("exceeded quota: no-jobs, requested: count/jobs.batch=1"))
	if got, want := refusalNote(err), "the API server refused a Job: exceeded quota: no-jobs, requested: count/jobs.batch=1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	long := apierrors.NewForbidden(gr, "x", errors.New(strings.Repeat("é", 1000)))
	note := refusalNote(long)
	if len(note) > eventNoteMax || !strings.HasSuffix(note, "...") || !utf8.ValidString(note) {
		t.Errorf("a long note: %d bytes, valid %v, ends %q", len(note), utf8.ValidString(note), note[len(note)-5:])
	}
}
