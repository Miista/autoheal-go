package main

import "testing"

func TestStrikeTracker_CrossesThresholdOnce(t *testing.T) {
	s := newStrikeTracker(3)

	crossed, count := s.recordRestart("abc")
	if crossed || count != 1 {
		t.Fatalf("strike 1: crossed=%v count=%d, want false,1", crossed, count)
	}
	crossed, count = s.recordRestart("abc")
	if crossed || count != 2 {
		t.Fatalf("strike 2: crossed=%v count=%d, want false,2", crossed, count)
	}
	crossed, count = s.recordRestart("abc")
	if !crossed || count != 3 {
		t.Fatalf("strike 3: crossed=%v count=%d, want true,3", crossed, count)
	}
	if !s.isGivenUp("abc") {
		t.Fatal("expected container to be marked given-up after crossing threshold")
	}
}

func TestStrikeTracker_ClearOnConfirmedHealthy(t *testing.T) {
	s := newStrikeTracker(3)
	s.recordRestart("abc")
	s.recordRestart("abc")

	s.clear("abc")

	crossed, count := s.recordRestart("abc")
	if crossed || count != 1 {
		t.Fatalf("after clear, strike count should restart from 1, got crossed=%v count=%d", crossed, count)
	}
	if s.isGivenUp("abc") {
		t.Fatal("clear should also reset given-up state")
	}
}

func TestStrikeTracker_AllowsRealertAfterClearAndRelapse(t *testing.T) {
	s := newStrikeTracker(2)

	s.recordRestart("abc")
	crossed, _ := s.recordRestart("abc")
	if !crossed {
		t.Fatal("expected threshold crossed on 2nd strike")
	}

	// Recovers fully (confirmed healthy), then relapses later — should be
	// able to alert (and give up) again, not permanently suppressed.
	s.clear("abc")
	s.recordRestart("abc")
	crossed, count := s.recordRestart("abc")
	if !crossed || count != 2 {
		t.Fatalf("after relapse, expected a fresh threshold-cross at count 2, got crossed=%v count=%d", crossed, count)
	}
}

func TestStrikeTracker_IndependentPerContainer(t *testing.T) {
	s := newStrikeTracker(2)

	s.recordRestart("abc")
	crossed, _ := s.recordRestart("abc")
	if !crossed {
		t.Fatal("container abc should have crossed threshold")
	}

	// A different container starts fresh, unaffected by abc's state.
	crossed, count := s.recordRestart("xyz")
	if crossed || count != 1 {
		t.Fatalf("container xyz: crossed=%v count=%d, want false,1", crossed, count)
	}
}

func TestStrikeTracker_TrackedReflectsActiveState(t *testing.T) {
	s := newStrikeTracker(3)
	s.recordRestart("abc")
	s.recordRestart("xyz")

	tracked := s.tracked()
	if !tracked["abc"] || !tracked["xyz"] {
		t.Fatalf("expected both abc and xyz tracked, got %v", tracked)
	}

	s.clear("xyz")
	tracked = s.tracked()
	if tracked["xyz"] {
		t.Fatal("xyz should no longer be tracked after clear")
	}
	if !tracked["abc"] {
		t.Fatal("abc should still be tracked")
	}
}

func TestStrikeTracker_NotGivenUpBelowThreshold(t *testing.T) {
	s := newStrikeTracker(3)
	s.recordRestart("abc")
	s.recordRestart("abc")
	if s.isGivenUp("abc") {
		t.Fatal("should not be given up before crossing threshold")
	}
}

// A condition that lasts should be logged when it starts, not on every poll.
// A container paused for a twenty-minute backup is one line at a five-second
// interval, or two hundred and forty.
func TestFirstMentionSpeaksOncePerState(t *testing.T) {
	s := newStrikeTracker(3)

	if !s.firstMention("abc", "paused") {
		t.Error("the first time a state is seen should be worth saying")
	}
	for i := 0; i < 5; i++ {
		if s.firstMention("abc", "paused") {
			t.Fatal("the same state should not be worth saying again")
		}
	}

	// A different container is a different event, however alike the state.
	if !s.firstMention("def", "paused") {
		t.Error("another container's pause is its own event")
	}

	// And so is a genuine change: paused, running, paused again is two pauses
	// a reader wants to see, not one.
	if !s.firstMention("abc", "running") {
		t.Error("a change of state should be worth saying")
	}
	if !s.firstMention("abc", "paused") {
		t.Error("being paused again after running is a new pause")
	}
}

// Clearing a container's history forgets what was said about it, so a pause
// after a recovery is reported rather than swallowed as a repeat.
func TestClearForgetsWhatWasMentioned(t *testing.T) {
	s := newStrikeTracker(3)

	s.firstMention("abc", "paused")
	s.clear("abc")

	if !s.firstMention("abc", "paused") {
		t.Error("after clearing, the same state should be worth saying again")
	}
}
