//go:build integration

package integration

import (
	"testing"
	"time"
)

// The whole point of autoheal: a labelled container that goes unhealthy is
// restarted.
//
// Asserted on the container rather than on autoheal's log lines. A restart
// keeps the container's id -- docker restarts it rather than replacing it --
// so the start time is what moves, and it is the daemon's own record of the
// thing that happened rather than something somebody chose to print.
//
// The order is enforced rather than hoped for: the stack comes up healthy, and
// MakeSick returns only once docker reports the container unhealthy. So what
// follows is definitely a response to a container autoheal was offered.
func TestUnhealthyLabelledContainerIsRestarted(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")
	s.Start()

	before := s.StartedAt("sick")
	s.MakeSick("sick")

	s.waitFor("sick to be restarted", 60*time.Second,
		func() bool { return s.StartedAt("sick").After(before) },
		func() string { return s.Logs("autoheal") })
}

// Carrying the label is permission, not an instruction. A container that is
// healthy must be left alone, or autoheal would be restarting things on a
// schedule rather than in response to anything.
//
// The other half of the label's meaning, and the reason it is not enough to
// test that unlabelled containers are ignored: an implementation that
// restarted every labelled container regardless of health would pass that and
// fail this.
func TestHealthyLabelledContainerIsLeftAlone(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")
	s.Start()

	before := s.StartedAt("well")

	// Nothing is done to it. The wait is for autoheal to have had the chance:
	// its sibling going sick and being restarted is proof that it was awake
	// and looking during this window.
	sick := s.StartedAt("sick")
	s.MakeSick("sick")
	s.waitFor("autoheal to act on the sick one", 60*time.Second,
		func() bool { return s.StartedAt("sick").After(sick) },
		func() string { return s.Logs("autoheal") })

	logs := s.Logs("autoheal")
	if got := s.Health("well"); got != "healthy" {
		t.Errorf("the healthy container should still be healthy, docker says %q\n%s", got, logs)
	}
	if got := s.StartedAt("well"); got.After(before) {
		t.Errorf("a healthy container was restarted at %s\n%s", got, logs)
	}
}

// A restart is only useful if the container comes back. Recovery is also what
// separates a transient fault from the crash loop autoheal gives up on, so it
// is worth its own assertion rather than being implied.
func TestRestartedContainerRecovers(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")
	s.Start()

	before := s.StartedAt("sick")
	s.MakeSick("sick")
	s.waitFor("sick to be restarted", 60*time.Second,
		func() bool { return s.StartedAt("sick").After(before) },
		func() string { return s.Logs("autoheal") })

	// Clearing the gate is what a real fix looks like. MakeWell returns only
	// once docker reports it healthy, so this is the assertion -- checking the
	// health afterwards would be asking the same question twice.
	s.MakeWell("sick")
}
