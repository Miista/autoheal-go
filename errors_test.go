package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// newCapturingWebhook stands up a webhook endpoint that hands the text of each
// notification back on a channel, and returns its URL. Notifications are sent
// from their own goroutine, so a channel rather than a captured variable is
// what makes the assertion wait for one to arrive instead of racing it.
func newCapturingWebhook(t *testing.T, into chan<- string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			into <- string(body)
			return
		}
		for _, v := range payload {
			into <- v
			return
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The daemon explains itself in the response body, and a status code alone is
// not an answer to "why did the restart fail" (upstream/111). Every branch here
// is a body autoheal could actually be handed, including the ones that are not
// JSON at all -- a proxy in front of the socket, a truncated response -- since
// this runs on the error path and must not fail a second time while reporting
// the first.
func TestDockerErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "the daemon's own error shape",
			body: `{"message":"No such container: abc123"}`,
			want: "No such container: abc123",
		},
		{
			name: "message alongside other fields",
			body: `{"message":"device or resource busy","other":1}`,
			want: "device or resource busy",
		},
		{
			// A reverse proxy or a socket-permission layer answering instead
			// of the daemon: still worth repeating verbatim.
			body: "502 Bad Gateway",
			name: "not JSON at all",
			want: "502 Bad Gateway",
		},
		{
			name: "JSON without a message field",
			body: `{"error":"something"}`,
			want: `{"error":"something"}`,
		},
		{
			name: "empty body",
			body: "",
			want: "no detail given",
		},
		{
			name: "whitespace only",
			body: "   \n",
			want: "no detail given",
		},
		{
			name: "explicitly empty message falls back to the raw body",
			body: `{"message":""}`,
			want: `{"message":""}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerErrorMessage(strings.NewReader(tc.body)); got != tc.want {
				t.Errorf("dockerErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// A body large enough to be a mistake is read but bounded -- autoheal should
// not hold an arbitrary amount of somebody else's HTML in memory to quote it
// back, and truncating is better than refusing to report at all.
func TestDockerErrorMessageIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 64<<10)
	got := dockerErrorMessage(strings.NewReader(huge))
	if len(got) > 8<<10 {
		t.Errorf("read %d bytes of the error body, want it capped at 8KiB", len(got))
	}
	if got == "" {
		t.Error("a large body produced no message at all")
	}
}

// The reason has to reach the notification, not only the log: whoever is
// paged cannot go and grep, and the daemon's message is usually the entire
// diagnosis.
func TestRestartFailureNotificationCarriesTheReason(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	received := make(chan string, 1)
	srv := newCapturingWebhook(t, received)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{unhealthy("abc", "app")}
	api.restartErr["abc"] = fmt.Errorf("docker API returned 409 restarting abc: container is marked for removal")

	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10, WebhookURL: srv, WebhookJSONKey: "text"}
	runOnce(log, api, cfg, newStrikeTracker(3))

	got := <-received
	if !strings.Contains(got, "container is marked for removal") {
		t.Errorf("the alert did not say why the restart failed:\n%s", got)
	}
	if !strings.Contains(buf.String(), "container is marked for removal") {
		t.Errorf("the log did not say why the restart failed:\n%s", buf.String())
	}
}
