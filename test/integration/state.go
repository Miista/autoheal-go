package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A scenario's state: what a test reads and changes between bringing the
// world up and asserting on it -- pinning a service, what the compose file
// says, and which container is behind a service.

// dockerPinOnce builds docker pin once per package, for the same reason the
// image is built once: it is the same binary for every test.
var (
	dockerPinOnce sync.Once
	dockerPinPath string
)

// dockerPin is the path to a built docker pin, built once per package for the
// same reason the images are: it is the same binary for every test.
func (s *Scenario) dockerPin() string {
	s.t.Helper()
	dockerPinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "docker-pin")
		if err != nil {
			s.t.Fatal(err)
		}
		dockerPinPath = filepath.Join(dir, "docker-pin")
		args := []string{"build", "-o", dockerPinPath}
		if os.Getenv("GOCOVERDIR") != "" {
			args = append(args, "-cover", "-coverpkg=./...")
		}
		build := exec.Command("go", append(args, "./cmd/docker-pin")...)
		build.Dir = s.root
		if out, err := build.CombinedOutput(); err != nil {
			s.t.Fatalf("building docker pin: %v\n%s", err, out)
		}
	})
	return dockerPinPath
}

// imageLine matches a service's image, whatever indentation the fixture uses.
var imageLineRe = regexp.MustCompile(`(?m)^\s+image:\s*(\S+)`)

// Image is what the compose file currently pins for a service: the image line
// as written, digest and all.
func (s *Scenario) Image(service string) string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker-compose.yml"))
	if err != nil {
		s.t.Fatalf("reading the compose file: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != service+":" {
			continue
		}
		for _, rest := range lines[i+1:] {
			if m := imageLineRe.FindStringSubmatch("\n" + rest); m != nil {
				return m[1]
			}
			// A blank line or a dedent means the service ended without one.
			if trimmed := strings.TrimSpace(rest); trimmed == "" {
				break
			}
		}
	}
	s.t.Fatalf("no image line for service %q in %s", service, s.Dir)
	return ""
}

// Container is the id of the container running a service, or "" if none is.
//
// Asked of compose rather than derived: compose names a project after the
// directory holding the compose file, by rules that are its business, and
// reimplementing them here would be guessing at someone else's algorithm.
func (s *Scenario) Container(service string) string {
	s.t.Helper()
	out, err := s.compose("ps", "-q", service)
	if err != nil || out == "" {
		return ""
	}
	// One id per line; a service has one container in these scenarios.
	return strings.Fields(out)[0]
}

// StartedAt is when a service's container last started.
//
// The question a restart test actually asks. A restart keeps the container's
// id -- docker restarts it rather than replacing it -- so identity says
// nothing, and a log line says only what somebody chose to print. The start
// time is the daemon's own record of the thing that happened.
//
// Zero time when there is no container, so a caller comparing against a later
// reading sees "not started" rather than a false restart.
func (s *Scenario) StartedAt(service string) time.Time {
	s.t.Helper()
	id := s.Container(service)
	if id == "" {
		return time.Time{}
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.StartedAt}}", id).Output()
	if err != nil {
		s.t.Fatalf("reading %s's start time: %v", service, err)
	}
	started, err := time.Parse(time.RFC3339Nano, trimmed(out))
	if err != nil {
		s.t.Fatalf("parsing %s's start time %q: %v", service, trimmed(out), err)
	}
	return started
}

// RunningImage is the image a container was created from.
func (s *Scenario) RunningImage(container string) string {
	s.t.Helper()
	if container == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect", container,
		"--format", "{{.Config.Image}}").Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// BindSource is the host path a container has mounted at dest.
//
// Read from the daemon rather than by exec'ing into the container: the test
// images are FROM scratch with a static binary and have no shell.
func (s *Scenario) BindSource(container, dest string) string {
	s.t.Helper()
	if container == "" {
		return ""
	}
	format := `{{range .Mounts}}{{if eq .Destination "` + dest + `"}}{{.Source}}{{end}}{{end}}`
	out, err := exec.Command("docker", "inspect", container, "--format", format).Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// queuePort is where a fixture publishes an HTTP endpoint a test reads.
const queuePort = "8098"

// Run invokes docker pin in the scenario's project and returns what it said,
// with whether it succeeded.
//
// The exit status matters as much as the output: a mistyped safety flag that
// exits zero has run for real, which is the bug rejectUnknownFlags exists to
// prevent.
func (s *Scenario) Run(args ...string) (string, bool) {
	s.t.Helper()
	cmd := exec.Command(s.dockerPin(), args...)
	cmd.Dir = s.Dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

// Digest is the digest the registry currently serves for a tag.
func (s *Scenario) Digest(repo, tag string) string {
	s.t.Helper()
	ref := registryHost + "/" + repo + ":" + tag
	out, err := exec.Command("docker", "image", "inspect", ref,
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}").Output()
	if err != nil {
		s.t.Fatalf("no local image for %s: %v", ref, err)
	}
	// An image derived from another can carry several repo digests; only the
	// one for this repository names what was pushed here.
	for _, line := range strings.Fields(string(out)) {
		if strings.HasPrefix(line, registryHost+"/"+repo+"@") {
			return line[strings.Index(line, "@")+1:]
		}
	}
	s.t.Fatalf("no digest for %s in %s", ref, out)
	return ""
}

// WriteCompose replaces the scenario's compose file.
//
// For scenarios that assert on the compose file itself: what matters
// there is the image line before and after, and a fixture per permutation
// would be four files differing by one word.
func (s *Scenario) WriteCompose(content string) {
	s.t.Helper()
	s.writeFile(filepath.Join(s.Dir, "docker-compose.yml"), content)
}

// Setting reads a value from the scenario's .env.
//
// The same file compose reads when it brings the stack up, so a test asks for
// the number the subject was actually given rather than repeating it. A test
// that hardcoded the threshold would keep passing after the fixture changed,
// while asserting on a number nothing sets.
func (s *Scenario) Setting(key string) string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, ".env"))
	if err != nil {
		s.t.Fatalf("reading %s's .env: %v", s.Name, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	s.t.Fatalf("%s's .env does not set %s", s.Name, key)
	return ""
}

// SettingInt is Setting for a value that is a number.
func (s *Scenario) SettingInt(key string) int {
	s.t.Helper()
	n, err := strconv.Atoi(s.Setting(key))
	if err != nil {
		s.t.Fatalf("%s=%q is not a number: %v", key, s.Setting(key), err)
	}
	return n
}

// Health is what docker currently says about a service's container:
// "healthy", "unhealthy", "starting", or "" when it has no healthcheck.
func (s *Scenario) Health(service string) string {
	s.t.Helper()
	id := s.Container(service)
	if id == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect",
		"-f", "{{if .State.Health}}{{.State.Health.Status}}{{end}}", id).Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// State is docker's own word for what a container is doing: "running",
// "paused", "restarting", "exited".
//
// Separate from Health, which is what the healthcheck says. The two are
// independent: a paused container keeps whatever health status it had when it
// was paused, and can be both "paused" and "unhealthy" at once.
func (s *Scenario) State(service string) string {
	s.t.Helper()
	id := s.Container(service)
	if id == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", id).Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// Pause suspends a service's container and returns once docker says it is
// paused.
//
// What `docker compose pause` does during a backup: the processes are frozen,
// not stopped. Waited on for the same reason MakeSick is -- a test that
// carried on before docker had applied it would be asserting about a container
// that was still running.
func (s *Scenario) Pause(service string) {
	s.t.Helper()
	if out, err := s.compose("pause", service); err != nil {
		s.t.Fatalf("pausing %s: %v\n%s", service, err, out)
	}
	s.waitFor(service+" to be paused", 30*time.Second,
		func() bool { return s.State(service) == "paused" },
		func() string { return service + " is " + s.State(service) })
}

// Unpause is the mirror of Pause.
func (s *Scenario) Unpause(service string) {
	s.t.Helper()
	if out, err := s.compose("unpause", service); err != nil {
		s.t.Fatalf("unpausing %s: %v\n%s", service, err, out)
	}
	s.waitFor(service+" to be running again", 30*time.Second,
		func() bool { return s.State(service) == "running" },
		func() string { return service + " is " + s.State(service) })
}

// MakeSick tells a patient to report unhealthy, and returns once docker says
// it is.
//
// The wait is the point, not a convenience. The gate only changes what the
// patient will answer; docker still has to probe, notice, and change the
// container's status, and autoheal acts on that status alone. A test that
// returned as soon as the gate was set would be racing both -- and when it
// then found the container had not been restarted, it could not tell
// "autoheal ignored it" from "autoheal was never given the chance".
func (s *Scenario) MakeSick(service string) {
	s.t.Helper()
	s.gate(service, "sick")
	s.waitFor(service+" to be reported unhealthy", 60*time.Second,
		func() bool { return s.Health(service) == "unhealthy" },
		func() string { return service + " is " + s.Health(service) })
}

// MakeWell is the mirror of MakeSick, and waits for the same reason.
func (s *Scenario) MakeWell(service string) {
	s.t.Helper()
	s.gate(service, "well")
	s.waitFor(service+" to be reported healthy", 60*time.Second,
		func() bool { return s.Health(service) == "healthy" },
		func() string { return service + " is " + s.Health(service) })
}

// gate posts to a patient's control endpoint.
//
// Through the container rather than a published port: a fixture would
// otherwise have to publish one per patient and keep them from colliding,
// and the scenarios that matter run several patients at once.
func (s *Scenario) gate(service, action string) {
	s.t.Helper()
	id := s.Container(service)
	if id == "" {
		s.t.Fatalf("no container for %s", service)
	}
	out, err := exec.Command("docker", "exec", id,
		"/patient", "-post", "/"+action).CombinedOutput()
	if err != nil {
		s.t.Fatalf("setting %s %s: %v\n%s", service, action, err, out)
	}
}
