//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// What these tests are about: a recreate that keeps failing is retried at the
// pace AUTOHEAL_STRANDED_RETRY_INTERVALS sets, never stops, is logged on every
// attempt, and is announced once -- not on every attempt, and not at all if it
// is somebody else's by then. The fixture sets the list to 2s,4s,8s and the
// notice to 15s, so a minute of watching covers all of it.

// stillDown is the log line for a failed retry. The first failure is reported
// by a different line, so these start at the second attempt.
const stillDown = "is still down"

// notices are the notifications that say a service has been down a while,
// leaving out the ordinary ones (a failed heal, a recovery).
func (s *Scenario) notices() []string {
	s.t.Helper()
	var out []string
	for _, n := range s.Notifications() {
		if strings.Contains(n, "has been down for") {
			out = append(out, n)
		}
	}
	return out
}

// The list is the schedule, in order, and its last entry repeats.
//
// The first failure is the heal attempt itself; the first line this asserts on
// is the second attempt, so the waits seen are 4s (the second entry), then 8s
// (the third), and then 8s again for as long as it goes on. Six lines is past
// the end of a three-entry list, so a retry that stopped, or fell back to the
// first entry, would show.
func TestStrandedRetriesFollowTheConfiguredIntervals(t *testing.T) {
	s := Up(t, "autoheal-stranded/backoff")
	s.Start()

	strand(s)

	s.waitFor("six failed retries to be logged", 3*time.Minute,
		func() bool { return s.Times("autoheal", stillDown) >= 6 },
		func() string { return s.Logs("autoheal") })

	var waits []string
	for _, line := range strings.Split(s.Logs("autoheal"), "\n") {
		if !strings.Contains(line, stillDown) {
			continue
		}
		i := strings.Index(line, "next in ")
		if i < 0 {
			t.Fatalf("a failed retry was logged without saying when the next is:\n%s", line)
		}
		// The console logger colours its output; the escape sequence trails
		// the last word of the message.
		wait := strings.Fields(line[i+len("next in "):])[0]
		waits = append(waits, strings.SplitN(wait, "\x1b", 2)[0])
	}
	want := []string{"4s", "8s", "8s", "8s", "8s", "8s"}
	for i, w := range want {
		if i >= len(waits) || waits[i] != w {
			t.Fatalf("retry waits were %v, want them to start %v", waits, want)
		}
	}
}

// One notice, however long it goes on; every retry still logged; and when the
// service comes back the notice does not repeat.
func TestStrandedNoticeIsSentOnceButEveryRetryIsLogged(t *testing.T) {
	s := Up(t, "autoheal-stranded/backoff")
	s.Start()

	strand(s)

	s.waitFor("the 'down for a while' notice", 90*time.Second,
		func() bool { return len(s.notices()) > 0 },
		func() string { return s.Logs("autoheal") })

	got := s.notices()[0]
	if !strings.Contains(got, "test-host") || !strings.Contains(got, "app") {
		t.Errorf("the notice does not say which host and service:\n%s", got)
	}
	if !strings.Contains(got, "Autoheal is still retrying") {
		t.Errorf("the notice does not say autoheal is still on it:\n%s", got)
	}

	// Well past the notice, with retries still failing: more lines, no more
	// notices.
	seen := s.Times("autoheal", stillDown)
	s.waitFor("three more failed retries", 2*time.Minute,
		func() bool { return s.Times("autoheal", stillDown) >= seen+3 },
		func() string { return s.Logs("autoheal") })
	if n := len(s.notices()); n != 1 {
		t.Errorf("expected exactly one notice while it stayed down, got %d:\n%v", n, s.notices())
	}

	// And not again for the recovery.
	s.MakeWell("dep")
	s.waitFor("app to be brought back up", 2*time.Minute,
		func() bool { return s.State("app") == "running" && s.Health("app") == "healthy" },
		func() string { return s.Logs("autoheal") })
	time.Sleep(10 * time.Second)
	if n := len(s.notices()); n != 1 {
		t.Errorf("a notice was sent again around the recovery, got %d:\n%v", n, s.notices())
	}
}

// The refusing case: a service somebody started by hand is not down any more,
// so there is nothing to announce, however long the threshold would have taken.
func TestNoNoticeForAServiceStartedByHand(t *testing.T) {
	s := Up(t, "autoheal-stranded/backoff")
	s.Start()

	stranded := strand(s)
	if out, err := exec.Command("docker", "start", stranded).CombinedOutput(); err != nil {
		t.Fatalf("starting app by hand: %v\n%s", err, out)
	}
	s.WaitForLine("autoheal", "testbed-app-1 is running again")

	// Past the 15s threshold with margin: a notice that was going to be sent
	// has been.
	time.Sleep(25 * time.Second)
	if n := s.notices(); len(n) != 0 {
		t.Errorf("announced a service that had been started by hand:\n%v\n%s", n, s.Logs("autoheal"))
	}
}
