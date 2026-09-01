package integration

import (
	"fmt"
	"strings"
	"time"
)

// Waiting for the world to settle.
//
// Almost every flaky integration test is a test that looked before the thing
// it was watching had happened. In shell that was a sleep, or a poll with a
// wall-clock window guessing which log lines belonged to which run -- both
// wrong often enough to matter, and silent when they were.
//
// These wait for a condition and say what they last saw when it never came
// true, so a timeout is a diagnosis rather than a mystery.

// waitFor blocks until cond returns true, or fails the test after timeout
// with what describe last reported.
func (s *Scenario) waitFor(what string, timeout time.Duration, cond func() bool, describe func() string) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("timed out after %s waiting for %s\nlast saw:\n%s",
		timeout, what, indent(describe()))
}

func indent(s string) string {
	if s == "" {
		return "    <nothing>"
	}
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// ReadyLine is what the subject prints once it is running and has done
// something. Start waits for it, so a test never acts on a subject that has
// not looked at the world yet.
//
// A line rather than a container state, because "up" and "ready" are not the
// same: autoheal is up the moment its process starts and useful only once it
// has read its configuration and begun polling. Which line that is belongs to
// the subject, so it lives here rather than in the harness's logic.
var ReadyLine = "Watching containers"

// ready reports whether the subject has announced itself.
func (s *Scenario) ready(service string) bool {
	return strings.Contains(s.Logs(service), ReadyLine)
}

// WaitForLine returns the first log line from a service containing want,
// waiting for it to appear.
func (s *Scenario) WaitForLine(service, want string) string {
	s.t.Helper()
	var found string
	s.waitFor(want+" in "+service+"'s output", 30*time.Second,
		func() bool {
			for _, line := range strings.Split(s.Logs(service), "\n") {
				if strings.Contains(line, want) {
					found = strings.TrimSpace(line)
					return true
				}
			}
			return false
		},
		func() string { return s.Logs(service) })
	return found
}

// WaitForRestarts blocks until a service has been restarted want times since
// from, and returns how many were seen.
//
// Counted by watching the container's start time, not docker's RestartCount:
// that field only counts restarts docker itself performed under a restart
// policy, and stays at zero for the ones autoheal asks for. Verified rather
// than assumed -- two `docker restart` calls leave it reading 0.
//
// The count is the honest signal that a scenario has been exercised. Waiting
// for a log line instead would mean sequencing the test on the same wording it
// is about to assert on, and a subject that printed nothing would look like a
// subject that did nothing.
func (s *Scenario) WaitForRestarts(service string, from time.Time, want int) int {
	s.t.Helper()

	seen, last := 0, from
	s.waitFor(fmt.Sprintf("%s to be restarted %d times", service, want),
		90*time.Second,
		func() bool {
			if at := s.StartedAt(service); at.After(last) {
				seen++
				last = at
			}
			return seen >= want
		},
		func() string {
			return fmt.Sprintf("saw %d of %d restarts\n%s", seen, want, s.Logs(service))
		})
	return seen
}

// Times is how often a service has printed a line containing want.
//
// A count rather than a yes/no, because "it happened" cannot tell a second
// occurrence from the first one still being there. A test that drives a
// container round the same loop twice needs to know the second lap happened,
// and Contains says yes to the line left over from the first.
func (s *Scenario) Times(service, want string) int {
	s.t.Helper()
	return strings.Count(s.Logs(service), want)
}

// WaitForMore blocks until a service has printed want more times than it had
// at was.
//
// The shape a test wants when it drives something round a loop: take a count,
// act, then wait for the count to move.
func (s *Scenario) WaitForMore(service, want string, was int) {
	s.t.Helper()
	s.waitFor(fmt.Sprintf("%s to print %q again", service, want), 60*time.Second,
		func() bool { return s.Times(service, want) > was },
		func() string { return s.Logs(service) })
}
