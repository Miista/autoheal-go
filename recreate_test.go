package main

import (
	"strings"
	"testing"
)

func TestParseHealMode(t *testing.T) {
	for _, tc := range []struct {
		in         string
		want       healMode
		understood bool
	}{
		{"restart", healRestart, true},
		{"recreate", healRecreate, true},
		// Case and surrounding space are the same intent written carelessly,
		// not a different instruction.
		{"Recreate", healRecreate, true},
		{"RECREATE", healRecreate, true},
		{" recreate ", healRecreate, true},
		// The refusing cases. Each of these is somebody trying to say
		// something; defaulting to restart would silently do the one thing the
		// label was added to prevent.
		{"recreat", healRestart, false},
		{"rebuild", healRestart, false},
		{"true", healRestart, false},
		{"", healRestart, false},
	} {
		got, understood := parseHealMode(tc.in)
		if understood != tc.understood {
			t.Errorf("parseHealMode(%q) understood = %v, want %v", tc.in, understood, tc.understood)
		}
		if understood && got != tc.want {
			t.Errorf("parseHealMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestComposeProjectFrom(t *testing.T) {
	// The real labels off the qbittorrent container this feature was written
	// for, copied verbatim from `docker inspect`.
	full := map[string]string{
		"com.docker.compose.project":              "optiplex",
		"com.docker.compose.service":              "qbittorrent",
		"com.docker.compose.project.working_dir":  "/home/guldmund/docker/optiplex",
		"com.docker.compose.project.config_files": "/home/guldmund/docker/optiplex/docker-compose.yml",
	}

	p, ok := composeProjectFrom(full)
	if !ok {
		t.Fatal("real compose labels were not recognised")
	}
	if p.Name != "optiplex" || p.Service != "qbittorrent" {
		t.Errorf("got project %q service %q", p.Name, p.Service)
	}
	if len(p.ConfigFiles) != 1 || p.ConfigFiles[0] != "/home/guldmund/docker/optiplex/docker-compose.yml" {
		t.Errorf("got config files %v", p.ConfigFiles)
	}

	// A project with several compose files lists them comma-separated.
	multi := map[string]string{
		"com.docker.compose.project":              "p",
		"com.docker.compose.service":              "s",
		"com.docker.compose.project.working_dir":  "/w",
		"com.docker.compose.project.config_files": "/w/a.yml,/w/b.yml",
	}
	p, ok = composeProjectFrom(multi)
	if !ok || len(p.ConfigFiles) != 2 {
		t.Fatalf("multi-file project: ok=%v files=%v", ok, p.ConfigFiles)
	}

	// The negative direction: a container created by `docker run` carries no
	// compose labels, and there is no file to rebuild it from. Each label is
	// dropped in turn because any one of them missing is disqualifying.
	for _, missing := range []string{
		"com.docker.compose.project",
		"com.docker.compose.service",
		"com.docker.compose.project.working_dir",
		"com.docker.compose.project.config_files",
	} {
		partial := map[string]string{}
		for k, v := range full {
			if k != missing {
				partial[k] = v
			}
		}
		if _, ok := composeProjectFrom(partial); ok {
			t.Errorf("accepted a project with %s missing", missing)
		}
	}

	if _, ok := composeProjectFrom(map[string]string{}); ok {
		t.Error("accepted a container with no compose labels at all")
	}
}

func TestComposeProjectArgs(t *testing.T) {
	p := composeProject{
		Name:        "optiplex",
		WorkingDir:  "/home/guldmund/docker/optiplex",
		ConfigFiles: []string{"/home/guldmund/docker/optiplex/docker-compose.yml"},
		Service:     "qbittorrent",
	}
	got := strings.Join(p.args(), " ")
	want := "--project-name optiplex " +
		"--project-directory /home/guldmund/docker/optiplex " +
		"-f /home/guldmund/docker/optiplex/docker-compose.yml " +
		"up -d --force-recreate qbittorrent"
	if got != want {
		t.Errorf("args:\n got: %s\nwant: %s", got, want)
	}

	// Several files become repeated -f flags rather than one joined argument.
	p.ConfigFiles = []string{"/w/a.yml", "/w/b.yml"}
	if got := strings.Join(p.args(), " "); !strings.Contains(got, "-f /w/a.yml -f /w/b.yml") {
		t.Errorf("multi-file args did not repeat -f: %s", got)
	}
}

// Recreate refuses before running anything when its preconditions are not met,
// and each refusal names the thing to fix. These are the cases an operator
// actually hits on a first deployment, and a compose error would describe them
// misleadingly -- "no configuration file provided" reads like the project
// moved, not like the mount is missing.
func TestRecreateRefusesWithoutPreconditions(t *testing.T) {
	log := newLogger("error")

	err := recreate(log, "plain-container", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "no compose labels") {
		t.Errorf("non-compose container: got %v", err)
	}

	// The compose binary is not mounted in the test process, so a container
	// with perfectly good labels still cannot be recreated -- and must say so
	// about the binary rather than about the labels.
	err = recreate(log, "qbittorrent", map[string]string{
		"com.docker.compose.project":              "optiplex",
		"com.docker.compose.service":              "qbittorrent",
		"com.docker.compose.project.working_dir":  "/home/guldmund/docker/optiplex",
		"com.docker.compose.project.config_files": "/home/guldmund/docker/optiplex/docker-compose.yml",
	})
	if err == nil || !strings.Contains(err.Error(), composeBinary) {
		t.Errorf("missing compose binary: got %v", err)
	}
}

func TestLastLine(t *testing.T) {
	// The shape of the real failure: progress first, the diagnosis last.
	out := " Container gluetun Running \n Container gluetun Waiting \n" +
		"dependency failed to start: container gluetun is unhealthy\n\n"
	if got := lastLine(out); got != "dependency failed to start: container gluetun is unhealthy" {
		t.Errorf("got %q", got)
	}
	if got := lastLine(""); got != "no output" {
		t.Errorf("empty: got %q", got)
	}
	if got := lastLine(strings.Repeat("x", 500)); len(got) != 200 {
		t.Errorf("long line not trimmed: %d chars", len(got))
	}
}

// The mode label routes the heal, and an unreadable one stops it. Checked
// through runOnce rather than parseHealMode alone, because the thing that
// matters is that a container asking for recreate is never quietly restarted
// instead -- restart is precisely what does not work for it.
func TestRunOnceHonoursHealMode(t *testing.T) {
	composeLabels := func(extra map[string]string) map[string]string {
		l := map[string]string{
			"autoheal":                                "true",
			"com.docker.compose.project":              "optiplex",
			"com.docker.compose.service":              "qbittorrent",
			"com.docker.compose.project.working_dir":  "/nonexistent",
			"com.docker.compose.project.config_files": "/nonexistent/docker-compose.yml",
		}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}

	t.Run("no mode label restarts, as before", func(t *testing.T) {
		api := newFakeDockerAPI()
		api.containers = []containerSummary{{
			ID: "aaa", Names: []string{"/qbittorrent"}, State: "running",
			Labels: composeLabels(nil),
		}}
		runOnce(newLogger("error"), api, config{ContainerLabel: "autoheal"}, newStrikeTracker(3))
		if len(api.restarted) != 1 {
			t.Errorf("expected a restart, got %v", api.restarted)
		}
	})

	t.Run("recreate mode does not restart", func(t *testing.T) {
		api := newFakeDockerAPI()
		api.containers = []containerSummary{{
			ID: "aaa", Names: []string{"/qbittorrent"}, State: "running",
			Labels: composeLabels(map[string]string{"autoheal.mode": "recreate"}),
		}}
		// recreate() fails here (no compose binary in the test process), which
		// is the point: the restart path must not be reached as a fallback.
		runOnce(newLogger("error"), api, config{ContainerLabel: "autoheal"}, newStrikeTracker(3))
		if len(api.restarted) != 0 {
			t.Errorf("recreate mode fell back to restart: %v", api.restarted)
		}
	})

	t.Run("unreadable mode heals nothing", func(t *testing.T) {
		api := newFakeDockerAPI()
		api.containers = []containerSummary{{
			ID: "aaa", Names: []string{"/qbittorrent"}, State: "running",
			Labels: composeLabels(map[string]string{"autoheal.mode": "recreat"}),
		}}
		runOnce(newLogger("error"), api, config{ContainerLabel: "autoheal"}, newStrikeTracker(3))
		if len(api.restarted) != 0 {
			t.Errorf("a typo'd mode was healed anyway: %v", api.restarted)
		}
	})
}

// A recreate gives the service a new container ID, and the crash-loop history
// has to follow it. Without the transfer the replacement starts at zero every
// cycle, so a service that recreates and fails forever would never be
// recognised as a crash loop -- the one thing this port added over upstream.
func TestStrikeTransferAcrossRecreate(t *testing.T) {
	s := newStrikeTracker(3)

	s.recordRestart("old")
	s.recordRestart("old")
	s.transfer("old", "new")

	if s.tracked()["old"] {
		t.Error("old container ID still tracked after transfer")
	}
	if !s.tracked()["new"] {
		t.Fatal("new container ID not tracked after transfer")
	}
	// Two strikes carried across, so the third crosses the threshold.
	crossed, count := s.recordRestart("new")
	if !crossed || count != 3 {
		t.Errorf("crossed=%v count=%d, want true/3 — history did not carry", crossed, count)
	}
}

func TestStrikeTransferCarriesGivenUp(t *testing.T) {
	s := newStrikeTracker(1)
	s.recordRestart("old")
	if !s.isGivenUp("old") {
		t.Fatal("precondition: expected old to be given up")
	}
	s.transfer("old", "new")
	if !s.isGivenUp("new") {
		t.Error("given-up state did not survive the recreate — autoheal would resume restarting a container it had written off")
	}
	if s.isGivenUp("old") {
		t.Error("old ID still marked given up")
	}
}

// Transferring onto itself is a no-op rather than a state-losing shuffle. This
// is the path taken when a recreate somehow returns the same ID.
func TestStrikeTransferSameID(t *testing.T) {
	s := newStrikeTracker(3)
	s.recordRestart("same")
	s.transfer("same", "same")
	if !s.tracked()["same"] {
		t.Error("self-transfer dropped the container's state")
	}
}
