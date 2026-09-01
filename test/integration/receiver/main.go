// receiver stands in for a webhook endpoint: it accepts notifications and records
// them where a test can read them.
//
// A container like everything else in a scenario, rather than a server in the
// test process. The subject runs in a container and posts over the network, so a
// receiver on the host would be reachable only through a gateway address that
// nothing in production uses -- and the fixture could not simply declare it
// as a service.
//
// It exists because the unit tests fake the notifier, so what they cannot
// show is whether a notification fired at all, fired once rather than every
// run, and named the right thing. That needs something on the other end of
// the request.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// logPath is where notifications are recorded, mounted from the scenario so a
// test can read it without entering the container.
const logPath = "/log/requests.log"

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// ntfy answers other methods; only a publish is a notification.
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)

		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer f.Close()

		// One line per notification: "Title|body", with the body flattened so
		// that counting lines counts notifications.
		fmt.Fprintf(f, "%s|%s\n",
			r.Header.Get("Title"),
			strings.ReplaceAll(string(body), "\n", " "))
		w.WriteHeader(http.StatusOK)
	})

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}
