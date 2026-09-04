// patient is a container whose health a test decides.
//
// Every autoheal test needs a container that is unhealthy on purpose, and no
// real image can be talked into that on cue. A healthcheck of `false` is
// unhealthy from its first probe, so `compose up --wait` never returns and the
// world never comes up.
//
// The patient starts healthy and stays that way until a test says otherwise:
//
//	POST /sick    -> unhealthy from the next probe
//	POST /well    -> healthy again
//	GET  /        -> "ok" or "sick", what the next probe will report
//
// A test brings the stack up -- everything healthy, no races -- then trips the
// gate when it is ready, and knows rather than hopes that the container was
// healthy first and sick after. That ordering is what a timing-based script
// cannot give you: with a schedule, a test that misses its window cannot tell
// "autoheal did nothing" from "autoheal was never asked".
//
// The gate outlives a restart, because it is kept in the container's own
// filesystem rather than in memory. `docker restart` stops and starts the same
// container -- it does not recreate it -- so the writable layer is untouched
// and the patient comes back exactly as sick as it went down. That is what a
// crash loop is: the cause survives the restart. A scenario that wants
// recovery clears the gate itself, which is the honest shape -- the test is
// what fixed it.
//
// No bind mount and no named volume, so a fixture declares nothing for this
// and there is nothing left on the host to clean up.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// gatePath is inside the container, deliberately. See the package comment.
const gatePath = "/patient.sick"

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "-check":
		os.Exit(check())
	// The image is FROM scratch, so there is no curl and no shell to reach the
	// endpoint with. `docker exec <id> /patient -post /sick` is how a test
	// talks to a patient without a fixture publishing a port per container.
	case len(os.Args) > 2 && os.Args[1] == "-post":
		os.Exit(post(os.Args[2]))
	}

	http.HandleFunc("/sick", set(true))
	http.HandleFunc("/well", set(false))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, verdict())
	})

	addr := ":" + port()
	fmt.Printf("patient: up on %s, %s\n", addr, verdict())
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "patient: %v\n", err)
		os.Exit(1)
	}
}

func set(sick bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		if sick {
			err = os.WriteFile(gatePath, nil, 0o666)
		} else if err = os.Remove(gatePath); os.IsNotExist(err) {
			err = nil // already well
		}
		if err != nil {
			// Unable to record it means the healthcheck would disagree with
			// what the test was told, which is worse than failing here.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			fmt.Fprintf(os.Stderr, "patient: %v\n", err)
			return
		}
		fmt.Printf("patient: now %s\n", verdict())
		fmt.Fprintln(w, verdict())
	}
}

// check is what the image's HEALTHCHECK runs. It reads the gate directly
// rather than asking the server, so a wedged server is unhealthy rather than
// reported as whatever it last said.
//
// A failing probe says why, on stdout, the way a real healthcheck does --
// `curl: (7) Failed to connect`. Docker keeps that text in State.Health.Log
// and autoheal puts it in the notification (upstream/81), so a scenario
// asserting the notification explains itself needs a patient that explains
// itself. The wording is deliberately distinctive: a test matching on it is
// then matching on the healthcheck's own words rather than on any text that
// happened to arrive.
func check() int {
	if _, err := os.Stat(gatePath); err == nil {
		fmt.Println("patient: the gate is closed, refusing connections")
		return 1
	}
	return 0
}

// post asks the running patient to change its gate, from a second process
// inside the same container.
func post(path string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://127.0.0.1:"+port()+path, "", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "patient: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "patient: %s\n", resp.Status)
		return 1
	}
	return 0
}

func verdict() string {
	if check() == 1 {
		return "sick"
	}
	return "ok"
}

func port() string {
	if v := os.Getenv("PORT"); v != "" {
		return v
	}
	return "8080"
}
