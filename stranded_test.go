package main

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeRecreate stands in for recreateService and a clock for now, restoring
// both when the test ends. Each call to recreate pops the next scripted
// result; running out means success.
type fakeRecreate struct {
	calls   []string
	results []error
	clock   time.Time
}

func useFakeRecreate(t *testing.T) *fakeRecreate {
	t.Helper()
	f := &fakeRecreate{clock: time.Date(2026, 10, 20, 3, 36, 0, 0, time.UTC)}
	oldRecreate, oldNow := recreateService, now
	recreateService = func(_ zerolog.Logger, name string, _ map[string]string) error {
		f.calls = append(f.calls, name)
		if len(f.results) == 0 {
			return nil
		}
		err := f.results[0]
		f.results = f.results[1:]
		return err
	}
	now = func() time.Time { return f.clock }
	t.Cleanup(func() { recreateService, now = oldRecreate, oldNow })
	return f
}

func recreateLabeled(id, name string) containerSummary {
	return containerSummary{
		ID: id, Names: []string{"/" + name}, State: "running",
		Labels: map[string]string{"autoheal": "true", "autoheal.mode": "recreate"},
	}
}

var cfgLabel = config{ContainerLabel: "autoheal"}

// The incident this exists for: the recreate fails after compose has replaced
// the container, the new one sits in "created" with no health status, and the
// unhealthy list never shows it again. It must still be retried.
func TestStranded_FailedRecreateIsRetriedOnceOffTheUnhealthyList(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("dependency failed to start")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)

	// Compose got as far as creating the replacement, then failed.
	api.containers = nil
	api.states["qbittorrent"] = "created"
	api.newIDs["qbittorrent"] = "new"

	rc.clock = rc.clock.Add(strandedRetryEvery)
	runOnce(testLogger(), api, cfgLabel, strikes)

	if len(rc.calls) != 2 {
		t.Fatalf("expected the failed recreate to be retried, got calls %v", rc.calls)
	}
	if _, still := strikes.strandedServices()["qbittorrent"]; still {
		t.Errorf("a successful retry should forget the service")
	}
}

// The container is gone entirely when the failure came between remove and
// create. That is still a service to bring back, not one to forget.
func TestStranded_MissingContainerIsRetried(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("boom")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)
	api.containers = nil

	rc.clock = rc.clock.Add(strandedRetryEvery)
	runOnce(testLogger(), api, cfgLabel, strikes)

	if len(rc.calls) != 2 {
		t.Errorf("expected a retry for a missing container, got calls %v", rc.calls)
	}
}

// A failed retry keeps it stranded and tries again later.
func TestStranded_FailedRetryKeepsTrying(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("a"), errors.New("b"), errors.New("c")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"

	for i := 0; i < 2; i++ {
		rc.clock = rc.clock.Add(strandedRetryEvery)
		runOnce(testLogger(), api, cfgLabel, strikes)
	}
	if len(rc.calls) != 3 {
		t.Errorf("expected two retries, got calls %v", rc.calls)
	}
	if _, still := strikes.strandedServices()["qbittorrent"]; !still {
		t.Errorf("a service whose retries keep failing must stay stranded")
	}
	// Retries are not strikes: a service waiting on its dependency must not
	// be written off as a crash loop.
	if strikes.isGivenUp("new") || strikes.isGivenUp("old") {
		t.Errorf("stranded retries were counted as crash-loop strikes")
	}
}

// --- the refusing cases ---

// Polls are every five seconds; retries are not.
func TestStranded_NotRetriedBeforeTheInterval(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("boom")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"

	rc.clock = rc.clock.Add(strandedRetryEvery - time.Second)
	runOnce(testLogger(), api, cfgLabel, strikes)

	if len(rc.calls) != 1 {
		t.Errorf("retried before the interval: calls %v", rc.calls)
	}
}

// Somebody started it by hand -- which is what happened in the incident. It is
// running, so it is forgotten and never recreated out from under them.
func TestStranded_RunningAgainIsForgottenNotRecreated(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("boom")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "running"

	rc.clock = rc.clock.Add(strandedRetryEvery)
	runOnce(testLogger(), api, cfgLabel, strikes)
	rc.clock = rc.clock.Add(strandedRetryEvery)
	api.states["qbittorrent"] = "exited"
	runOnce(testLogger(), api, cfgLabel, strikes)

	if len(rc.calls) != 1 {
		t.Errorf("a service that came back was recreated anyway: calls %v", rc.calls)
	}
}

func TestStranded_RestartingOrPausedIsLeftAlone(t *testing.T) {
	for _, state := range []string{"restarting", "paused"} {
		t.Run(state, func(t *testing.T) {
			rc := useFakeRecreate(t)
			rc.results = []error{errors.New("boom")}
			api := newFakeDockerAPI()
			strikes := newStrikeTracker(3)

			api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
			runOnce(testLogger(), api, cfgLabel, strikes)
			api.containers = nil
			api.states["qbittorrent"] = state

			rc.clock = rc.clock.Add(strandedRetryEvery)
			runOnce(testLogger(), api, cfgLabel, strikes)

			if len(rc.calls) != 1 {
				t.Errorf("%s container was recreated: calls %v", state, rc.calls)
			}
			if _, still := strikes.strandedServices()["qbittorrent"]; !still {
				t.Errorf("%s container was forgotten before it was running", state)
			}
		})
	}
}

// A successful recreate strands nothing, and a failed restart strands nothing
// either: the restarted container is still there to be listed again.
func TestStranded_OnlyAFailedRecreateStrands(t *testing.T) {
	t.Run("successful recreate", func(t *testing.T) {
		useFakeRecreate(t)
		api := newFakeDockerAPI()
		api.newIDs["qbittorrent"] = "new"
		strikes := newStrikeTracker(3)
		api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
		runOnce(testLogger(), api, cfgLabel, strikes)
		if len(strikes.strandedServices()) != 0 {
			t.Errorf("stranded after a successful recreate: %v", strikes.strandedServices())
		}
	})
	t.Run("failed restart", func(t *testing.T) {
		rc := useFakeRecreate(t)
		api := newFakeDockerAPI()
		api.restartErr["aaa"] = errors.New("boom")
		strikes := newStrikeTracker(3)
		api.containers = []containerSummary{unhealthy("aaa", "nginx")}
		runOnce(testLogger(), api, cfgLabel, strikes)
		if len(strikes.strandedServices()) != 0 || len(rc.calls) != 0 {
			t.Errorf("a failed restart was treated as a stranded recreate")
		}
	})
}
