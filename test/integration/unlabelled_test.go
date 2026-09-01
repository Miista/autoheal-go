//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// A container without the label is not autoheal's business, and autoheal
// should not so much as name it.
//
// Stronger than "it was not restarted", which is what the subtest in
// restart_test.go checks. Not restarting could still mean autoheal looked at
// the container, considered it, and decided against -- and an implementation
// that filtered at the wrong point would pass that while doing a great deal of
// work it had no business doing. The label is meant to be a filter, not a veto
// applied late.
//
// It also catches the log noise that comes with getting this wrong: someone
// running autoheal over a busy host should not have their log filled with
// containers they never opted in.
func TestUnlabelledContainerIsNeverMentioned(t *testing.T) {
	s := Up(t, "autoheal-restart/unhealthy")

	before := s.StartedAt("sick-unlabelled")

	// Genuinely unhealthy, and unhealthy before autoheal starts: everything a
	// labelled container would need to be acted on, except the label.
	s.MakeSick("sick-unlabelled")
	s.Start()

	// Several polls at AUTOHEAL_INTERVAL=1. Sleeping is honest here: the
	// assertion is that nothing happens, and there is no event to wait for.
	time.Sleep(6 * time.Second)

	logs := s.Logs("autoheal")

	// Still sick, so this is not a container that quietly got better.
	if got := s.Health("sick-unlabelled"); got != "unhealthy" {
		t.Errorf("the container should still be unhealthy, docker says %q\n%s", got, logs)
	}
	if got := s.StartedAt("sick-unlabelled"); got.After(before) {
		t.Errorf("an unlabelled container was restarted at %s\n%s", got, logs)
	}

	// And autoheal never had an opinion about it.
	if strings.Contains(logs, "sick-unlabelled") {
		t.Errorf("autoheal mentioned a container it was never asked to watch:\n%s", logs)
	}
}
