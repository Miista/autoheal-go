package main

import (
	"fmt"
	"io"
	"testing"

	"github.com/rs/zerolog"
)

// fakeDockerAPI is a scripted Docker seam: a plain struct with recorded
// calls and injectable errors, no mocking library.
type fakeDockerAPI struct {
	containers []containerSummary

	listErr    error
	restartErr map[string]error // container ID -> error
	healthy    map[string]bool  // container ID -> isHealthy() result
	healthErr  map[string]error

	restarted []string // container IDs, in call order
}

func newFakeDockerAPI() *fakeDockerAPI {
	return &fakeDockerAPI{
		restartErr: map[string]error{},
		healthy:    map[string]bool{},
		healthErr:  map[string]error{},
	}
}

func (f *fakeDockerAPI) listUnhealthy(config) ([]containerSummary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.containers, nil
}

func (f *fakeDockerAPI) restart(containerID string, _ int) error {
	f.restarted = append(f.restarted, containerID)
	return f.restartErr[containerID]
}

func (f *fakeDockerAPI) isHealthy(containerID string) (bool, error) {
	if err := f.healthErr[containerID]; err != nil {
		return false, err
	}
	return f.healthy[containerID], nil
}

func testLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

func unhealthy(id, name string) containerSummary {
	return containerSummary{ID: id, Names: []string{"/" + name}, State: "running"}
}

func TestRunOnce_UnhealthyContainerIsRestarted(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}

	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, newStrikeTracker(3))

	if len(client.restarted) != 1 || client.restarted[0] != "abc" {
		t.Fatalf("expected exactly one restart of abc, got %v", client.restarted)
	}
}

func TestRunOnce_UnhealthyContainerAddsAStrike(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}
	strikes := newStrikeTracker(3)

	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, strikes)

	if strikes.isGivenUp("abc") {
		t.Fatal("one strike should not be given-up yet (threshold 3)")
	}
	// Confirm the strike was actually recorded, not silently skipped: two
	// more unhealthy cycles should cross the threshold.
	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, strikes)
	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, strikes)
	if !strikes.isGivenUp("abc") {
		t.Fatal("three consecutive unhealthy restarts should have crossed threshold 3")
	}
}

func TestRunOnce_FailedRestartDoesNotAddAStrike(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}
	client.restartErr["abc"] = fmt.Errorf("connection refused")
	strikes := newStrikeTracker(1)

	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 1}, strikes)

	if strikes.isGivenUp("abc") {
		t.Fatal("a restart call that itself failed should not count as a strike — we never completed a heal attempt")
	}
}

func TestRunOnce_CrossingThresholdStopsFurtherRestarts(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}
	strikes := newStrikeTracker(2)
	cfg := config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 2}

	runOnce(testLogger(), client, cfg, strikes) // strike 1
	runOnce(testLogger(), client, cfg, strikes) // strike 2 -> crosses, given up
	if len(client.restarted) != 2 {
		t.Fatalf("expected 2 restarts before giving up, got %d", len(client.restarted))
	}

	// Container is still reported unhealthy on every subsequent cycle —
	// autoheal must NOT attempt a third restart once given up.
	runOnce(testLogger(), client, cfg, strikes)
	runOnce(testLogger(), client, cfg, strikes)
	if len(client.restarted) != 2 {
		t.Fatalf("expected no further restarts after giving up, got %d total", len(client.restarted))
	}
}

func TestRunOnce_RestartingContainerIsSkippedAndNotStruck(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{{ID: "abc", Names: []string{"/app"}, State: "restarting"}}
	strikes := newStrikeTracker(3)

	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, strikes)

	if len(client.restarted) != 0 {
		t.Fatalf("a container already restarting should not get a second restart call, got %v", client.restarted)
	}
	tracked := strikes.tracked()
	if tracked["abc"] {
		t.Fatal("a skipped 'restarting' container should not accrue a strike")
	}
}

func TestRunOnce_GenuinelyHealthyClearsStrikes(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}
	strikes := newStrikeTracker(3)
	cfg := config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}

	runOnce(testLogger(), client, cfg, strikes) // 1 strike

	// Now the container drops out of the unhealthy list (recovered) and
	// isHealthy confirms it.
	client.containers = nil
	client.healthy["abc"] = true
	runOnce(testLogger(), client, cfg, strikes)

	tracked := strikes.tracked()
	if tracked["abc"] {
		t.Fatal("expected strikes cleared once genuinely healthy")
	}
}

func TestRunOnce_MissingFromUnhealthyListButNotYetHealthyKeepsStrikes(t *testing.T) {
	// Regression test for the "starting" health-state bug found live: a
	// container can disappear from the unhealthy-filtered list without
	// actually being healthy yet (Docker's transient "starting" state).
	// Strikes must survive until isHealthy explicitly confirms recovery.
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}
	strikes := newStrikeTracker(3)
	cfg := config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}

	runOnce(testLogger(), client, cfg, strikes) // 1 strike

	client.containers = nil
	client.healthy["abc"] = false // still not confirmed healthy (e.g. "starting")
	runOnce(testLogger(), client, cfg, strikes)

	tracked := strikes.tracked()
	if !tracked["abc"] {
		t.Fatal("strikes should survive while health is not yet confirmed, even though the container left the unhealthy list")
	}
}

func TestRunOnce_GivenUpContainerStillPolledForRecovery(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{unhealthy("abc", "app")}
	strikes := newStrikeTracker(1)
	cfg := config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 1}

	runOnce(testLogger(), client, cfg, strikes) // 1 strike -> given up immediately
	if !strikes.isGivenUp("abc") {
		t.Fatal("expected given-up after crossing threshold 1")
	}

	// Container disappears from the unhealthy list (recovered) and is
	// confirmed genuinely healthy — even though given up, it must still be
	// checked and cleared, per the "always keep checking" requirement.
	client.containers = nil
	client.healthy["abc"] = true
	runOnce(testLogger(), client, cfg, strikes)

	if strikes.isGivenUp("abc") {
		t.Fatal("a given-up container confirmed healthy should have its given-up state cleared")
	}

	// Relapse: unhealthy again — should get a fresh restart attempt, not
	// stay silently suppressed.
	client.containers = []containerSummary{unhealthy("abc", "app")}
	runOnce(testLogger(), client, cfg, strikes)
	if len(client.restarted) != 2 {
		t.Fatalf("expected a fresh restart attempt after recovery+relapse, got %d total restarts", len(client.restarted))
	}
}

func TestRunOnce_LabeledFalseIsNeverRestarted(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{{
		ID: "abc", Names: []string{"/app"}, State: "running",
		Labels: map[string]string{"autoheal": "False"},
	}}

	runOnce(testLogger(), client, config{ContainerLabel: "autoheal", DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, newStrikeTracker(3))

	if len(client.restarted) != 0 {
		t.Fatalf("a container explicitly labeled autoheal=False must never be restarted, got %v", client.restarted)
	}
}

func TestRunOnce_NamelessContainerIsSkipped(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{{ID: "abc", Names: nil, State: "running"}}

	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, newStrikeTracker(3))

	if len(client.restarted) != 0 {
		t.Fatalf("a nameless container (implies it no longer exists) must not be restarted, got %v", client.restarted)
	}
}

func TestRunOnce_PerContainerStopTimeoutLabelOverridesDefault(t *testing.T) {
	client := newFakeDockerAPI()
	client.containers = []containerSummary{{
		ID: "abc", Names: []string{"/app"}, State: "running",
		Labels: map[string]string{"autoheal.stop.timeout": "42"},
	}}

	// The fake doesn't record the timeout argument directly, so this test
	// only proves restart is still invoked when a per-container override is
	// present — the value itself is exercised by hack/e2e.sh against the
	// real Docker API, where the timeout affects real restart behavior.
	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, newStrikeTracker(3))

	if len(client.restarted) != 1 {
		t.Fatalf("expected restart despite per-container timeout override, got %v", client.restarted)
	}
}

func TestRunOnce_ListErrorSkipsCycleWithoutPanicking(t *testing.T) {
	client := newFakeDockerAPI()
	client.listErr = fmt.Errorf("docker daemon unreachable")

	runOnce(testLogger(), client, config{DefaultStopTimeout: 10, CrashLoopStrikeThreshold: 3}, newStrikeTracker(3))

	if len(client.restarted) != 0 {
		t.Fatalf("a list error should skip the cycle entirely, got restarts %v", client.restarted)
	}
}
