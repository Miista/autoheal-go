package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestParseLabelBool(t *testing.T) {
	for _, tc := range []struct {
		in         string
		want       bool
		understood bool
	}{
		// compose leaves an unquoted YAML boolean capitalised; quoted stays
		// lowercase. Same intent, and upstream honouring only one spelling of
		// each is what upstream/110, /46 and /126 are all made of.
		{"true", true, true},
		{"True", true, true},
		{"TRUE", true, true},
		{"1", true, true},
		{"yes", true, true},
		{"false", false, true},
		{"False", false, true},
		{"FALSE", false, true},
		{"0", false, true},
		{"no", false, true},

		// Present but meaningless. Not a default to fall back on -- a mistake
		// to report. "ture" is the literal typo in upstream/119.
		{"ture", false, false},
		{"falsch", false, false},
		{"maybe", false, false},
		{"", false, false},
		{" true", false, false},
	} {
		got, ok := parseLabelBool(tc.in)
		if got != tc.want || ok != tc.understood {
			t.Errorf("parseLabelBool(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.understood)
		}
	}
}

// labelled builds an unhealthy container carrying arbitrary labels.
func labelled(id, name string, labels map[string]string) containerSummary {
	c := unhealthy(id, name)
	c.Labels = labels
	return c
}

// TestAllModeLabelHandling pins the four-way outcome in AUTOHEAL_CONTAINER_LABEL=all,
// the mode upstream/126 and upstream/73 are about. An absent label is not a
// misconfiguration -- it is the mode's default and still gets healed -- while a
// label that is present and unreadable stops us touching the container at all.
func TestAllModeLabelHandling(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		wantRestart bool
	}{
		{"absent label heals under all", nil, true},
		{"explicit true heals (redundant but harmless)", map[string]string{"autoheal": "true"}, true},
		{"compose-capitalised True heals", map[string]string{"autoheal": "True"}, true},
		{"explicit false is excluded", map[string]string{"autoheal": "false"}, false},
		{"compose-capitalised False is excluded", map[string]string{"autoheal": "False"}, false},
		{"unparseable value is skipped, not healed", map[string]string{"autoheal": "falsch"}, false},
		{"typo is skipped, not healed", map[string]string{"autoheal": "ture"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeDockerAPI()
			api.containers = []containerSummary{labelled("c1", "app", tc.labels)}
			cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10}

			runOnce(testLogger(), api, cfg, newStrikeTracker(3))

			if got := len(api.restarted) > 0; got != tc.wantRestart {
				t.Errorf("restarted = %v, want %v", got, tc.wantRestart)
			}
		})
	}
}

// TestLabelModeLabelHandling is the same table in the default mode, where
// AUTOHEAL_CONTAINER_LABEL names the label. The difference from all-mode is
// what an absent label means: here the container only reached us because the
// server-side filter matched the label's presence, so absence does not occur
// in practice and healing is opt-in rather than default.
func TestLabelModeLabelHandling(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		wantRestart bool
	}{
		{"explicit true heals", map[string]string{"autoheal": "true"}, true},
		{"compose-capitalised True heals", map[string]string{"autoheal": "True"}, true},
		{"quoted lowercase false is excluded", map[string]string{"autoheal": "false"}, false},
		{"compose-capitalised False is excluded", map[string]string{"autoheal": "False"}, false},
		{"unparseable value is skipped, not healed", map[string]string{"autoheal": "falsch"}, false},
		{"typo is skipped, not healed", map[string]string{"autoheal": "ture"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeDockerAPI()
			api.containers = []containerSummary{labelled("c1", "app", tc.labels)}
			cfg := config{ContainerLabel: "autoheal", DefaultStopTimeout: 10}

			runOnce(testLogger(), api, cfg, newStrikeTracker(3))

			if got := len(api.restarted) > 0; got != tc.wantRestart {
				t.Errorf("restarted = %v, want %v", got, tc.wantRestart)
			}
		})
	}
}

// TestUnparseableLabelIsReportedOnce covers the half that makes skipping
// survivable: silently dropping a container from coverage would be its own
// invisible failure, so the fault is logged -- but once, not every poll.
func TestUnparseableLabelIsReportedOnce(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{labelled("c1", "app", map[string]string{"autoheal": "ture"})}
	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10}
	strikes := newStrikeTracker(3)

	for i := 0; i < 5; i++ {
		runOnce(log, api, cfg, strikes)
	}

	if n := strings.Count(buf.String(), "not a value I understand"); n != 1 {
		t.Errorf("logged the bad label %d times across 5 polls, want exactly 1:\n%s", n, buf.String())
	}
	if len(api.restarted) != 0 {
		t.Errorf("restarted a misconfigured container: %v", api.restarted)
	}
}

// TestDeliberateExclusionIsSilent -- a correct autoheal=false is working
// configuration, not a fault, and must not narrate itself for as long as it
// holds.
func TestDeliberateExclusionIsSilent(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{labelled("c1", "app", map[string]string{"autoheal": "false"})}
	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10}
	strikes := newStrikeTracker(3)

	for i := 0; i < 3; i++ {
		runOnce(log, api, cfg, strikes)
	}

	if buf.Len() != 0 {
		t.Errorf("a deliberate exclusion logged something:\n%s", buf.String())
	}
}

// TestCustomLabelNameIsHonoured -- the exclusion check used to read a
// hardcoded "autoheal" key, so anyone running a custom
// AUTOHEAL_CONTAINER_LABEL could not exclude anything at all.
func TestCustomLabelNameIsHonoured(t *testing.T) {
	api := newFakeDockerAPI()
	api.containers = []containerSummary{labelled("c1", "app", map[string]string{"myapp": "false"})}
	cfg := config{ContainerLabel: "myapp", DefaultStopTimeout: 10}

	runOnce(testLogger(), api, cfg, newStrikeTracker(3))

	if len(api.restarted) != 0 {
		t.Errorf("custom label exclusion ignored, restarted: %v", api.restarted)
	}
}

// TestBadStopTimeoutIsSkippedAndReportedOnce -- a stop timeout that cannot be
// read is the upstream/119 complaint: the label looked ignored and the
// container was restarted on the 10s default when 240s was asked for. Guessing
// is the failure, so we skip, and say why once.
func TestBadStopTimeoutIsSkippedAndReportedOnce(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{labelled("c1", "app", map[string]string{
		"autoheal":              "true",
		"autoheal.stop.timeout": "4min",
	})}
	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10}
	strikes := newStrikeTracker(3)

	for i := 0; i < 5; i++ {
		runOnce(log, api, cfg, strikes)
	}

	if len(api.restarted) != 0 {
		t.Errorf("restarted despite an unreadable stop timeout: %v", api.restarted)
	}
	if n := strings.Count(buf.String(), "not a whole number of seconds"); n != 1 {
		t.Errorf("logged the bad timeout %d times across 5 polls, want exactly 1:\n%s", n, buf.String())
	}
}

// TestGoodStopTimeoutIsUsed guards the ordinary path the validation sits in
// front of.
func TestGoodStopTimeoutIsUsed(t *testing.T) {
	api := newFakeDockerAPI()
	api.containers = []containerSummary{labelled("c1", "app", map[string]string{
		"autoheal.stop.timeout": "240",
	})}
	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10}

	runOnce(testLogger(), api, cfg, newStrikeTracker(3))

	if len(api.restarted) != 1 {
		t.Fatalf("expected the container to be restarted, got %v", api.restarted)
	}
}

// A container can lose its name between the daemon building the list and us
// reading it -- it was removed in the gap. A misconfiguration is still worth
// reporting when that happens, against the id, rather than crashing on an
// empty Names slice or reporting against nothing at all.
func TestBadLabelOnNamelessContainerIsStillReported(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	api := newFakeDockerAPI()
	api.containers = []containerSummary{{
		ID:     "abcdef0123456789",
		Names:  nil,
		State:  "running",
		Labels: map[string]string{"autoheal": "ture"},
	}}
	cfg := config{ContainerLabel: "all", DefaultStopTimeout: 10}

	runOnce(log, api, cfg, newStrikeTracker(3))

	if len(api.restarted) != 0 {
		t.Errorf("restarted a misconfigured container: %v", api.restarted)
	}
	if !strings.Contains(buf.String(), "abcdef012345") {
		t.Errorf("the report did not identify the container by its short id:\n%s", buf.String())
	}
}
