// patient is a container whose health a test decides.
//
// Every autoheal test needs a container that is unhealthy on purpose, and no
// real image can be talked into that on cue. A healthcheck of `false` is
// unhealthy from its first probe, so `compose up --wait` never returns and the
// world never comes up.
//
// The patient is healthy until a test says otherwise. It watches a gate file
// and reports unhealthy exactly while that file exists:
//
//	GATE=/gate/sick     # bind-mounted from the scenario directory
//
// A test brings the stack up -- everything healthy, no races -- then trips the
// gate when it is ready, and knows rather than hopes that the container was
// healthy first and sick after. That ordering is what a timing-based script
// cannot give you: with a schedule, a test that misses its window cannot tell
// "autoheal did nothing" from "autoheal was never asked".
//
// Removing the gate makes it healthy again, which is how a scenario about
// recovery -- or about autoheal giving up after too many restarts -- says what
// it means.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-check" {
		os.Exit(check())
	}

	// Nothing to do but stay up: the healthcheck runs as its own process and
	// reads the gate itself, so this one holds the container open and no more.
	//
	// Sleeping rather than blocking forever, because the runtime treats a
	// permanent block with nothing else running as a deadlock and panics --
	// which would kill the container the moment it started.
	fmt.Printf("patient: up, watching %s\n", gate())
	for {
		time.Sleep(time.Hour)
	}
}

// check is what the image's HEALTHCHECK runs: unhealthy while the gate file
// exists, healthy otherwise.
//
// A missing gate is healthy rather than an error. A scenario that never trips
// one is a container that is simply well, and that is the common case -- it is
// what every other service in a fixture is there to be.
func check() int {
	if _, err := os.Stat(gate()); err == nil {
		return 1
	}
	return 0
}

func gate() string {
	if v := os.Getenv("GATE"); v != "" {
		return v
	}
	return "/gate/sick"
}
