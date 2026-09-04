package main

import (
	"os"
	"strings"
	"testing"
)

// Container names repeat across hosts -- everybody has an nginx -- so an alert
// that one was restarted does not say where it happened (upstream/118).
func TestNotificationsAreStampedWithTheHost(t *testing.T) {
	received := make(chan string, 1)
	srv := newCapturingWebhook(t, received)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{unhealthy("abc", "nginx")}

	cfg := config{
		ContainerLabel:     "all",
		DefaultStopTimeout: 10,
		WebhookURL:         srv,
		WebhookJSONKey:     "text",
		NotifyPrefix:       "prod-db-1",
	}
	runOnce(testLogger(), api, cfg, newStrikeTracker(3))

	got := <-received
	if !strings.HasPrefix(got, "[prod-db-1] ") {
		t.Errorf("the alert does not say which host it came from: %q", got)
	}
	if !strings.Contains(got, "nginx") {
		t.Errorf("stamping the host lost the message: %q", got)
	}
}

// An empty prefix turns the stamping off rather than producing an empty pair
// of brackets.
func TestEmptyPrefixLeavesNotificationsAlone(t *testing.T) {
	received := make(chan string, 1)
	srv := newCapturingWebhook(t, received)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{unhealthy("abc", "nginx")}

	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10, WebhookURL: srv, WebhookJSONKey: "text"}
	runOnce(testLogger(), api, cfg, newStrikeTracker(3))

	if got := <-received; strings.HasPrefix(got, "[") {
		t.Errorf("an unset prefix still stamped the alert: %q", got)
	}
}

// "nginx was restarted" does not say why. The healthcheck already worked out
// the diagnosis and it was being discarded (upstream/81).
func TestNotificationCarriesTheHealthcheckOutput(t *testing.T) {
	received := make(chan string, 1)
	srv := newCapturingWebhook(t, received)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{unhealthy("abc", "nginx")}
	api.healthOutput["abc"] = "curl: (7) Failed to connect to localhost port 8080"

	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10, WebhookURL: srv, WebhookJSONKey: "text"}
	runOnce(testLogger(), api, cfg, newStrikeTracker(3))

	got := <-received
	if !strings.Contains(got, "Failed to connect to localhost port 8080") {
		t.Errorf("the alert did not say what the healthcheck reported: %q", got)
	}
}

// A healthcheck of `exit 1` prints nothing, which is common. That must read as
// an ordinary alert, not one with an empty explanation hanging off it.
func TestSilentHealthcheckLeavesNoDanglingClause(t *testing.T) {
	received := make(chan string, 1)
	srv := newCapturingWebhook(t, received)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{unhealthy("abc", "nginx")}

	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10, WebhookURL: srv, WebhookJSONKey: "text"}
	runOnce(testLogger(), api, cfg, newStrikeTracker(3))

	if got := <-received; strings.Contains(got, "healthcheck said") {
		t.Errorf("a healthcheck that printed nothing still produced an explanation: %q", got)
	}
}

func TestSummarizeHealthOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"ordinary one-liner", "connection refused", "connection refused"},
		{"leading blank lines are skipped", "\n\n  real message\n", "real message"},
		{
			// Alerts go to phones and chat clients; a stack trace must not
			// arrive as one, and the first line is the useful part.
			name: "only the first non-empty line",
			in:   "fatal: cannot connect\n  at foo()\n  at bar()",
			want: "fatal: cannot connect",
		},
		{"nothing printed", "", ""},
		{"whitespace only", "  \n\t\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeHealthOutput(tc.in); got != tc.want {
				t.Errorf("summarizeHealthOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A probe that prints a page of output must not become a page-long alert.
func TestSummarizeHealthOutputIsCapped(t *testing.T) {
	got := summarizeHealthOutput(strings.Repeat("x", 5000))
	if len([]rune(got)) > 201 {
		t.Errorf("a long healthcheck output was not trimmed: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a trimmed output should show that it was trimmed, got %q", got[len(got)-10:])
	}
}

// The prefix defaults to the hostname but is overridable, including to
// nothing: someone whose alerts already carry the host (a webhook that adds
// it, a single-host setup) should be able to turn the stamping off rather
// than live with it doubled.
func TestNotifyPrefixResolution(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname available on this machine: %v", err)
	}

	for _, tc := range []struct {
		name string
		set  bool
		vlue string
		want string
	}{
		{name: "unset falls back to the hostname", set: false, want: host},
		{name: "an explicit name wins", set: true, vlue: "prod-db-1", want: "prod-db-1"},
		{name: "empty turns the stamping off", set: true, vlue: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("AUTOHEAL_NOTIFY_PREFIX", tc.vlue)
			} else {
				os.Unsetenv("AUTOHEAL_NOTIFY_PREFIX")
			}
			if got := notifyPrefix(testLogger()); got != tc.want {
				t.Errorf("notifyPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}
