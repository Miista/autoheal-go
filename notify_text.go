package main

// How a notification reads: where it came from and why it was sent.
//
// Separate from the sending in main.go, which is about getting bytes to a
// webhook, and from the deciding in runOnce, which is about whether to act.
// This is only the wording -- the part a person actually reads at three in the
// morning, and the part upstream/118 and upstream/81 are both complaints
// about.

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// notifyPrefix is what notifications are stamped with to say which machine
// they came from (upstream/118).
//
// AUTOHEAL_NOTIFY_PREFIX names it explicitly, for people who would rather see
// "prod-db-1" than whatever the kernel thinks. Unset, the hostname is used --
// which inside a container is its short id unless the operator set one, and
// that is still enough to tell two hosts apart. Set it to the empty string to
// turn the stamping off.
func notifyPrefix(log zerolog.Logger) string {
	if v, ok := os.LookupEnv("AUTOHEAL_NOTIFY_PREFIX"); ok {
		return v
	}
	host, err := os.Hostname()
	if err != nil {
		log.Warn().Err(err).Msg("Couldn't read the hostname, so notifications won't say which host they came from")
		return ""
	}
	return host
}

// summarizeHealthOutput makes a healthcheck's output fit in a notification.
//
// The output is whatever the probe command printed, which can be a stack trace
// or a page of curl progress; alerts go to phones and chat channels, so this
// takes the first non-empty line and caps it. Newlines are collapsed rather
// than kept -- a multi-line body would break the plain-text ntfy format and
// bury the container name in most chat clients.
func summarizeHealthOutput(out string) string {
	const max = 200
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > max {
			return line[:max] + "…"
		}
		return line
	}
	return ""
}

// withHealthOutput appends what the healthcheck said to a notification, when
// it said anything. Kept separate from the message construction so that a
// healthcheck which printed nothing -- a bare `exit 1`, which is common --
// leaves no dangling "The last healthcheck said:" behind it.
func withHealthOutput(text, output string) string {
	if output == "" {
		return text
	}
	return text + " The last healthcheck said: " + output
}
