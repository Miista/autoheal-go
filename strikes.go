package main

import "sync"

// strikeTracker is in-memory, deliberately not persisted to disk.
//
// Why not a file: a strike count is only meaningful while this process is
// actively watching a container. If autoheal-go itself restarts, there's no
// principled way to know whether the crash loop it was tracking is still
// happening or resolved in the gap — a stale on-disk count would either
// falsely re-arm an alert for a problem that's long since fixed, or (worse)
// silently suppress a real one if the file survived a container recreate
// that reset the underlying problem. In-memory state that resets alongside
// the watcher's own lifecycle is the more honest failure mode: worst case,
// a crash-loop that started right as autoheal-go itself restarted takes a
// few extra strikes to notice, which is strictly better than acting on
// state we can no longer vouch for.
//
// Reset semantics (two-condition design, confirmed against Docker's own
// RestartCount which does NOT cover this — see main.go's package comment):
//   - increment when a container is unhealthy AND we successfully restart it
//   - clear when a container is confirmed genuinely healthy (NOT merely
//     "not currently unhealthy" — a restarting container passes through
//     Docker's "starting" health state, which is neither; treating that as
//     recovery was a real bug caught live, see main.go's isHealthy() doc
//     comment)
//
// Crash-loop response: once a container's strikes cross the threshold, we
// stop attempting further restarts for it (isGivenUp) — repeated blind
// restarts that aren't fixing anything just add noise and risk, and the
// crash-loop notification already told a human to look at it. A container
// only leaves "given up" state by being confirmed genuinely healthy (same
// clear() path as a normal strike reset), which also clears the given-up
// flag — so recovery (manual fix, image update, etc.) re-arms normal
// autoheal behavior with no separate un-give-up step needed.
type strikeTracker struct {
	mu        sync.Mutex
	threshold int
	counts    map[string]int
	givenUp   map[string]bool
}

func newStrikeTracker(threshold int) *strikeTracker {
	return &strikeTracker{
		threshold: threshold,
		counts:    make(map[string]int),
		givenUp:   make(map[string]bool),
	}
}

// recordRestart increments the strike count for containerID and reports
// whether this call just crossed the threshold for the first time (so the
// caller fires exactly one crash-loop notification, not one per strike
// past the threshold — matches the "one-time at threshold, silent after"
// alert behavior decided for this tool). Crossing the threshold also marks
// the container as given-up: see isGivenUp.
func (s *strikeTracker) recordRestart(containerID string) (crossed bool, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counts[containerID]++
	count = s.counts[containerID]

	if count >= s.threshold && !s.givenUp[containerID] {
		s.givenUp[containerID] = true
		return true, count
	}
	return false, count
}

// isGivenUp reports whether containerID has crossed the crash-loop
// threshold and should no longer receive automatic restart attempts.
func (s *strikeTracker) isGivenUp(containerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.givenUp[containerID]
}

// tracked returns the set of container IDs currently holding any strike
// state (count > 0 or given-up), so the caller can check each one's real
// health status once it drops out of the unhealthy-filtered list.
func (s *strikeTracker) tracked() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.counts))
	for id := range s.counts {
		out[id] = true
	}
	return out
}

// clear drops all strike state for containerID — used once it's confirmed
// genuinely healthy. A future recurrence starts counting from zero and is
// eligible to alert (and give up) again.
func (s *strikeTracker) clear(containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counts, containerID)
	delete(s.givenUp, containerID)
}
