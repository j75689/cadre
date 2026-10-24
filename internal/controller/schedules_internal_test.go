package controller

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// A tick whose jitter outlasts the next tick's holds the next one back: it
// fires first, and the next after it, neither skipped.
func TestLatestDueFiresTicksInOrder(t *testing.T) {
	sched, err := parseSchedule(v1alpha1.ScheduleSpec{Cron: "* * * * *"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Minute), t0.Add(2*time.Minute)
	offset := map[time.Time]time.Duration{t1: 90 * time.Second}
	fire := func(tick time.Time) time.Time { return tick.Add(offset[tick]) }

	if due, missed := latestDue(sched, t0, t2.Add(time.Second), fire); !due.IsZero() || missed != "" {
		t.Errorf("before t1's fire time: due %v, missed %q; want nothing", due, missed)
	}
	if due, missed := latestDue(sched, t0, t1.Add(91*time.Second), fire); !due.Equal(t1) || missed != "" {
		t.Errorf("after t1's fire time: due %v, missed %q; want t1, not t2", due, missed)
	}
	if due, missed := latestDue(sched, t1, t1.Add(91*time.Second), fire); !due.Equal(t2) || missed != "" {
		t.Errorf("t1 fired: due %v, missed %q; want t2", due, missed)
	}
	// Ticks that were due before the next one ticked were missed.
	if due, missed := latestDue(sched, t2, t2.Add(3*time.Minute+time.Second), fire); !due.Equal(t2.Add(3*time.Minute)) || missed != "2" {
		t.Errorf("three ticks late: due %v, missed %q; want the third, 2", due, missed)
	}
}

func TestParseScheduleRefuses(t *testing.T) {
	for name, spec := range map[string]v1alpha1.ScheduleSpec{
		"a TZ prefix":      {Cron: "TZ=Asia/Tokyo 0 9 * * *"},
		"a CRON_TZ prefix": {Cron: "CRON_TZ=Asia/Tokyo 0 9 * * *"},
		"a bare TZ":        {Cron: "TZ=UTC"},
		"every 30 seconds": {Cron: "@every 30s"},
		"the pod's zone":   {Cron: "0 9 * * *", Timezone: "Local"},
		"six fields":       {Cron: "0 0 9 * * *"},
		"an unknown zone":  {Cron: "0 9 * * *", Timezone: "Mars/Olympus_Mons"},
	} {
		if _, err := parseSchedule(spec); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "cron") && !strings.Contains(err.Error(), "timezone") {
			t.Errorf("%s: %v does not name the field", name, err)
		}
	}
	for _, cron := range []string{"@every 1m", "@every 5m", "@daily", "*/5 * * * *"} {
		if _, err := parseSchedule(v1alpha1.ScheduleSpec{Cron: cron}); err != nil {
			t.Errorf("%s: %v", cron, err)
		}
	}
}
