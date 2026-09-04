//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// The label table against a real daemon, in the mode the exclusion label
// exists for (upstream/126, upstream/73).
//
// Every patient is made genuinely sick before autoheal is allowed to look, so
// each one is eligible in every respect except what its label says. A restart
// that does not happen can then only be the label's doing.
//
// The three outcomes are asserted together rather than in separate tests
// because it is their separation that is the property: healing and excluding
// are both easy to get right alone, and the failure that matters is a value we
// cannot read collapsing into one of them.
func TestAllModeLabelTable(t *testing.T) {
	s := Up(t, "autoheal-labels/all-mode")

	patients := []string{
		"unlabelled", "explicit-true", "capital-true",
		"explicit-false", "capital-false", "gibberish", "typo", "bad-timeout",
	}

	before := map[string]time.Time{}
	for _, p := range patients {
		before[p] = s.StartedAt(p)
		s.MakeSick(p)
	}

	// Only now does autoheal get to look, so nothing is decided on timing.
	s.Start()

	// The containers that should be healed are the ones with something to wait
	// for; waiting on them first also guarantees autoheal has completed polls
	// in which it saw, and passed over, all the others.
	heal := []string{"unlabelled", "explicit-true", "capital-true"}
	for _, p := range heal {
		p := p
		s.waitFor(p+" to be restarted", 60*time.Second,
			func() bool { return s.StartedAt(p).After(before[p]) },
			func() string { return s.Logs("autoheal") })
	}

	// Several further polls, so "not restarted" below means autoheal decided
	// against these rather than merely not having reached them yet.
	time.Sleep(6 * time.Second)
	logs := s.Logs("autoheal")

	skip := []string{"explicit-false", "capital-false", "gibberish", "typo", "bad-timeout"}
	for _, p := range skip {
		if got := s.StartedAt(p); got.After(before[p]) {
			t.Errorf("%s should have been left alone but was restarted at %s\n%s", p, got, logs)
		}
		if got := s.Health(p); got != "unhealthy" {
			t.Errorf("%s should still be unhealthy, docker says %q\n%s", p, got, logs)
		}
	}

	// A value we cannot read is a fault, and saying so is what makes skipping
	// survivable -- silently dropping a container from coverage would be its
	// own invisible failure.
	for _, p := range []string{"gibberish", "typo"} {
		if !mentions(logs, p, "not a value I understand") {
			t.Errorf("autoheal skipped %s without reporting why\n%s", p, logs)
		}
	}
	if !mentions(logs, "bad-timeout", "not a whole number of seconds") {
		t.Errorf("autoheal skipped bad-timeout without reporting why\n%s", logs)
	}

	// A correct opt-out is working configuration, not a fault: it must not
	// narrate itself for as long as it holds.
	for _, p := range []string{"explicit-false", "capital-false"} {
		if strings.Contains(logs, p) {
			t.Errorf("a deliberate exclusion (%s) produced log noise\n%s", p, logs)
		}
	}
}

// The same table in the default mode. The property worth having here is the
// typo: filtering on the label's value server-side, as upstream does, means a
// container labelled "ture" never comes back from the daemon at all and cannot
// be reported. Asking only that the label exists is what makes it visible.
func TestLabelModeLabelTable(t *testing.T) {
	s := Up(t, "autoheal-labels/label-mode")

	patients := []string{
		"explicit-true", "capital-true", "explicit-false",
		"capital-false", "typo", "unlabelled",
	}

	before := map[string]time.Time{}
	for _, p := range patients {
		before[p] = s.StartedAt(p)
		s.MakeSick(p)
	}

	s.Start()

	for _, p := range []string{"explicit-true", "capital-true"} {
		p := p
		s.waitFor(p+" to be restarted", 60*time.Second,
			func() bool { return s.StartedAt(p).After(before[p]) },
			func() string { return s.Logs("autoheal") })
	}

	time.Sleep(6 * time.Second)
	logs := s.Logs("autoheal")

	for _, p := range []string{"explicit-false", "capital-false", "typo", "unlabelled"} {
		if got := s.StartedAt(p); got.After(before[p]) {
			t.Errorf("%s should have been left alone but was restarted at %s\n%s", p, got, logs)
		}
	}

	// Seen and reported, rather than filtered out of existence.
	if !mentions(logs, "typo", "not a value I understand") {
		t.Errorf("a container labelled \"ture\" was never reported -- it is invisible again\n%s", logs)
	}

	// And a container with no label at all remains none of autoheal's
	// business, in the strong sense: never even named.
	if strings.Contains(logs, "unlabelled") {
		t.Errorf("autoheal mentioned a container it was never asked to watch:\n%s", logs)
	}
}

// A misconfiguration lasts until somebody fixes it, and it is reported when it
// starts rather than on every poll -- a container left labelled "ture" over a
// weekend should not be the entire log.
func TestUnparseableLabelIsReportedOnce(t *testing.T) {
	s := Up(t, "autoheal-labels/all-mode")

	s.MakeSick("typo")
	s.Start()

	s.WaitForLine("autoheal", "not a value I understand")

	// Many more polls at AUTOHEAL_INTERVAL=1.
	time.Sleep(8 * time.Second)

	if n := s.Times("autoheal", "not a value I understand"); n != 1 {
		t.Errorf("reported the same bad label %d times, want once\n%s", n, s.Logs("autoheal"))
	}
}

// mentions reports whether autoheal said want about the container named name,
// on one line. Checking the two together matters: the logs describe several
// containers at once, and a test that only asked whether the phrase appeared
// anywhere would pass on a message about a different container entirely.
func mentions(logs, name, want string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, name) && strings.Contains(line, want) {
			return true
		}
	}
	return false
}
