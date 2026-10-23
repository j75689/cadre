package task

import (
	"errors"
	"slices"
	"strings"
	"time"

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

// AcquireSlot gives runID a slot for taskName. It is written before the
// Task claim, so a lost claim must be followed by ReleaseSlot. Taking a slot
// runID already holds succeeds without a second slot.
func AcquireSlot(team *v1alpha1.Team, runID, taskName string, now time.Time) (v1alpha1.TeamStatus, error) {
	s := *team.Status.DeepCopy()
	if HoldsSlot(team, runID) {
		return s, nil
	}
	// The API server defaults MaxConcurrentRuns to 1; a Team built elsewhere
	// gets the same floor rather than zero slots.
	if len(s.Slots) >= int(max(team.Spec.MaxConcurrentRuns, 1)) {
		return s, ErrNoSlot
	}
	return AddSlot(team, runID, taskName, now), nil
}

// AddSlot records that runID holds a slot for taskName since acquiredAt,
// past the cap if need be. AcquireSlot uses it once the cap is checked; the
// reconciler uses it directly, for a run that is visibly Running but whose
// slot is missing, since refusing would hide a run from the count.
func AddSlot(team *v1alpha1.Team, runID, taskName string, acquiredAt time.Time) v1alpha1.TeamStatus {
	s := *team.Status.DeepCopy()
	if !HoldsSlot(team, runID) {
		s.Slots = append(s.Slots, v1alpha1.Slot{RunID: runID, Task: taskName, AcquiredAt: metaTime(acquiredAt)})
	}
	s.Running = int32(len(s.Slots))
	return s
}

// ReleaseSlot gives runID's slot back. Releasing a slot runID does not hold
// is a no-op, so every release can be retried and none counts twice.
func ReleaseSlot(team *v1alpha1.Team, runID string) v1alpha1.TeamStatus {
	s := *team.Status.DeepCopy()
	s.Slots = slices.DeleteFunc(s.Slots, func(sl v1alpha1.Slot) bool { return sl.RunID == runID })
	s.Running = int32(len(s.Slots))
	return s
}

// HoldsSlot reports whether runID holds a slot.
func HoldsSlot(team *v1alpha1.Team, runID string) bool {
	return slices.ContainsFunc(team.Status.Slots, func(sl v1alpha1.Slot) bool { return sl.RunID == runID })
}

// SubtaskName is the metadata.name of the subtask a CreateSubtask outbox item
// creates: the item's ULID, lowercased into a DNS-1123 label. Because the name
// is fixed by the item, re-applying it is a create that already exists.
func SubtaskName(item v1alpha1.OutboxItem) string { return strings.ToLower(item.ID) }
