package assistant

import (
	"testing"
	"time"
)

// Durations go to the model as they are said, not as Go writes them: "2m0s" came back from it as a
// timer's label.
func TestWordsSaysADuration(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{2 * time.Minute, "2 minutes"},
		{time.Minute + 30*time.Second, "1 minute 30 seconds"},
		{time.Hour + 5*time.Minute + 9*time.Second, "1 hour 5 minutes"},
		{40 * time.Second, "40 seconds"},
		{0, "0 seconds"},
	} {
		if got := words(c.d); got != c.want {
			t.Errorf("words(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// A day by name lands on the next one to come, today included.
func TestDayOf(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.Local) // a Tuesday
	for in, want := range map[string]string{"": "", "today": "2026-09-29", "tomorrow": "2026-09-30",
		"Saturday": "2026-10-03", "this saturday": "2026-10-03", "tuesday": "2026-09-29", "2026-10-07": "2026-10-07"} {
		if got, err := dayOf(in, now); err != nil || got != want {
			t.Errorf("dayOf(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := dayOf("someday", now); err == nil {
		t.Error("someday was taken for a day")
	}
}

// An alarm's time is read as it was said, and a bare hour lands where people mean it.
func TestAlarmClock(t *testing.T) {
	for in, want := range map[string][2]int{"6:30": {6, 30}, "6:30 pm": {18, 30}, "6:30 PM": {18, 30}, "18:30": {18, 30},
		"7 am": {7, 0}, "7": {7, 0}, "12:15": {12, 15}, "12 am": {0, 0}, "2:00": {14, 0}, "11:45": {11, 45}, "6:30 a.m.": {6, 30}} {
		h, m, err := alarmClock(in)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("alarmClock(%q) = %d:%02d, %v; want %d:%02d", in, h, m, err, want[0], want[1])
		}
	}
	for _, bad := range []string{"soon", "25:00", "13 pm", "6:75"} {
		if _, _, err := alarmClock(bad); err == nil {
			t.Errorf("%q was taken for a time", bad)
		}
	}
}

// "Go home" is the screen's to do, and nothing for the model: asked as well, it took it for a call.
func TestGoingHomeIsHandledHere(t *testing.T) {
	for _, s := range []string{"go home", "Go home.", "show the home screen"} {
		if !handledHere(s) {
			t.Errorf("%q went to the model", s)
		}
	}
	for _, s := range []string{"what time is it", "set a timer for ten minutes", "when do the Huskers play"} {
		if handledHere(s) {
			t.Errorf("%q was kept from the model", s)
		}
	}
}
