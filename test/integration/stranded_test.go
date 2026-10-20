//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// anyContainer is the service's container whatever its state. Container()
// asks `compose ps`, which lists only running ones, and the container these
// tests are about is the one that never started.
func (s *Scenario) anyContainer(service string) string {
	s.t.Helper()
	out, err := s.compose("ps", "-a", "-q", service)
	if err != nil || out == "" {
		return ""
	}
	return strings.Fields(out)[0]
}

// stateOf is docker's state for a container id, "" if it is gone.
func stateOf(id string) string {
	if id == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", id).Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// strand makes app's recreate fail partway: dep is held unhealthy, so compose
// replaces app's container and then refuses to start the new one. Returns the
// stranded container's id.
func strand(s *Scenario) string {
	s.t.Helper()
	s.MakeSick("dep")
	before := s.Container("app")
	s.MakeSick("app")

	var stranded string
	s.waitFor("app's recreate to fail partway", 90*time.Second,
		func() bool {
			stranded = s.anyContainer("app")
			return stranded != "" && stranded != before && stateOf(stranded) == "created"
		},
		func() string { return s.Logs("autoheal") })
	return stranded
}

// The incident: a recreate fails after compose has already replaced the
// container. Nothing lists the replacement as unhealthy -- it has never run
// -- so before the fix it stayed down for good. Once the dependency is back,
// autoheal must bring it up without being asked.
func TestFailedRecreateIsRetried(t *testing.T) {
	s := Up(t, "autoheal-stranded/dependency")
	s.Start()

	strand(s)
	s.MakeWell("dep")

	s.waitFor("app to be brought back up", 120*time.Second,
		func() bool { return s.State("app") == "running" && s.Health("app") == "healthy" },
		func() string { return s.Logs("autoheal") })

	if logs := s.Logs("autoheal"); !strings.Contains(logs, "Successfully recreated testbed-app-1") {
		t.Errorf("app came back but autoheal did not say it did it\n%s", logs)
	}
}

// The control, and what makes TestFailedRecreateIsRetried mean anything: the
// same partial recreate with autoheal not running at all. It has to strand the
// container -- created, never started -- in a way the unhealthy filter cannot
// see, and nothing else may bring it back once the dependency recovers.
//
// If compose ever started finishing the job itself, or docker began listing a
// never-started container as unhealthy, the retry test would still pass while
// proving nothing about autoheal. This one would fail instead, and say which.
func TestPartialRecreateStrandsTheServiceWithoutAutoheal(t *testing.T) {
	s := Up(t, "autoheal-stranded/dependency")
	// No Start(): Up leaves autoheal stopped, which is the point.

	s.MakeSick("dep")
	before := s.Container("app")
	// The command autoheal runs -- see composeProject.args.
	if out, err := s.compose("up", "-d", "--force-recreate", "app"); err == nil {
		t.Fatalf("the recreate succeeded with its dependency unhealthy, so there is nothing to strand:\n%s", out)
	}

	stranded := s.anyContainer("app")
	if stranded == "" || stranded == before {
		t.Fatalf("expected compose to have replaced app before failing; before %q, now %q", before, stranded)
	}
	if got := stateOf(stranded); got != "created" {
		t.Fatalf("expected the replacement to be left created, docker says %q", got)
	}

	// The filter autoheal polls with. A container in it would be found by the
	// ordinary path, and the stranded tracking would not be needed.
	unhealthy := s.docker("ps", "-a", "-q", "--no-trunc", "--filter", "health=unhealthy")
	if strings.Contains(unhealthy, stranded) {
		t.Fatalf("the stranded container is listed as unhealthy, so autoheal would see it anyway")
	}

	// Nothing else finishes the job once the cause is gone: longer than
	// autoheal's retry interval, so the retry test's recovery cannot be put
	// down to the passage of time.
	s.MakeWell("dep")
	time.Sleep(35 * time.Second)
	if got := stateOf(stranded); got != "created" {
		t.Errorf("app left the created state with no one acting on it: docker says %q", got)
	}
}

// The refusing case: a stranded service somebody has already started by hand
// is running, and is not autoheal's to rebuild. It must be forgotten, not
// recreated out from under them on the next retry.
func TestStrandedServiceStartedByHandIsLeftAlone(t *testing.T) {
	s := Up(t, "autoheal-stranded/dependency")
	s.Start()

	stranded := strand(s)
	// `docker start` rather than compose: it does not wait on dep, which is
	// still unhealthy -- so a retry, if one happened, would fail, and the
	// container id is what shows whether one was even attempted.
	if out, err := exec.Command("docker", "start", stranded).CombinedOutput(); err != nil {
		t.Fatalf("starting app by hand: %v\n%s", err, out)
	}
	s.WaitForLine("autoheal", "testbed-app-1 is running again")

	// Longer than the retry interval, so a retry that was going to happen has.
	time.Sleep(35 * time.Second)
	if now := s.anyContainer("app"); now != stranded {
		t.Errorf("app was recreated after it had been started by hand: %s became %s\n%s",
			stranded[:12], now[:12], s.Logs("autoheal"))
	}
}
