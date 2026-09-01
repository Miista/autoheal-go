//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// What autoheal prints when it gives up on a container, and when it decides a
// recovered one has earned a clean slate. Named because several tests wait on
// them, and because a log line is a contract with whoever is reading it at
// three in the morning -- if it changes, these should fail rather than quietly
// wait forever.
const (
	gaveUp         = "crash loop"
	clearedHistory = "clearing its crash-loop history"
)

// Crash-loop detection is the one thing this tool does that upstream autoheal
// does not: upstream restarts an unhealthy container forever, with no memory
// of how many times it has already tried.
//
// A patient whose gate stays tripped is a real crash loop -- it comes back
// unhealthy from every restart, because the thing making it sick outlives the
// restart. That is what a crash loop is, and it is why restarting again is
// not the answer.
func TestCrashLoopIsGivenUpOn(t *testing.T) {
	s := Up(t, "autoheal-crashloop/gives-up")
	s.Start()

	// How many restarts autoheal will try before giving up, from the same
	// .env compose read when it started the stack.
	strikes := s.SettingInt("CRASHLOOP_STRIKES")

	at := s.StartedAt("looper")

	// Tripped and left tripped: every restart lands back in the same state,
	// which is what makes this a loop rather than three unrelated faults.
	s.MakeSick("looper")

	// The scenario is exercised once autoheal has spent its allowance. Waited
	// on by counting restarts rather than by watching for a log line: the log
	// is what this test is about to assert on, and sequencing on it would mean
	// a subject that printed nothing looked like a subject that did nothing.
	s.WaitForRestarts("looper", at, strikes)

	// Having spent it, autoheal must stop. Several poll cycles at
	// AUTOHEAL_INTERVAL=1 -- sleeping is honest here, because the assertion is
	// that nothing happens and there is no event to wait for.
	settled := s.StartedAt("looper")
	time.Sleep(6 * time.Second)

	logs := s.Logs("autoheal")

	if got := s.StartedAt("looper"); got.After(settled) {
		t.Errorf("looper was restarted again after autoheal gave up, at %s\n%s", got, logs)
	}
	if n := strings.Count(logs, gaveUp); n != 1 {
		t.Errorf("autoheal should give up once and say so once, said it %d times:\n%s", n, logs)
	}
}

// Giving up is not permanent. A container that becomes genuinely healthy has
// had whatever ailed it fixed, so its history is no longer evidence of
// anything -- and the next time it goes bad it deserves a fresh restart rather
// than being written off for something it did yesterday.
func TestGivingUpIsForgottenOnRecovery(t *testing.T) {
	s := Up(t, "autoheal-crashloop/gives-up")
	s.Start()

	s.MakeSick("looper")
	s.waitFor("autoheal to give up on looper", 60*time.Second,
		func() bool { return strings.Contains(s.Logs("autoheal"), gaveUp) },
		func() string { return s.Logs("autoheal") })

	// Genuinely fixed. MakeWell returns only once docker reports it healthy,
	// so autoheal has something real to observe.
	s.MakeWell("looper")

	// autoheal clears the history on its own poll, which is a different clock
	// from docker's healthcheck. Waiting for it to say so is the only honest
	// signal that it has noticed -- there is no other observable effect until
	// the container goes bad again.
	s.waitFor("autoheal to clear looper's history", 60*time.Second,
		func() bool { return strings.Contains(s.Logs("autoheal"), clearedHistory) },
		func() string { return s.Logs("autoheal") })

	// The relapse: it must be restarted again rather than stay written off.
	at := s.StartedAt("looper")
	s.MakeSick("looper")

	s.waitFor("looper to be restarted after recovering", 60*time.Second,
		func() bool { return s.StartedAt("looper").After(at) },
		func() string { return s.Logs("autoheal") })
}

// The negative case for the whole feature: a single restart that works is not
// a crash loop, and must not be reported as one. Without this, a crash-loop
// detector that fired on every restart would pass the tests above.
func TestAOneOffRestartIsNotACrashLoop(t *testing.T) {
	s := Up(t, "autoheal-crashloop/gives-up")
	s.Start()

	at := s.StartedAt("looper")
	s.MakeSick("looper")
	s.waitFor("looper to be restarted", 60*time.Second,
		func() bool { return s.StartedAt("looper").After(at) },
		func() string { return s.Logs("autoheal") })

	// Fixed before autoheal runs out of patience, which is what a transient
	// fault looks like.
	s.MakeWell("looper")

	// Long enough for the strikes to have accumulated had they been going to.
	time.Sleep(6 * time.Second)

	if logs := s.Logs("autoheal"); strings.Contains(logs, gaveUp) {
		t.Errorf("a container that recovered was reported as crash-looping:\n%s", logs)
	}
}

// Strikes that stop short of the threshold must be forgotten on recovery, not
// merely paused.
//
// The distinction matters and nothing else covers it: TestGivingUpIsForgotten
// crosses the threshold first, so it proves that being given up on is
// reversible. This one never crosses it. A container that racked up two
// strikes, recovered, and later relapsed should get the full allowance again
// -- if the count merely persisted, the first relapse would cross the
// threshold immediately and autoheal would give up on something that has
// failed once since it was last well.
func TestStrikesBelowTheThresholdAreClearedOnRecovery(t *testing.T) {
	s := Up(t, "autoheal-crashloop/gives-up")
	s.Start()

	// Exactly as many strikes as autoheal tolerates, one at a time: the gate
	// is cleared after each restart so the container genuinely recovers, then
	// tripped again. Left tripped, these would be a crash loop and would cross
	// the threshold.
	//
	// The count comes from the same .env compose read, so this stays the
	// interesting case -- one short of giving up -- if the fixture changes.
	//
	// Each recovery is waited on twice, and both waits are load-bearing.
	// MakeWell returns when *docker* reports the container healthy; autoheal
	// polls on its own clock and has not necessarily looked yet. Tripping the
	// gate again before it did would show it two unhealthy states in a row --
	// a crash loop, and a test that fails for a reason that has nothing to do
	// with what it is testing.
	for i := 0; i < s.SettingInt("CRASHLOOP_STRIKES"); i++ {
		at := s.StartedAt("looper")
		cleared := strings.Count(s.Logs("autoheal"), clearedHistory)

		s.MakeSick("looper")
		s.waitFor("looper to be restarted", 60*time.Second,
			func() bool { return s.StartedAt("looper").After(at) },
			func() string { return s.Logs("autoheal") })

		s.MakeWell("looper")
		s.waitFor("autoheal to notice looper recovered", 60*time.Second,
			func() bool { return strings.Count(s.Logs("autoheal"), clearedHistory) > cleared },
			func() string { return s.Logs("autoheal") })
	}

	// Never given up on: crossing the threshold takes one more restart than
	// that, with no recovery in between.
	if logs := s.Logs("autoheal"); strings.Contains(logs, gaveUp) {
		t.Fatalf("autoheal gave up after strikes that were each followed by a recovery:\n%s", logs)
	}

	// The proof: a relapse now gets restarted rather than written off. Had the
	// two earlier strikes survived, this third one would cross the threshold.
	at := s.StartedAt("looper")
	s.MakeSick("looper")
	s.waitFor("looper to be restarted after its strikes were cleared", 60*time.Second,
		func() bool { return s.StartedAt("looper").After(at) },
		func() string { return s.Logs("autoheal") })
}

// The threshold is a setting, so it has to be honoured rather than assumed.
//
// This is the same scenario as TestCrashLoopIsGivenUpOn against a fixture
// whose .env asks for more patience. The pair is the test: a threshold that
// was hardcoded, off by one, or read once and cached passes against one of
// them and fails against the other. Neither alone would notice.
func TestTheCrashLoopThresholdIsHonoured(t *testing.T) {
	s := Up(t, "autoheal-crashloop/patient-threshold")
	s.Start()

	strikes := s.SettingInt("CRASHLOOP_STRIKES")

	at := s.StartedAt("looper")
	s.MakeSick("looper")

	// It must get this far, which is the half that fails if autoheal gives up
	// too early.
	s.WaitForRestarts("looper", at, strikes)

	// And no further, which is the half that fails if it ignores the setting
	// and keeps going.
	settled := s.StartedAt("looper")
	time.Sleep(6 * time.Second)

	logs := s.Logs("autoheal")

	if got := s.StartedAt("looper"); got.After(settled) {
		t.Errorf("looper was still being restarted after %d attempts, at %s\n%s",
			strikes, got, logs)
	}
	if n := strings.Count(logs, gaveUp); n != 1 {
		t.Errorf("autoheal should give up once and say so once, said it %d times:\n%s", n, logs)
	}
}
