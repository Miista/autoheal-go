package integration

// What the package needs before any scenario can run: the image under test,
// the receiver that stands in for ntfy, and the certificate the registry
// serves.
//
// Here rather than in main_test.go because the sandbox uses them too --
// standing a scenario up by hand and testing one are the same work.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Subject is the compose service name of the thing under test, and the prefix
// a fixture uses for it. A scenario running more than one copy names them
// autoheal_a, autoheal_b and so on, so this is matched as a prefix rather than
// exactly.
//
// The harness holds the stack up around it: the suite brings the world up,
// each scenario copies in a fixture, and teardown removes both. What the
// subject does with that world is the test's business, not the harness's.
const Subject = "autoheal"

// suiteImage is the image every scenario in this package refers to, built once
// per suite from the repository it lives in.
const suiteImage = "autoheal:integration"

// receiverImage stands in for a webhook endpoint, for the scenarios that
// assert on what was announced.
const receiverImage = "autoheal-receiver:integration"

// patientImage is the container under care: it reports whatever health its
// HEALTH env var scripts. Every scenario uses it, so it is built once here
// rather than per fixture.
const patientImage = "autoheal-patient:integration"

// suiteCerts holds the certificate the registry serves and the subject trusts,
// generated once and copied into each scenario.
var suiteCerts string

// buildReceiver builds the notification receiver from its own source, rather
// than a heredoc: it is ordinary Go that compiles and vets with everything
// else.
func buildReceiver(root string) error {
	return buildGoImage(root, "./test/integration/receiver", "receiver", receiverImage)
}

// patientHealthcheck is baked into the image rather than written into every
// fixture: a fixture that forgot to declare one would be a container autoheal
// never looks at, and one that picked its own interval would make a scenario
// mean something different from the next.
//
// Docker still has to probe -- that is the only way a container's status
// becomes "healthy" or "unhealthy" for autoheal to act on. The probe just does
// not decide anything: it reports whatever the patient's own loop last wrote,
// so there are two clocks and only one of them is authoritative.
//
// The interval is well under any sensible STEP, so a state the script asks for
// is observed rather than skipped past. retries=1 because the patient is
// definite: it has already decided, and re-asking would only delay the answer.
const patientHealthcheck = `HEALTHCHECK --interval=1s --timeout=2s --retries=1 ` +
	`CMD ["/patient", "-check"]`

// buildGoImage builds one of the suite's own helper programs into a scratch
// image.
//
// From its own source rather than a heredoc: these are ordinary Go that
// compiles and vets with everything else, so a mistake in them is a build
// failure and not a container that misbehaves at three in the morning.
func buildGoImage(root, pkg, binary, tag string, extra ...string) error {
	dir, err := os.MkdirTemp("", "autoheal-"+binary)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	build := exec.Command("go", "build", "-ldflags", "-s -w", "-o",
		filepath.Join(dir, binary), pkg)
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}

	// FROM scratch: nothing is pulled, so the suites work offline.
	dockerfile := fmt.Sprintf("FROM scratch\nCOPY %s /%s\nENTRYPOINT [\"/%s\"]\n",
		binary, binary, binary)
	for _, line := range extra {
		dockerfile += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}

	cmd := exec.Command("docker", "build", "-q",
		"--label", label+"=integration", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// buildImage builds the subject from the shipped Dockerfile.
//
// The real one, deliberately: a suite that builds its own image tests an
// artifact nobody ships, and the two drift. In the repository this harness
// came from that was not hypothetical -- the test image was FROM scratch, the
// shipped one was not, and a new runtime dependency was invisible until the
// shipped image was the one under test.
func buildImage(root string) error {
	args := []string{"build", "-q", "--label", label + "=integration",
		"-f", "Dockerfile", "-t", suiteImage}
	if os.Getenv("GOCOVERDIR") != "" {
		args = append(args, "--build-arg", "COVER=1")
	}
	args = append(args, root)

	cmd := exec.Command("docker", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func findRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w", err)
	}
	return trimmed(out), nil
}

// Setup prepares what every scenario needs and returns the repository root.
//
// Building the image under test, building the receiver that stands in for
// ntfy, and starting the registry: all identical whatever a scenario
// declares, and none of it what any test is about. Rebuilding a
// byte-identical image per test is pure cost.
//
// Shared with the sandbox, which needs exactly the same world.
func Setup() (string, error) {
	root, err := findRoot()
	if err != nil {
		return "", err
	}
	if err := buildImage(root); err != nil {
		return "", fmt.Errorf("building %s: %w", suiteImage, err)
	}
	if err := buildReceiver(root); err != nil {
		return "", fmt.Errorf("building %s: %w", receiverImage, err)
	}
	if err := buildGoImage(root, "./test/integration/patient", "patient", patientImage, patientHealthcheck); err != nil {
		return "", fmt.Errorf("building %s: %w", patientImage, err)
	}
	// Generates the certificate beside the registry's compose file, then
	// brings the registry up and waits for it to accept pushes.
	if err := startRegistry(root); err != nil {
		return "", err
	}
	return root, nil
}

// Teardown removes what Setup started.
//
// The images are left: they are the artifacts under test, rebuilt next run,
// and removing them would throw away the layer cache that makes a rerun
// quick.
func Teardown(root string) {
	stopRegistry(root)
}

// Down empties the testbed, removing whatever a sandbox left running.
func Down(t T, root string) {
	s := &Scenario{t: t, root: root, Dir: filepath.Join(root, "testbed")}
	s.sweep()
}

// Scenarios lists every fixture, as <suite>/<scenario>.
func Scenarios() []string {
	root, err := findRoot()
	if err != nil {
		return nil
	}
	fixtures := filepath.Join(root, "hack", "fixtures")
	suites, err := os.ReadDir(fixtures)
	if err != nil {
		return nil
	}

	var names []string
	for _, suite := range suites {
		if !suite.IsDir() || suite.Name() == "common" || suite.Name() == "infrastructure" {
			continue
		}
		scenarios, err := os.ReadDir(filepath.Join(fixtures, suite.Name()))
		if err != nil {
			continue
		}
		for _, scenario := range scenarios {
			if scenario.IsDir() {
				names = append(names, suite.Name()+"/"+scenario.Name())
			}
		}
	}
	sort.Strings(names)
	return names
}
