//go:build integration

package integration

import (
	"testing"
	"time"
)

// A paused container must be left alone, however unhealthy it looks.
//
// upstream/98: people pause containers with `docker compose pause` while a
// backup runs. A paused container's processes are frozen, so its healthcheck
// stops answering and docker eventually calls it unhealthy -- at which point
// autoheal restarts it, which is the one thing the person pausing it was
// trying to avoid. The pause is deliberate maintenance, not a fault.
//
// The world is built before autoheal is started, which is the whole reason
// Up leaves it stopped: sick and paused first, then let it look. Started
// earlier it would restart the container for being unhealthy long before the
// pause, and the test would be about its speed rather than about pausing.
//
// Trip then pause, in that order. The container is genuinely unhealthy and
// genuinely eligible by the time it is paused, so a restart that does not
// happen can only be because of the pause. Pausing a healthy container and
// waiting for it to rot would prove much less.
func TestPausedContainerIsLeftAlone(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")

	at := s.StartedAt("sick")

	// Unhealthy and labelled: everything autoheal acts on.
	s.MakeSick("sick")

	// And now deliberately paused.
	s.Pause("sick")

	// Only now does autoheal get to look.
	s.Start()

	// Long enough for several AUTOHEAL_INTERVAL=1 polls. Sleeping is honest
	// here: the assertion is that nothing happens, and there is no event to
	// wait for.
	time.Sleep(6 * time.Second)

	logs := s.Logs("autoheal")

	if got := s.State("sick"); got != "paused" {
		t.Errorf("a paused container should still be paused, docker says %q\n%s", got, logs)
	}
	if got := s.StartedAt("sick"); got.After(at) {
		t.Errorf("a paused container was restarted at %s\n%s", got, logs)
	}
}

// The other half: pausing is not a way to be ignored for ever. Once a
// container is unpaused it is an ordinary unhealthy container again, and
// autoheal should treat it as one.
//
// Without this, "skip paused containers" could be implemented by writing the
// container off permanently the first time it was seen paused, and the test
// above would not notice.
func TestUnpausedContainerIsRestartedAgain(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")

	at := s.StartedAt("sick")

	s.MakeSick("sick")
	s.Pause("sick")
	s.Start()

	// Long enough that autoheal has certainly seen it paused and passed over
	// it -- otherwise this proves only that it was restarted, not that being
	// paused was forgotten.
	time.Sleep(6 * time.Second)
	if got := s.StartedAt("sick"); got.After(at) {
		t.Fatalf("a paused container was restarted at %s\n%s", got, s.Logs("autoheal"))
	}

	s.Unpause("sick")

	// Still sick -- the gate outlives the pause -- so autoheal should act on
	// it now exactly as it would have before.
	s.waitFor("sick to be restarted once it is running again", 60*time.Second,
		func() bool { return s.StartedAt("sick").After(at) },
		func() string { return s.Logs("autoheal") })
}
