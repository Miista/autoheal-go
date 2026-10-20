package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// composeBinary is where the compose binary is expected inside this container.
//
// It is mounted from the host rather than shipped in the image, on purpose.
// Compose computes the config hash that decides what a recreate produces, so
// running a different version than the host's is a real behaviour difference,
// and pinning one in the image would drift the moment the host was upgraded.
// Mounting the host's own binary makes the version question disappear instead
// of managing it.
//
// The Docker-distributed plugin is a statically linked Go binary, so it runs
// in a FROM scratch container with no libc, no shell and no docker CLI --
// verified against compose v5.1.4 and v5.5.1.
const composeBinary = "/docker-compose"

// recreateTimeout bounds a compose run. Recreating pulls nothing (the image is
// already present, by definition -- the container is running it) but it does
// stop, remove, create and start, and a container with a long stop timeout can
// legitimately take a while.
const recreateTimeout = 5 * time.Minute

// recreate rebuilds a container from its compose file: `docker compose up -d
// --force-recreate <service>`, with the project recovered from the container's
// own labels.
//
// Why shell out to compose rather than do this over the Docker API: there is
// no recreate endpoint, and there cannot be one. The Engine API treats a
// container as an immutable object -- create takes a full configuration, and
// every verb after that (start, stop, restart, rm) acts on what was already
// stored. Recreating means recomputing that configuration from a desired state
// the daemon has never seen: the compose file lives on the client's disk, and
// compose resolved `service:gluetun` into a concrete container ID before the
// daemon was told anything. Reimplementing that resolution here would mean
// owning compose's config-hash diffing and every HostConfig field Docker adds,
// forever, and getting the netns case right only by re-resolving the one field
// we happen to know about. Calling compose calls the component that owns the
// concept.
//
// Two mounts are required, and both are checked before running so a missing
// one is reported as itself rather than as a confusing compose error:
//
//	/usr/libexec/docker/cli-plugins/docker-compose:/docker-compose:ro
//	<project dir>:<same path>:ro
//
// The project directory must appear at the identical path it has on the host,
// because compose resolves .env files, relative bind mounts and `include:`
// against it. A translated path would not fail -- it would silently resolve to
// a different configuration and recreate the container wrong, which is worse.
// Read-only is sufficient; compose writes nothing to the project.
func recreate(log zerolog.Logger, name string, labels map[string]string) error {
	project, ok := composeProjectFrom(labels)
	if !ok {
		return fmt.Errorf("%s has no compose labels, so there is no compose file to rebuild it from — recreate only works for containers created by docker compose", name)
	}

	if _, err := os.Stat(composeBinary); err != nil {
		return fmt.Errorf("the compose binary is not mounted at %s, so %s cannot be recreated — mount the host's compose plugin there read-only", composeBinary, name)
	}

	// Checked explicitly because the failure is otherwise reported by compose
	// as a missing file, which reads like the project moved rather than like
	// this container cannot see it.
	if _, err := os.Stat(project.WorkingDir); err != nil {
		return fmt.Errorf("%s's project directory %s is not visible from inside autoheal, so it cannot be recreated — bind mount that path here at the same path, read-only", name, project.WorkingDir)
	}

	args := project.args()
	log.Debug().Msgf("Recreating %s: %s %s", name, composeBinary, strings.Join(args, " "))

	ctx, cancel := context.WithTimeout(context.Background(), recreateTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, composeBinary, args...)
	// Compose is being driven headless: without this it tries to render
	// progress as an ANSI-animated TTY into a pipe nothing is reading.
	cmd.Env = append(os.Environ(), "COMPOSE_PROGRESS=plain")

	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("compose took longer than %s to recreate %s and was killed", recreateTimeout, name)
	}
	if err != nil {
		// The whole output goes to the log, because the alert can only carry
		// one line and that line is not always the whole story.
		log.Error().Msgf("compose output while recreating %s:\n%s", name, strings.TrimSpace(string(output)))
		return fmt.Errorf("compose failed to recreate %s: %w: %s", name, err, lastLine(string(output)))
	}
	return nil
}

// lastLine trims command output down to something an alert can carry, the
// same way summarizeHealthOutput does for healthcheck output.
//
// The last line, not the first: compose prints its progress ("Container
// gluetun Running", "Container gluetun Waiting") before it gets anywhere near
// failing, and the error comes at the end. Taking the first line once turned
// "dependency failed to start" into an alert that just said gluetun was
// running.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if len(line) > 200 {
				return line[:200]
			}
			return line
		}
	}
	return "no output"
}
