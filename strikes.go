package main

import (
	"sync"
	"time"
)

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
	// mentioned records the states already reported for a container, so a
	// condition that lasts is logged when it starts rather than on every poll.
	// A container paused for a twenty-minute backup is one line, not one line
	// every AUTOHEAL_INTERVAL.
	mentioned map[string]string
	// stranded holds services a recreate failed partway through, keyed by
	// container name -- see strand.
	stranded map[string]strandedService
}

// strandedService is what it takes to try a failed recreate again: the
// labels name the compose project, and the container that carried them may
// no longer exist to be asked.
type strandedService struct {
	labels    map[string]string
	lastTried time.Time
	// attempts counts the failed recreates so far, paces the next retry (see
	// strandedRetryAfter), and since marks when the first one failed, so a
	// long outage can be told apart from a blip. Both belong to one stranding:
	// unstrand drops them with the entry, so the next one starts over.
	attempts int
	since    time.Time
	notified bool
}

func newStrikeTracker(threshold int) *strikeTracker {
	return &strikeTracker{
		threshold: threshold,
		counts:    make(map[string]int),
		givenUp:   make(map[string]bool),
		mentioned: make(map[string]string),
		stranded:  make(map[string]strandedService),
	}
}

// strand records a service whose recreate failed, so it is tried again.
//
// A failed restart needs none of this: the same container is still there and
// still unhealthy, so the next poll lists it and tries again by itself. A
// failed recreate is different. Compose stops and removes the old container
// before it creates and starts the new one, so a failure in the second half
// leaves a container in "created" (or nothing at all), with no health status
// to match the unhealthy filter. Nothing would ever list it again, and the
// service would stay down with autoheal unaware of it -- which is how
// qbittorrent sat in "created" for hours after its VPN dependency was
// unhealthy at the moment of the recreate.
//
// Keyed by name, because that is the only handle that survives a recreate.
func (s *strikeTracker) strand(name string, labels map[string]string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, again := s.stranded[name]
	if !again {
		svc = strandedService{since: at}
	}
	svc.labels, svc.lastTried = labels, at
	svc.attempts++
	s.stranded[name] = svc
}

// dueStrandedNotice reports whether this stranding has lasted long enough to
// be worth telling a human about, and records that it has been told so it
// is said once per stranding, not on every retry. A recovery clears the
// record, so a later, separate stranding earns its own notice.
func (s *strikeTracker) dueStrandedNotice(name string, at time.Time, after time.Duration) (strandedService, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.stranded[name]
	if !ok || svc.notified || at.Sub(svc.since) < after {
		return svc, false
	}
	svc.notified = true
	s.stranded[name] = svc
	return svc, true
}

// unstrand forgets a stranded service, once it is running again by any means.
func (s *strikeTracker) unstrand(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stranded, name)
	delete(s.mentioned, "stranded:"+name)
}

// strandedServices returns a copy of the stranded set, so the caller can
// retry each one without holding the lock across a compose run.
func (s *strikeTracker) strandedServices() map[string]strandedService {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]strandedService, len(s.stranded))
	for name, svc := range s.stranded {
		out[name] = svc
	}
	return out
}

// firstMention reports whether this is the first cycle a container has been
// seen in the given state, and records it either way.
//
// The state is remembered rather than a bare "already said": a container that
// goes paused, runs again and is paused a second time is two separate events a
// reader wants to see, and comparing the state distinguishes that from the
// same pause continuing.
func (s *strikeTracker) firstMention(containerID, state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mentioned[containerID] == state {
		return false
	}
	s.mentioned[containerID] = state
	return true
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
	// A container that recovered starts over in every sense: if it is paused
	// again later, that is a new event and worth saying so.
	delete(s.mentioned, containerID)
}

// transfer moves all strike state from one container ID to another.
//
// Recreating a container produces a new object with a new ID, so without this
// the history would be left behind on an ID that no longer exists: the old
// entry would never clear (the recovery check asks Docker about a container it
// cannot find), and -- the part that matters -- the replacement would start at
// zero strikes every cycle, so a service that recreates and fails, recreates
// and fails, would never be recognised as a crash loop. Carrying the count
// across is what makes AUTOHEAL_CRASHLOOP_STRIKES mean the same thing in both
// heal modes.
func (s *strikeTracker) transfer(oldID, newID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if oldID == newID {
		return
	}
	if n, ok := s.counts[oldID]; ok {
		s.counts[newID] = n
		delete(s.counts, oldID)
	}
	if s.givenUp[oldID] {
		s.givenUp[newID] = true
		delete(s.givenUp, oldID)
	}
	// Deliberately not carried: mentioned is keyed to states the old container
	// was observed in, and the replacement has not been seen in any of them
	// yet. A fresh container earning a fresh "is paused" line is correct.
	delete(s.mentioned, oldID)
}
