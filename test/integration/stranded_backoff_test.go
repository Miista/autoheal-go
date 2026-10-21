//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// What these tests are about: a recreate that keeps failing is retried at the
// pace AUTOHEAL_STRANDED_RETRY_INTERVALS sets, never stops, and is announced
// once -- not on every attempt, and not at all if it is somebody else's by
// then. The fixture sets the list to 2s,4s,8s and the notice to 15s, so a
// minute of watching covers all of it.
//
// Everything asserted is an outcome in the world: containers that were
// replaced, and when; notifications that arrived. Nothing reads autoheal's own
// log, which is wording it is free to change.

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

// createdAt is when docker made the container, and false if it is already gone:
// with retries a few seconds apart, the next recreate can remove a container
// between being listed and being asked about.
func (s *Scenario) createdAt(id string) (time.Time, bool) {
	s.t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.Created}}", id).Output()
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, trimmed(out))
	if err != nil {
		s.t.Fatalf("reading when container %s was created from %q: %v", id, trimmed(out), err)
	}
	return at, true
}

// attempts watches app's container being replaced, once per recreate autoheal
// tries, and returns when each replacement was made, the stranded one first.
//
// Every attempt compose gets as far as replacing the container before it fails
// on the dependency, so a new container is an attempt that happened, whatever
// autoheal said about it.
func (s *Scenario) attempts(stranded string, n int, timeout time.Duration) []time.Time {
	s.t.Helper()
	var times []time.Time
	if at, ok := s.createdAt(stranded); ok {
		times = append(times, at)
	}
	last := stranded
	s.waitFor("autoheal to have tried the recreate again", timeout,
		func() bool {
			if id := s.anyContainer("app"); id != "" && id != last {
				if at, ok := s.createdAt(id); ok {
					last = id
					times = append(times, at)
				}
			}
			return len(times) >= n
		},
		func() string { return s.Logs("autoheal") })
	return times
}

// The list is the schedule, in order, and its last entry repeats.
//
// The waits between attempts are 2s, 4s, then 8s for as long as it goes on.
// Each gap is at least its interval -- a retry that came sooner would not be
// backing off -- and the later ones do not grow beyond the last entry, which
// they would if the list were being doubled or run past its end. Seven
// containers is past the end of a three-entry list, so a retry that stopped
// would not produce them.
func TestStrandedRetriesFollowTheConfiguredIntervals(t *testing.T) {
	s := Up(t, "autoheal-stranded/backoff")
	s.Start()

	stranded := strand(s)
	times := s.attempts(stranded, 7, 3*time.Minute)

	// Compose takes a moment on each attempt, and both ends of a gap are
	// stamped mid-run, so the bounds are generous either way.
	const slack = time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, interval := range want {
		gap := times[i+1].Sub(times[i])
		if gap < interval-slack {
			t.Errorf("attempt %d came %s after the one before, sooner than the %s the list asks for", i+2, gap, interval)
		}
		if i >= 3 && gap > interval+6*time.Second {
			t.Errorf("attempt %d came %s after the one before: the last entry (%s) should repeat, not grow", i+2, gap, interval)
		}
	}
}

// One notice, however long it goes on; retries carry on regardless; and when
// the service comes back the notice does not repeat.
func TestStrandedNoticeIsSentOnceWhileRetriesContinue(t *testing.T) {
	s := Up(t, "autoheal-stranded/backoff")
	s.Start()

	stranded := strand(s)

	s.waitFor("the 'down for a while' notice", 90*time.Second,
		func() bool { return len(s.notices()) > 0 },
		func() string { return s.Logs("autoheal") })

	got := s.notices()[0]
	if !strings.Contains(got, "test-host") || !strings.Contains(got, "app") {
		t.Errorf("the notice does not say which host and service:\n%s", got)
	}

	// Well past the notice, with retries still failing -- shown by the
	// container still being replaced, not by anything autoheal says: more
	// attempts, no more notices.
	s.attempts(stranded, 5, 2*time.Minute)
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
// so there is nothing to announce, however long the threshold would have taken
// -- and it is not rebuilt out from under them either.
func TestNoNoticeForAServiceStartedByHand(t *testing.T) {
	s := Up(t, "autoheal-stranded/backoff")
	s.Start()

	stranded := strand(s)
	if out, err := exec.Command("docker", "start", stranded).CombinedOutput(); err != nil {
		t.Fatalf("starting app by hand: %v\n%s", err, out)
	}
	s.waitFor("app to be running", 30*time.Second,
		func() bool { return stateOf(stranded) == "running" },
		func() string { return s.Logs("autoheal") })

	// Past the 15s threshold with margin: a notice that was going to be sent
	// has been, and a retry that was going to happen has.
	time.Sleep(25 * time.Second)
	if n := s.notices(); len(n) != 0 {
		t.Errorf("announced a service that had been started by hand:\n%v", n)
	}
	if now := s.anyContainer("app"); now != stranded {
		t.Errorf("app was recreated after it had been started by hand: %s became %s", stranded[:12], now[:12])
	}
}
