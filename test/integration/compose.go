package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Recreating a container is the one thing autoheal cannot do over the Docker
// API, so it runs the compose binary -- and the scenarios that exercise that
// have to put one inside the subject's container. This is how they find one.
//
// See recreate.go for why it is mounted from the host rather than shipped in
// the image.

// composePluginPaths are where Docker installs the compose plugin. Searched
// in order; the first that exists wins.
var composePluginPaths = []string{
	"/usr/libexec/docker/cli-plugins/docker-compose",
	"/usr/local/lib/docker/cli-plugins/docker-compose",
	"/usr/lib/docker/cli-plugins/docker-compose",
}

// hostComposeBinary is a Linux compose binary that can be mounted into a
// container, or "" with the reason why there is none.
//
// On Linux the host's own plugin is it, which is also exactly what the
// deployment does. On macOS it is not: Docker Desktop's CLI is a Mach-O
// binary that cannot execute inside a Linux container at all, so the
// scenarios that need one have to source it elsewhere -- see
// composeFromImage.
func hostComposeBinary() (string, string) {
	if runtime.GOOS != "linux" {
		return "", "the host's compose plugin is a " + runtime.GOOS + " binary, which cannot run in a Linux container"
	}
	home, _ := os.UserHomeDir()
	paths := append([]string{}, composePluginPaths...)
	if home != "" {
		paths = append(paths, filepath.Join(home, ".docker", "cli-plugins", "docker-compose"))
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			// A symlink to a non-Linux binary is what Docker Desktop leaves on
			// a Mac; resolved here so the check below sees the real file.
			if resolved, err := filepath.EvalSymlinks(p); err == nil {
				p = resolved
			}
			return p, ""
		}
	}
	return "", "no docker compose plugin found in " + strings.Join(paths, ", ")
}

// composeBinaryForTests returns a path to a Linux compose binary to mount into
// the subject, extracting one from the official image when the host cannot
// supply it.
//
// Extracted rather than skipped on macOS, because the recreate path is
// precisely what a developer on a Mac needs to be able to test before it goes
// near the host it was written for.
func composeBinaryForTests(t T, root string) string {
	t.Helper()

	if path, _ := hostComposeBinary(); path != "" {
		return path
	}

	// Cached beside the testbed: the image is ~30MB and every scenario that
	// recreates would otherwise pull and extract it again.
	cached := filepath.Join(root, "testbed", ".compose", "docker-compose")
	if _, err := os.Stat(cached); err == nil {
		return cached
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatalf("preparing the compose cache: %v", err)
	}

	// docker/compose-bin carries the binary and nothing else -- no entrypoint,
	// hence the unused command, which is never run.
	name := "autoheal-compose-src"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "create", "--name", name,
		"docker/compose-bin:latest", "/unused").CombinedOutput(); err != nil {
		t.Fatalf("fetching a Linux compose binary: %v\n%s", err, out)
	}
	defer exec.Command("docker", "rm", name).Run()

	if out, err := exec.Command("docker", "cp",
		name+":/docker-compose", cached).CombinedOutput(); err != nil {
		t.Fatalf("extracting the compose binary: %v\n%s", err, out)
	}
	if err := os.Chmod(cached, 0o755); err != nil {
		t.Fatalf("making the compose binary executable: %v", err)
	}
	return cached
}

// enableRecreateIfDeclared writes the .env a recreate fixture refers to, and
// does nothing for the fixtures that do not.
//
// Driven by what the compose file actually says rather than by a test
// remembering to ask: the mounts are declared in the fixture, so the fixture
// is what knows whether they are needed. A test that forgot the call would
// otherwise get a stack that comes up with an empty mount path and fails
// somewhere less obvious.
func (s *Scenario) enableRecreateIfDeclared() {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker-compose.yml"))
	if err != nil || !strings.Contains(string(raw), "${COMPOSE_BIN}") {
		return
	}
	s.EnableRecreate()
}

// EnableRecreate gives a scenario what the subject needs to run compose: a
// Linux compose binary, and the project directory visible inside the
// container at the path it has outside.
//
// Written as a .env file rather than passed through the harness, because
// compose reads one from the project directory on its own and the fixture can
// then declare the mounts in the ordinary way. The values cannot be baked into
// the fixture: the binary is wherever this host keeps it, and the project path
// is the testbed, which is only known once the scenario exists.
//
// Called before Start(), since the subject reads its mounts when it comes up.
func (s *Scenario) EnableRecreate() {
	s.t.Helper()
	binary := composeBinaryForTests(s.t, s.root)

	// Appended, not written: a fixture ships its own .env for the constants
	// its tests read back through Setting(), and overwriting it would take
	// those away -- silently, since compose treats an unset variable as empty
	// rather than as an error.
	path := filepath.Join(s.Dir, ".env")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		s.t.Fatalf("reading %s's .env: %v", s.Name, err)
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	s.writeFile(path, content+"COMPOSE_BIN="+binary+"\nPROJECT_DIR="+s.Dir+"\n")
}
