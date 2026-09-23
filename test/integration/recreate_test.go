//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// A container labelled autoheal.mode=recreate is rebuilt from its compose
// file, not restarted.
//
// Asserted on the container id, which is exactly what tells the two apart:
// docker restarts a container in place and the id survives, while compose
// recreates it by replacing the object, so a new id is the daemon's own record
// that a rebuild -- and not merely a restart -- is what happened.
//
// This is the case the mode exists for. A container whose stored configuration
// has gone stale (network_mode: service:<peer>, peer since recreated) cannot
// be fixed by a restart at all: it replays the same dead peer id and fails
// every time. Only recreating re-resolves it.
func TestRecreateModeReplacesTheContainer(t *testing.T) {
	s := Up(t, "autoheal-recreate/compose")
	s.Start()

	before := s.Container("sick")
	if before == "" {
		t.Fatal("no container for sick before the test started")
	}
	s.MakeSick("sick")

	s.waitFor("sick to be recreated", 90*time.Second,
		func() bool {
			now := s.Container("sick")
			return now != "" && now != before
		},
		func() string { return s.Logs("autoheal") })
}

// The control, and the reason the test above is about recreate rather than
// about autoheal doing anything at all: the same stack, the same fault, the
// same label -- only the mode differs -- and this container must keep its id.
//
// Without this an implementation that recreated everything would pass the
// recreate test and be wrong, and one that restarted everything would fail it
// for the right reason but leave the default unverified.
func TestRestartModeKeepsTheContainer(t *testing.T) {
	s := Up(t, "autoheal-recreate/compose")
	s.Start()

	before := s.Container("sick-restart")
	started := s.StartedAt("sick-restart")
	s.MakeSick("sick-restart")

	s.waitFor("sick-restart to be restarted", 90*time.Second,
		func() bool { return s.StartedAt("sick-restart").After(started) },
		func() string { return s.Logs("autoheal") })

	if now := s.Container("sick-restart"); now != before {
		t.Errorf("restart mode replaced the container: %s became %s\n%s",
			before[:12], now[:12], s.Logs("autoheal"))
	}
}

// The mode is permission to rebuild a broken container, not an instruction to
// rebuild a working one. A healthy container carrying the label must be left
// exactly where it is.
func TestRecreateModeLeavesHealthyContainersAlone(t *testing.T) {
	s := Up(t, "autoheal-recreate/compose")
	s.Start()

	before := s.Container("well")

	// Nothing is done to "well". The wait gives autoheal a window in which it
	// demonstrably acted -- its sibling being recreated is proof it was awake
	// and looking while "well" sat there labelled.
	sick := s.Container("sick")
	s.MakeSick("sick")
	s.waitFor("autoheal to act on the sick one", 90*time.Second,
		func() bool {
			now := s.Container("sick")
			return now != "" && now != sick
		},
		func() string { return s.Logs("autoheal") })

	logs := s.Logs("autoheal")
	if got := s.Container("well"); got != before {
		t.Errorf("a healthy container was recreated: %s became %s\n%s",
			before[:12], got[:12], logs)
	}
	if got := s.Health("well"); got != "healthy" {
		t.Errorf("the healthy container should still be healthy, docker says %q\n%s", got, logs)
	}
}

// A recreated container has to come back working, which is the whole point.
// Worth its own assertion because every other test here would pass on a
// container that was replaced by a broken one.
func TestRecreatedContainerRecovers(t *testing.T) {
	s := Up(t, "autoheal-recreate/compose")
	s.Start()

	before := s.Container("sick")
	s.MakeSick("sick")
	s.waitFor("sick to be recreated", 90*time.Second,
		func() bool {
			now := s.Container("sick")
			return now != "" && now != before
		},
		func() string { return s.Logs("autoheal") })

	// MakeWell returns only once docker reports it healthy, so reaching the
	// end of it is the assertion.
	s.MakeWell("sick")
}

// Crash-loop detection has to keep working across recreates, and this is the
// case where it could quietly stop: every recreate gives the service a new
// container id, so a strike count keyed on the old one would start from zero
// every cycle and the threshold would never be reached.
//
// The container here never recovers, so autoheal should rebuild it until the
// threshold and then stop -- the same contract as the restart-mode crash loop,
// across three different container ids.
func TestRecreateCrashLoopIsDetectedAcrossNewContainerIDs(t *testing.T) {
	s := Up(t, "autoheal-recreate/compose")
	s.Start()

	// How many attempts autoheal will make before giving up, from the same
	// .env compose read when it started the stack.
	strikes := s.SettingInt("CRASHLOOP_STRIKES")

	before := s.Container("sick")

	// Tripped and left tripped: every rebuild lands back in the same state,
	// which is what makes this a loop rather than unrelated faults.
	s.MakeSick("sick")

	// Waited on by counting recreates rather than by watching for the log
	// line this then asserts on -- sequencing on the log would mean a subject
	// that printed nothing looked like one that did nothing.
	//
	// Counting distinct container ids is what makes this test about the
	// strike transfer: reaching the threshold at all requires the count to
	// have followed the service across every one of them.
	seen := map[string]bool{before: true}
	s.waitFor("autoheal to spend its allowance on sick", 180*time.Second,
		func() bool {
			if id := s.Container("sick"); id != "" {
				seen[id] = true
			}
			// The original plus one container per recreate.
			return len(seen) > strikes
		},
		func() string { return s.Logs("autoheal") })

	logs := s.Logs("autoheal")
	if n := strings.Count(logs, gaveUp); n != 1 {
		t.Errorf("expected exactly one crash-loop report, got %d\n%s", n, logs)
	}

	// Having given up, it stops: the container it last built is left standing
	// for somebody to look at rather than being rebuilt forever.
	settled := s.Container("sick")
	time.Sleep(5 * time.Second)
	if now := s.Container("sick"); now != settled {
		t.Errorf("kept recreating after giving up: %s became %s\n%s",
			settled[:12], now[:12], s.Logs("autoheal"))
	}
}
