//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// What a notification says, end to end: composed here, posted over the
// network, read by something on the other side.
//
// The unit tests fake the notifier, so they can show the text was built
// correctly but not that it survives being sent. Both additions asserted here
// are about a human reading an alert and knowing what to do with it -- which
// host it came from (upstream/118) and why the container was unhealthy in the
// first place (upstream/81) -- so the thing worth testing is the text that
// actually arrives.
func TestNotificationSaysWhereAndWhy(t *testing.T) {
	s := Up(t, "autoheal-notify/content")

	s.MakeSick("noisy")
	s.Start()

	s.waitFor("a notification to arrive", 60*time.Second,
		func() bool { return len(s.Notifications()) > 0 },
		func() string { return s.Logs("autoheal") })

	got := s.Notifications()[0]

	// Which host. Container names repeat across machines, so an alert that
	// does not say where it came from cannot be acted on.
	if !strings.Contains(got, "test-host") {
		t.Errorf("the notification does not say which host it came from:\n%s", got)
	}

	// Still the ordinary message.
	if !strings.Contains(got, "noisy") {
		t.Errorf("the notification does not name the container:\n%s", got)
	}

	// And why: the healthcheck's own words, which docker kept in
	// State.Health.Log and autoheal read before restarting.
	if !strings.Contains(got, "the gate is closed") {
		t.Errorf("the notification does not say what the healthcheck reported:\n%s", got)
	}
}

// The healthcheck output has to be read before the restart, not after.
//
// Once the container comes back its health log belongs to the new process, and
// the words it said while failing -- the only ones worth putting in an alert
// -- are gone. An implementation that inspected after restarting would still
// produce a notification, and would still pass a test that only checked one
// arrived, so the assertion has to be on the text.
func TestHealthOutputIsReadBeforeTheRestart(t *testing.T) {
	s := Up(t, "autoheal-notify/content")

	before := s.StartedAt("noisy")
	s.MakeSick("noisy")
	s.Start()

	s.waitFor("noisy to be restarted", 60*time.Second,
		func() bool { return s.StartedAt("noisy").After(before) },
		func() string { return s.Logs("autoheal") })

	s.waitFor("a notification to arrive", 60*time.Second,
		func() bool { return len(s.Notifications()) > 0 },
		func() string { return s.Logs("autoheal") })

	// The container has restarted by now. If the output were read afterwards
	// it would describe the new process, which starts sick again here -- so
	// the distinguishing detail is that the text is present at all rather than
	// empty, and that it is the failing message rather than nothing.
	got := s.Notifications()[0]
	if !strings.Contains(got, "the gate is closed") {
		t.Errorf("the healthcheck's words did not survive the restart:\n%s\n%s", got, s.Logs("autoheal"))
	}
}
