//go:build integration

package integration

import (
	"testing"
	"time"
)

// The whole point of autoheal: a container that goes unhealthy is restarted,
// and one that is not labelled is not -- however sick it gets.
//
// Asserted on the containers rather than on autoheal's log lines. A restart
// keeps the container's id -- docker restarts it rather than replacing it --
// so the start time is what moves, and it is the daemon's own record of the
// thing that happened rather than something somebody chose to print.
//
// The order matters and is enforced rather than hoped for: the stack comes up
// with everything healthy, the test trips a gate, and MakeSick returns only
// once docker reports that container unhealthy. So a container that is not
// restarted afterwards was definitely offered to autoheal and definitely
// passed over.
func TestUnhealthyLabelledContainerIsRestarted(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")
	s.Start()

	before := map[string]time.Time{
		"sick":            s.StartedAt("sick"),
		"sick-unlabelled": s.StartedAt("sick-unlabelled"),
		"well":            s.StartedAt("well"),
	}

	// Both go unhealthy. Only one carries the label.
	s.MakeSick("sick")
	s.MakeSick("sick-unlabelled")

	t.Run("the labelled one is restarted", func(t *testing.T) {
		s.waitFor("sick to be restarted", 60*time.Second,
			func() bool { return s.StartedAt("sick").After(before["sick"]) },
			func() string { return s.Logs("autoheal") })
	})

	t.Run("the unlabelled one is left alone", func(t *testing.T) {
		// It has been unhealthy since MakeSick returned, and autoheal has by
		// now restarted the labelled one, so it has had every chance.
		if got := s.StartedAt("sick-unlabelled"); got.After(before["sick-unlabelled"]) {
			t.Errorf("a container with no autoheal label was restarted at %s", got)
		}
	})

	t.Run("the healthy one is left alone", func(t *testing.T) {
		if got := s.StartedAt("well"); got.After(before["well"]) {
			t.Errorf("a healthy container was restarted at %s", got)
		}
		if got := s.Health("well"); got != "healthy" {
			t.Errorf("well should still be healthy, docker says %q", got)
		}
	})
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
