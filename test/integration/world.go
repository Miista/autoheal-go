package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The parts of a scenario's world that are not the compose file: the image
// under test, the registry's certificate, and the images the project
// commits into, and the images the fixture refers to.

// label marks everything a scenario creates, so a run killed outright can
// still be swept by the next one.
const label = "io.github.miista.autoheal-go.integration"

func (s *Scenario) suite() string {
	suite, _, _ := strings.Cut(s.Name, "/")
	return suite
}

// copyCerts puts the package's certificate where this scenario's fixture
// expects it.
func (s *Scenario) copyCerts() {
	s.t.Helper()
	dir := filepath.Join(s.Dir, "certs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if out, err := exec.Command("cp", suiteCerts+"/cert.pem", suiteCerts+"/key.pem", dir).CombinedOutput(); err != nil {
		s.t.Fatalf("copying the certificate: %v: %s", err, out)
	}
}

func trimmed(b []byte) string { return strings.TrimSpace(string(b)) }

// pushDeclaredImages publishes every image the fixture's compose file refers
// to, so the stack can start.
//
// Read from the compose file rather than from a list beside it: the file
// already names them, and a second copy is a thing to keep in step. Only
// these -- what is AVAILABLE to update to is not referenced by any service,
// so it comes from the scenario's updates.
func (s *Scenario) pushDeclaredImages() {
	s.t.Helper()
	for _, ref := range s.declaredImages() {
		repo, tag, _ := strings.Cut(strings.TrimPrefix(ref, registryHost+"/"), ":")
		s.Push(repo, tag)
	}
}

// declaredImages is every image from this suite's registry that the fixture
// refers to, deduplicated.
//
// Asked of compose rather than read out of the yaml: compose resolves
// variables and includes, and knows what an image reference is. Matching them
// by hand would be guessing at a format someone else defines.
//
// Only the ones from the local registry: the subject's own image and the receiver
// are built once for the package and are not a scenario's to publish.
func (s *Scenario) declaredImages() []string {
	s.t.Helper()
	out, err := s.compose("config", "--images")
	if err != nil {
		s.t.Fatalf("listing %s's images: %v\n%s", s.Name, err, out)
	}

	var refs []string
	seen := map[string]bool{}
	for _, ref := range strings.Fields(out) {
		if !strings.HasPrefix(ref, registryHost+"/") || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs
}

// Push publishes a runnable image under a tag.
//
// The content is derived from the tag, so two tags never produce the same
// digest by accident: digests are compared, and identical content would make
// "the container was replaced" indistinguishable from "nothing happened".
//
// Pass content to override that, which is how a moving tag is made to move:
// the tag stays put while what it points at changes, exactly as it does when
// upstream rebuilds.
//
//	s.Push("app", "1.0.1")             // a new tag
//	s.Push("app", "latest", "second")  // the same tag, new digest
func (s *Scenario) Push(repo, tag string, content ...string) {
	s.t.Helper()
	marker := tag
	if len(content) > 0 {
		marker = content[0]
	}
	dir := s.scratch("build")

	main := fmt.Sprintf(`package main

import (
	"fmt"
)

func main() {
	for {
		fmt.Println("alive: %s")
		time.Sleep(5 * time.Second)
	}
}
`, marker)
	s.writeFile(filepath.Join(dir, "main.go"), main)
	s.writeFile(filepath.Join(dir, "go.mod"), "module sleeper\n\ngo 1.22\n")
	// FROM scratch with a static binary: nothing is pulled, so the suites
	// work offline, and the container genuinely runs so it can be replaced.
	s.writeFile(filepath.Join(dir, "Dockerfile"),
		"FROM scratch\nCOPY sleeper /sleeper\nCMD [\"/sleeper\"]\n")

	build := exec.Command("go", "build", "-ldflags", "-s -w", "-o", "sleeper", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		s.t.Fatalf("building the test image binary: %v\n%s", err, out)
	}

	ref := fmt.Sprintf("%s/%s:%s", registryHost, repo, tag)

	// Remove any image already under this tag before building.
	//
	// Building leaves the image on the HOST, so a tag from an earlier test --
	// or from a run that was killed before it could clean up -- would still
	// be here. A moving tag would then not move, and content from one test
	// would silently become another's. Removing it first means the build
	// cannot see a stale one, whoever left it.
	_ = exec.Command("docker", "rmi", "-f", ref).Run()

	s.docker("build", "-q", "-t", ref, dir)
	s.docker("push", "-q", ref)
	s.pushed = append(s.pushed, ref)
}

// PushUnrunnable publishes an image that exists but cannot start: FROM
// scratch with no command. For testing what the subject does when a container
// refuses the image it was just pinned to.
func (s *Scenario) PushUnrunnable(repo, tag string) {
	s.t.Helper()
	dir := s.scratch("broken")
	s.writeFile(filepath.Join(dir, "marker"), repo+":"+tag)
	s.writeFile(filepath.Join(dir, "Dockerfile"), "FROM scratch\nCOPY marker /marker\n")

	ref := fmt.Sprintf("%s/%s:%s", registryHost, repo, tag)

	// Remove any image already under this tag before building.
	//
	// Building leaves the image on the HOST, so a tag from an earlier test --
	// or from a run that was killed before it could clean up -- would still
	// be here. A moving tag would then not move, and content from one test
	// would silently become another's. Removing it first means the build
	// cannot see a stale one, whoever left it.
	_ = exec.Command("docker", "rmi", "-f", ref).Run()

	s.docker("build", "-q", "-t", ref, dir)
	s.docker("push", "-q", ref)
	s.pushed = append(s.pushed, ref)
}

// scratch is a directory for the harness's own working files -- image build
// contexts, mostly.
//
// Outside the project, because everything inside it is copied per scenario
// refuses to act on when it is dirty: a build context left inside would look
// like a half-applied change.
func (s *Scenario) scratch(name string) string {
	dir := filepath.Join(s.root, "testbed-scratch", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { os.RemoveAll(filepath.Join(s.root, "testbed-scratch")) })
	return dir
}

func (s *Scenario) writeFile(path, content string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}
