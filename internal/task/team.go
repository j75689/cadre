package task

import (
	"errors"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// ErrNoSlot means the Team's semaphore is full: MaxConcurrentRuns runs are
// already running.
var ErrNoSlot = errors.New("team has no free run slot")

// ClaimNumber takes the next task number from the Team's counter. The caller
// writes the returned status with a conditional write before using the number.
func ClaimNumber(team *v1alpha1.Team) (int64, v1alpha1.TeamStatus) {
	s := team.Status
	if s.NextNumber < 1 {
		s.NextNumber = 1
	}
	n := s.NextNumber
	s.NextNumber++
	return n, s
}

// AcquireSlot takes one run slot. It is written before the Task claim, so a
// lost Task claim must be followed by ReleaseSlot.
func AcquireSlot(team *v1alpha1.Team) (v1alpha1.TeamStatus, error) {
	s := team.Status
	// The API server defaults MaxConcurrentRuns to 1; a Team built elsewhere
	// gets the same floor rather than zero slots.
	if s.Running >= max(team.Spec.MaxConcurrentRuns, 1) {
		return s, ErrNoSlot
	}
	s.Running++
	return s, nil
}

// ReleaseSlot gives a run slot back. It never goes below zero, because a lost
// decrement is tolerated and the count is recomputed from the index.
func ReleaseSlot(team *v1alpha1.Team) v1alpha1.TeamStatus {
	s := team.Status
	if s.Running > 0 {
		s.Running--
	}
	return s
}

// SubtaskName is the metadata.name of the subtask a CreateSubtask outbox item
// creates: the item's ULID, lowercased into a DNS-1123 label. Because the name
// is fixed by the item, re-applying it is a create that already exists.
func SubtaskName(item v1alpha1.OutboxItem) string { return strings.ToLower(item.ID) }
