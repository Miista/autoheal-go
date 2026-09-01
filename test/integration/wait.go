package integration

import (
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
