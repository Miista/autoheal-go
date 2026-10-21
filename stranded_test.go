package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

	rc.clock = rc.clock.Add(firstRetry)
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

	rc.clock = rc.clock.Add(firstRetry)
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

	for i := 1; i <= 2; i++ {
		rc.clock = rc.clock.Add(retryAfter(defaultStrandedIntervals, i))
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

	rc.clock = rc.clock.Add(firstRetry - time.Second)
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

	rc.clock = rc.clock.Add(firstRetry)
	runOnce(testLogger(), api, cfgLabel, strikes)
	rc.clock = rc.clock.Add(firstRetry)
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

			rc.clock = rc.clock.Add(firstRetry)
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

// --- back-off and the one notice ---

// The defaults, and a list somebody set: each failure moves one entry along,
// and the last entry is used for as long as it takes.
func TestRetryAfter_WalksTheListThenRepeatsTheLast(t *testing.T) {
	cases := []struct {
		name      string
		intervals []time.Duration
		attempts  int
		want      time.Duration
	}{
		{"default first", defaultStrandedIntervals, 1, 30 * time.Second},
		{"default second", defaultStrandedIntervals, 2, time.Minute},
		{"default third", defaultStrandedIntervals, 3, 2 * time.Minute},
		{"default past the end", defaultStrandedIntervals, 4, 2 * time.Minute},
		{"default far past the end", defaultStrandedIntervals, 1000, 2 * time.Minute},
		{"custom list", []time.Duration{time.Second, 5 * time.Second}, 2, 5 * time.Second},
		{"custom list past the end", []time.Duration{time.Second, 5 * time.Second}, 9, 5 * time.Second},
		{"a single interval is a fixed retry", []time.Duration{10 * time.Second}, 50, 10 * time.Second},
		{"unset means the defaults", nil, 1, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfter(tc.intervals, tc.attempts); got != tc.want {
				t.Errorf("retryAfter(%v, %d) = %s, want %s", tc.intervals, tc.attempts, got, tc.want)
			}
		})
	}
}

func TestParseIntervals(t *testing.T) {
	got, err := parseIntervals("30s, 1m,2m")
	if err != nil || len(got) != 3 || got[0] != 30*time.Second || got[1] != time.Minute || got[2] != 2*time.Minute {
		t.Errorf("parseIntervals(\"30s, 1m,2m\") = %v, %v", got, err)
	}
	// Refused rather than quietly shortened: zero would be a busy loop.
	for _, bad := range []string{"", "abc", "30s,,1m", "30s,xyz", "0s", "30s,-1m", "30"} {
		if got, err := parseIntervals(bad); err == nil {
			t.Errorf("parseIntervals(%q) = %v, want an error", bad, got)
		}
	}
}

// A setting that cannot be read falls back to the defaults, not to nothing.
func TestGetEnvIntervals_BadValueFallsBackToDefaults(t *testing.T) {
	t.Setenv("AUTOHEAL_TEST_INTERVALS", "30s,nonsense")
	got := getEnvIntervals(testLogger(), "AUTOHEAL_TEST_INTERVALS", defaultStrandedIntervals)
	if len(got) != len(defaultStrandedIntervals) || got[0] != defaultStrandedIntervals[0] {
		t.Errorf("a bad list produced %v, want the defaults", got)
	}
	t.Setenv("AUTOHEAL_TEST_INTERVALS", "1s,2s")
	if got := getEnvIntervals(testLogger(), "AUTOHEAL_TEST_INTERVALS", defaultStrandedIntervals); len(got) != 2 || got[1] != 2*time.Second {
		t.Errorf("a good list produced %v", got)
	}
}

// The list in cfg is what paces retries, not the defaults: with one-second
// intervals a retry is due a second later, and not before.
func TestStranded_ConfiguredIntervalsPaceTheRetries(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("a"), errors.New("b"), errors.New("c")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)
	cfg := cfgLabel
	cfg.StrandedRetryIntervals = []time.Duration{time.Second, 3 * time.Second}

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfg, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"

	rc.clock = rc.clock.Add(time.Second)
	runOnce(testLogger(), api, cfg, strikes) // 1s after the first failure: due
	rc.clock = rc.clock.Add(2 * time.Second)
	runOnce(testLogger(), api, cfg, strikes) // second entry is 3s: not yet
	if len(rc.calls) != 2 {
		t.Fatalf("expected the second retry to wait for the second interval, calls %v", rc.calls)
	}
	rc.clock = rc.clock.Add(time.Second)
	runOnce(testLogger(), api, cfg, strikes)
	if len(rc.calls) != 3 {
		t.Errorf("expected a retry once 3s had passed, calls %v", rc.calls)
	}
}

// The notice threshold in cfg, likewise: shorter than the default, it fires
// sooner; and an unset value is the default, not "immediately".
func TestStranded_ConfiguredNoticeThreshold(t *testing.T) {
	rc := useFakeRecreate(t)
	cfg, count := webhookCount(t)
	cfg.StrandedNotifyAfter = 10 * time.Second
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)
	strandOnce(t, rc, api, cfg, strikes, 40)
	base := count()

	rc.clock = rc.clock.Add(9 * time.Second)
	runOnce(testLogger(), api, cfg, strikes)
	if got := count() - base; got != 0 {
		t.Fatalf("noticed after 9s with a 10s threshold: %d", got)
	}
	rc.clock = rc.clock.Add(time.Second)
	runOnce(testLogger(), api, cfg, strikes)
	if got := count() - base; got != 1 {
		t.Errorf("no notice after the configured 10s: %d", got)
	}
}

var (
	firstRetry = defaultStrandedIntervals[0]
	lastRetry  = defaultStrandedIntervals[len(defaultStrandedIntervals)-1]
)

// The reason back-off is safe: it slows retries, it never ends them. After a
// long run of failures it still tries again, and at the capped interval.
func TestStranded_NeverStopsRetryingAfterManyFailures(t *testing.T) {
	rc := useFakeRecreate(t)
	for i := 0; i < 40; i++ {
		rc.results = append(rc.results, errors.New("dependency failed to start"))
	}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"

	for i := 0; i < 39; i++ {
		rc.clock = rc.clock.Add(lastRetry)
		runOnce(testLogger(), api, cfgLabel, strikes)
	}
	if len(rc.calls) != 40 {
		t.Errorf("expected an attempt every capped interval, got %d calls", len(rc.calls))
	}
	if strikes.isGivenUp("old") || strikes.isGivenUp("new") {
		t.Errorf("a long stranding was counted as a crash loop")
	}
	rc.clock = rc.clock.Add(lastRetry)
	runOnce(testLogger(), api, cfgLabel, strikes)
	if len(rc.calls) != 41 {
		t.Errorf("stopped retrying after 40 failures: %d calls", len(rc.calls))
	}
}

// Back-off: a retry that is due on the first schedule step is not yet due once
// failures have pushed the wait out.
func TestStranded_WaitGrowsWithFailures(t *testing.T) {
	rc := useFakeRecreate(t)
	rc.results = []error{errors.New("a"), errors.New("b")}
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfgLabel, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"

	rc.clock = rc.clock.Add(retryAfter(defaultStrandedIntervals, 1))
	runOnce(testLogger(), api, cfgLabel, strikes) // second failure
	rc.clock = rc.clock.Add(retryAfter(defaultStrandedIntervals, 1))
	runOnce(testLogger(), api, cfgLabel, strikes) // only 30s later: too soon

	if len(rc.calls) != 2 {
		t.Errorf("retried at the base interval after a second failure: calls %v", rc.calls)
	}
	rc.clock = rc.clock.Add(retryAfter(defaultStrandedIntervals, 2) - retryAfter(defaultStrandedIntervals, 1))
	runOnce(testLogger(), api, cfgLabel, strikes)
	if len(rc.calls) != 3 {
		t.Errorf("not retried once the longer wait passed: calls %v", rc.calls)
	}
}

// Every failed attempt is logged, however long the stranding goes on, even
// though the notification goes out only once.
func TestStranded_EveryFailedRetryIsLogged(t *testing.T) {
	rc := useFakeRecreate(t)
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	for i := 0; i < 12; i++ {
		rc.results = append(rc.results, errors.New("dependency failed to start"))
	}
	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(log, api, cfgLabel, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"

	for i := 0; i < 10; i++ {
		rc.clock = rc.clock.Add(lastRetry)
		runOnce(log, api, cfgLabel, strikes)
	}
	if got := strings.Count(buf.String(), "is still down"); got != 10 {
		t.Errorf("expected a log line for each of the 10 failed retries, got %d\n%s", got, buf.String())
	}
}

// webhookCount stands up a webhook and counts what arrives.
func webhookCount(t *testing.T) (config, func() int) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
	}))
	t.Cleanup(srv.Close)
	cfg := cfgLabel
	cfg.WebhookURL, cfg.WebhookJSONKey = srv.URL, "text"
	return cfg, func() int {
		time.Sleep(150 * time.Millisecond) // notify runs in a goroutine
		return int(n.Load())
	}
}

func strandOnce(t *testing.T, rc *fakeRecreate, api *fakeDockerAPI, cfg config, strikes *strikeTracker, failures int) {
	t.Helper()
	for i := 0; i < failures; i++ {
		rc.results = append(rc.results, errors.New("dependency failed to start"))
	}
	api.containers = []containerSummary{recreateLabeled("old", "qbittorrent")}
	runOnce(testLogger(), api, cfg, strikes)
	api.containers = nil
	api.states["qbittorrent"] = "created"
}

// Once the service has been down long enough, say so -- one time, however
// many retries follow.
func TestStranded_NotifiesOnceWhenDownLong(t *testing.T) {
	rc := useFakeRecreate(t)
	cfg, count := webhookCount(t)
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)
	strandOnce(t, rc, api, cfg, strikes, 40)
	base := count() // runOnce already reports the first failed heal

	for i := 0; i < 30; i++ {
		rc.clock = rc.clock.Add(lastRetry)
		runOnce(testLogger(), api, cfg, strikes)
	}
	if got := count() - base; got != 1 {
		t.Errorf("expected exactly one notice for a long stranding, got %d", got)
	}
}

// A blip shorter than the threshold is the dependency restarting, not news.
func TestStranded_NoNoticeForAShortStranding(t *testing.T) {
	rc := useFakeRecreate(t)
	cfg, count := webhookCount(t)
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)
	strandOnce(t, rc, api, cfg, strikes, 1) // the retry after this one succeeds
	base := count()

	rc.clock = rc.clock.Add(defaultStrandedNotifyAfter - time.Second)
	runOnce(testLogger(), api, cfg, strikes)
	if got := count() - base; got != 1 { // only the "has now been recreated" notice
		t.Errorf("a short stranding sent %d notifications, want just the recovery notice", got)
	}
}

// Said while it is down, not about something somebody already fixed.
func TestStranded_NoNoticeOnceItIsRunningAgain(t *testing.T) {
	rc := useFakeRecreate(t)
	cfg, count := webhookCount(t)
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)
	strandOnce(t, rc, api, cfg, strikes, 40)
	base := count()

	api.states["qbittorrent"] = "running"
	rc.clock = rc.clock.Add(2 * defaultStrandedNotifyAfter)
	runOnce(testLogger(), api, cfg, strikes)
	if got := count() - base; got != 0 {
		t.Errorf("notified about a service that is running again: %d", got)
	}
}

// Recovery re-arms it: a separate, later stranding gets its own notice.
func TestStranded_NoticeRearmsAfterRecovery(t *testing.T) {
	rc := useFakeRecreate(t)
	cfg, count := webhookCount(t)
	api := newFakeDockerAPI()
	strikes := newStrikeTracker(3)

	strandOnce(t, rc, api, cfg, strikes, 40)
	base := count()
	rc.clock = rc.clock.Add(defaultStrandedNotifyAfter)
	runOnce(testLogger(), api, cfg, strikes)
	if got := count() - base; got != 1 {
		t.Fatalf("first stranding: %d notices, want 1", got)
	}

	api.states["qbittorrent"] = "running"
	runOnce(testLogger(), api, cfg, strikes) // forgotten
	if _, still := strikes.strandedServices()["qbittorrent"]; still {
		t.Fatalf("still stranded after it was running")
	}

	// A failed recreate leaves the replacement in "created", not "running".
	api.states["qbittorrent"] = "created"
	rc.results = nil
	strandOnce(t, rc, api, cfg, strikes, 40)
	base = count()
	rc.clock = rc.clock.Add(defaultStrandedNotifyAfter)
	runOnce(testLogger(), api, cfg, strikes)
	if got := count() - base; got != 1 {
		t.Errorf("second stranding: %d notices, want its own 1", got)
	}
}
