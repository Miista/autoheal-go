// autoheal-go is a from-scratch Go port of willfarrell/docker-autoheal
// (https://github.com/willfarrell/docker-autoheal, docker-entrypoint, 180
// lines of POSIX sh). Same env-var interface, same core loop semantics —
// verified line-by-line against the real upstream script, not a summary of
// it. The one behavioral addition is crash-loop awareness (see strikes.go):
// upstream restarts an unhealthy container forever with no memory of how
// many times it's already tried, which is fine for a one-off fix but gives
// no signal when restarting isn't actually helping. Docker's own
// RestartCount does NOT cover this — it only increments on automatic
// restart-policy restarts (container process exit), not on the manual
// `docker restart` API call autoheal (and this port) uses, confirmed
// against moby/moby's daemon/restart.go and daemon/monitor.go. So the
// strike count has to be our own in-memory state, which is also why this
// needed to be a real long-running process rather than a stateless POSIX
// sh loop like upstream — sh has no persistent variables across a `while
// true` loop's subshells without resorting to a state file on disk, which
// was deliberately ruled out (see strikes.go's doc comment for why).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // FROM scratch has no /usr/share/zoneinfo; embed it so TZ works for log timestamps

	"github.com/rs/zerolog"
)

type config struct {
	DockerSock               string
	CurlTimeout              time.Duration
	WebhookURL               string
	WebhookJSONKey           string
	AppriseURL               string
	NtfyURL                  string
	NtfyToken                string
	NotifyPrefix             string
	PostRestartScript        string
	ContainerLabel           string
	StartPeriod              time.Duration
	Interval                 time.Duration
	DefaultStopTimeout       int
	OnlyMonitorRunning       bool
	CrashLoopStrikeThreshold int
	// StrandedRetryIntervals and StrandedNotifyAfter govern a recreate that
	// failed partway; see defaultStrandedIntervals. Zero values mean the
	// defaults, so a config built by hand in a test need not set them.
	StrandedRetryIntervals []time.Duration
	StrandedNotifyAfter    time.Duration
}

func (c config) strandedNotifyAfter() time.Duration {
	if c.StrandedNotifyAfter <= 0 {
		return defaultStrandedNotifyAfter
	}
	return c.StrandedNotifyAfter
}

func loadConfig(log zerolog.Logger) config {
	return config{
		DockerSock:               getEnv("DOCKER_SOCK", "/var/run/docker.sock"),
		CurlTimeout:              time.Duration(getEnvInt(log, "CURL_TIMEOUT", 30)) * time.Second,
		WebhookURL:               os.Getenv("WEBHOOK_URL"),
		WebhookJSONKey:           getEnv("WEBHOOK_JSON_KEY", "text"),
		AppriseURL:               os.Getenv("APPRISE_URL"),
		NtfyURL:                  os.Getenv("NTFY_URL"),
		NtfyToken:                os.Getenv("NTFY_TOKEN"),
		NotifyPrefix:             notifyPrefix(log),
		PostRestartScript:        os.Getenv("POST_RESTART_SCRIPT"),
		ContainerLabel:           getEnv("AUTOHEAL_CONTAINER_LABEL", "autoheal"),
		StartPeriod:              time.Duration(getEnvInt(log, "AUTOHEAL_START_PERIOD", 0)) * time.Second,
		Interval:                 time.Duration(getEnvInt(log, "AUTOHEAL_INTERVAL", 5)) * time.Second,
		DefaultStopTimeout:       getEnvInt(log, "AUTOHEAL_DEFAULT_STOP_TIMEOUT", 10),
		OnlyMonitorRunning:       getEnv("AUTOHEAL_ONLY_MONITOR_RUNNING", "false") == "true",
		CrashLoopStrikeThreshold: getEnvInt(log, "AUTOHEAL_CRASHLOOP_STRIKES", 3),
		StrandedRetryIntervals:   getEnvIntervals(log, "AUTOHEAL_STRANDED_RETRY_INTERVALS", defaultStrandedIntervals),
		StrandedNotifyAfter:      getEnvDuration(log, "AUTOHEAL_STRANDED_NOTIFY_AFTER", defaultStrandedNotifyAfter),
	}
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getEnvInt(log zerolog.Logger, key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Warn().Msgf("invalid int for %s=%q, using default %d", key, v, def)
		return def
	}
	return n
}

// dockerAPI is the slice of the Docker Engine API runOnce needs. It exists so
// runOnce's restart/strike/give-up/clear decision logic can be unit-tested
// against a scripted fake — no real daemon, no container, and no mocking
// library, just a struct satisfying this interface. *dockerClient below is
// the real implementation; tests use a fake satisfying this interface
// instead.
type dockerAPI interface {
	listUnhealthy(cfg config) ([]containerSummary, error)
	restart(containerID string, timeoutSeconds int) error
	// containerID resolves a container name to its current ID. Recreating
	// replaces the container object, so the ID autoheal was tracking is
	// stale the moment compose returns; this is how the replacement is found.
	containerID(name string) (string, error)
	// containerState reports a container's state ("running", "created", ...)
	// by name, with found false if there is no such container. Used to check
	// on services a failed recreate left behind -- see strikeTracker.strand.
	containerState(name string) (state string, found bool, err error)
	isHealthy(containerID string) (bool, error)
	lastHealthOutput(containerID string) string
}

// dockerClient talks to the Docker Engine API the same way upstream's
// docker_curl() does: over the Unix socket by default, or a TCP endpoint if
// DOCKER_SOCK is set to a tcp://... URL. TLS (tcps://) is intentionally not
// ported — it was never used in this deployment and upstream's cert paths
// are hardcoded to /certs/*, not something worth carrying forward blind.
type dockerClient struct {
	http     *http.Client
	endpoint string
}

var _ dockerAPI = (*dockerClient)(nil)

func newDockerClient(cfg config) (*dockerClient, error) {
	switch {
	case strings.HasPrefix(cfg.DockerSock, "tcp://"):
		return &dockerClient{
			http:     &http.Client{Timeout: cfg.CurlTimeout},
			endpoint: strings.Replace(cfg.DockerSock, "tcp://", "http://", 1),
		}, nil
	case strings.HasPrefix(cfg.DockerSock, "tcps://"):
		return nil, fmt.Errorf("tcps:// (TLS) is not supported by this port — only used by upstream for a cert setup this deployment never had")
	default:
		if _, err := os.Stat(cfg.DockerSock); err != nil {
			return nil, fmt.Errorf("unix socket %s is not available: %w", cfg.DockerSock, err)
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", cfg.DockerSock)
			},
		}
		return &dockerClient{
			http:     &http.Client{Transport: transport, Timeout: cfg.CurlTimeout},
			endpoint: "http://localhost",
		}, nil
	}
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// listUnhealthy mirrors get_container_info(): server-side filtered by
// health=unhealthy, plus status=running if AUTOHEAL_ONLY_MONITOR_RUNNING is
// set, plus -- unless the label is "all" -- the presence of the autoheal
// label.
//
// Presence, not "=true", is deliberate. Filtering on the value server-side
// means a container whose label says anything else never comes back at all,
// and a container we never see is one we cannot warn about: the `autoheal:
// ture` typo of upstream/119 would stay as invisible here as it is upstream.
// Asking for the label and deciding on its value in runOnce costs one
// slightly wider response and is what makes a misconfigured container
// reportable rather than silently unwatched.
func (d *dockerClient) listUnhealthy(cfg config) ([]containerSummary, error) {
	filters := map[string][]string{"health": {"unhealthy"}}
	if cfg.ContainerLabel != "all" {
		filters["label"] = []string{cfg.ContainerLabel}
	}
	if cfg.OnlyMonitorRunning {
		filters["status"] = []string{"running"}
	}
	filterJSON, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}

	// Named endpoint rather than url: the package is what escapes the filter.
	endpoint := fmt.Sprintf("%s/containers/json?filters=%s",
		d.endpoint, url.QueryEscape(string(filterJSON)))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker API returned %d listing containers", resp.StatusCode)
	}
	var containers []containerSummary
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	return containers, nil
}

// isHealthy checks a single container's CURRENT health status directly,
// used only for strike-clearing decisions (see strikes.go's doc comment).
// Deliberately distinct from "absent from the unhealthy list" — a
// restarting container passes through Docker's "starting" health state
// (during health-start-period) where it's neither healthy nor unhealthy,
// and briefly disappears from the unhealthy-filtered list as a result.
// Treating that as recovery would silently reset the strike count on every
// single restart cycle for a container whose healthcheck has any
// start-period at all — confirmed live: a test container with a 1s
// start-period never crossed a strikes=2 threshold despite 5+ consecutive
// real restarts, because every other poll caught it mid-"starting" and
// wiped its count back to zero before the next unhealthy poll could
// increment it past 1.
func (d *dockerClient) isHealthy(containerID string) (bool, error) {
	url := fmt.Sprintf("%s/containers/%s/json", d.endpoint, containerID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Container is gone entirely — not "healthy", but also not
		// something to keep tracking; caller treats this as clearable.
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("docker API returned %d inspecting %s", resp.StatusCode, containerID)
	}
	var detail struct {
		State struct {
			Health *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return false, err
	}
	if detail.State.Health == nil {
		// No healthcheck configured (shouldn't happen for anything we're
		// tracking, since we only ever strike containers the unhealthy
		// filter matched, which requires a healthcheck to exist) — treat
		// as not-recovered rather than guess.
		return false, nil
	}
	return detail.State.Health.Status == "healthy", nil
}

// lastHealthOutput returns what the container's healthcheck said the last time
// it ran, trimmed to something that fits in an alert.
//
// "nginx is unhealthy and has been restarted" tells an operator nothing about
// why; the healthcheck already produced the diagnosis and it was being thrown
// away (upstream/81). Docker keeps the last few probe results in
// State.Health.Log, most recent last, each with the command's output and exit
// code.
//
// Best effort, and deliberately not an error return: this decorates a
// notification that is going out either way, and failing to read it must never
// stop the notification or turn into a second error to report.
func (d *dockerClient) lastHealthOutput(containerID string) string {
	url := fmt.Sprintf("%s/containers/%s/json", d.endpoint, containerID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var detail struct {
		State struct {
			Health *struct {
				Log []struct {
					ExitCode int    `json:"ExitCode"`
					Output   string `json:"Output"`
				} `json:"Log"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return ""
	}
	if detail.State.Health == nil || len(detail.State.Health.Log) == 0 {
		return ""
	}
	return summarizeHealthOutput(detail.State.Health.Log[len(detail.State.Health.Log)-1].Output)
}

// restart mirrors restart_container(): POST /containers/{id}/restart?t=timeout.
func (d *dockerClient) restart(containerID string, timeoutSeconds int) error {
	url := fmt.Sprintf("%s/containers/%s/restart?t=%d", d.endpoint, containerID, timeoutSeconds)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("docker API returned %d restarting %s: %s",
			resp.StatusCode, containerID, dockerErrorMessage(resp.Body))
	}
	return nil
}

// containerID looks up a container by name. Used after a recreate, where the
// name is the only stable handle: compose recreates a service into a new
// container object, and with container_name pinned the name survives while the
// ID does not.
func (d *dockerClient) containerID(name string) (string, error) {
	url := fmt.Sprintf("%s/containers/%s/json", d.endpoint, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("docker API returned %d looking up %s: %s",
			resp.StatusCode, name, dockerErrorMessage(resp.Body))
	}
	var detail struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return "", err
	}
	return detail.ID, nil
}

// containerState looks up a container's state by name. A 404 is an answer,
// not an error: a recreate can fail after the old container is removed and
// before the new one exists.
func (d *dockerClient) containerState(name string) (string, bool, error) {
	url := fmt.Sprintf("%s/containers/%s/json", d.endpoint, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("docker API returned %d looking up %s: %s",
			resp.StatusCode, name, dockerErrorMessage(resp.Body))
	}
	var detail struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return "", false, err
	}
	return detail.State.Status, true, nil
}

// dockerErrorMessage pulls the human-readable half out of a Docker error
// response. The daemon explains itself in the body -- {"message":"No such
// container: ..."} -- and throwing that away left "docker API returned 500"
// as the entire diagnosis, which is upstream/111 asking how to find out why a
// restart failed. There is no way to answer that from a status code.
//
// Best-effort by design: this is already the error path, and a body that is
// missing, truncated or not JSON must not turn a failed restart into a
// different, more confusing failure. Anything unreadable falls back to a
// description of what was there.
func dockerErrorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil {
		return "unreadable response body"
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Message != "" {
		return payload.Message
	}
	if text := strings.TrimSpace(string(raw)); text != "" {
		return text
	}
	return "no detail given"
}

func main() {
	log := newLogger(getEnv("LOG_LEVEL", "info"))

	cfg := loadConfig(log)
	log.Info().Msgf("Watching containers labeled %q, checking every %s, giving up after %d unhealthy restarts in a row", cfg.ContainerLabel, cfg.Interval, cfg.CrashLoopStrikeThreshold)
	if cfg.OnlyMonitorRunning {
		log.Info().Msg("Only monitoring containers that are currently running")
	}

	client, err := newDockerClient(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("Couldn't connect to the Docker daemon")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if cfg.StartPeriod > 0 {
		log.Info().Msgf("Waiting %s before the first check", cfg.StartPeriod)
		select {
		case <-time.After(cfg.StartPeriod):
		case <-ctx.Done():
			return
		}
	}

	strikes := newStrikeTracker(cfg.CrashLoopStrikeThreshold)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		runOnce(log, client, cfg, strikes)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runOnce(log zerolog.Logger, client dockerAPI, cfg config, strikes *strikeTracker) {
	containers, err := client.listUnhealthy(cfg)
	if err != nil {
		log.Error().Err(err).Msg("Couldn't ask Docker which containers are unhealthy — will try again next cycle")
		return
	}

	seenUnhealthy := make(map[string]bool, len(containers))

	for _, c := range containers {
		// The label decides whether this container is ours to touch. Three
		// outcomes, and the third is the reason this is a function call and
		// not an equality test:
		//
		//   absent      -- no intent expressed. In "all" mode the default
		//                  applies and we heal it; in label mode the server
		//                  side filter meant we never got it at all.
		//   understood  -- true heals, false leaves it alone, silently. A
		//                  deliberate exclusion is correct configuration and
		//                  must not log every poll for as long as it holds.
		//   present but
		//   unparseable -- somebody tried to say something and we cannot tell
		//                  what. Say so and skip: acting on a guess is the
		//                  irreversible half, and healing a container whose
		//                  owner may have been trying to exclude it is the
		//                  wrong way to be wrong.
		if raw, ok := c.Labels[cfg.labelName()]; ok {
			want, understood := parseLabelBool(raw)
			if !understood {
				if strikes.firstMention(c.ID, "bad-label:"+raw) {
					log.Error().Msgf("%s has %s=%q, which is not a value I understand — leaving it alone until that is fixed (use true or false)", nameOf(c), cfg.labelName(), raw)
				}
				continue
			}
			if !want {
				continue
			}
		}
		// Note both skips above return before seenUnhealthy is recorded below,
		// so a container we have been told not to touch -- or cannot read the
		// instructions for -- holds no strike state at all. That is the
		// difference from the paused case further down, which is deliberately
		// recorded: a paused container is one of ours that has stopped being
		// asked, while these were never ours to count.
		if len(c.Names) == 0 {
			log.Warn().Msgf("A container (%s) with no name showed up as unhealthy — it likely no longer exists, so leaving it alone", shortID(c.ID))
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		shortIDStr := shortID(c.ID)
		seenUnhealthy[c.ID] = true

		// Same rule as the autoheal label itself: a stop timeout that is
		// present but unusable is a mistake worth reporting, not something to
		// quietly paper over with the default. Restarting on a 10s default
		// when the author asked for 240s is its own kind of wrong -- it is the
		// complaint in upstream/119, where the label appeared to be ignored --
		// so we skip rather than guess at how long is safe to allow.
		//
		// Checked here, with the other configuration, and deliberately above
		// the state handling below: everything from "restarting" onward calls
		// firstMention with the container state, and interleaving a second key
		// against the same one-slot-per-container record would make the two
		// alternate and re-log on every poll. Config is settled first, then
		// state.
		timeout := cfg.DefaultStopTimeout
		if v, ok := c.Labels["autoheal.stop.timeout"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				if strikes.firstMention(c.ID, "bad-timeout:"+v) {
					log.Error().Msgf("%s has autoheal.stop.timeout=%q, which is not a whole number of seconds — leaving it alone until that is fixed", name, v)
				}
				continue
			}
			timeout = n
		}

		// Same rule again: a mode nobody can read is refused rather than
		// defaulted. Silently restarting a container whose author asked for a
		// recreate would reproduce the exact failure the label exists to fix,
		// and do it invisibly.
		mode := healRestart
		if v, ok := c.Labels["autoheal.mode"]; ok {
			m, understood := parseHealMode(v)
			if !understood {
				if strikes.firstMention(c.ID, "bad-mode:"+v) {
					log.Error().Msgf("%s has autoheal.mode=%q, which is not a mode I understand — leaving it alone until that is fixed (use restart or recreate)", name, v)
				}
				continue
			}
			mode = m
		}

		if c.State == "restarting" {
			log.Info().Msgf("%s is already restarting, so leaving it alone this cycle", name)
			continue
		}

		// A paused container is somebody's deliberate act -- `docker compose
		// pause` while a backup runs -- not a fault to repair. Its processes
		// are frozen, so its healthcheck stops answering and docker calls it
		// unhealthy soon after, which is what made this worth guarding: it
		// looks exactly like something to fix.
		//
		// Restarting it would resume it, which is the one thing the person who
		// paused it was avoiding, and it would then keep looking unhealthy
		// until it had been written off as a crash loop. upstream/98.
		//
		// Still recorded as seen above, so being paused holds its strike
		// history where it is rather than clearing it: the container has not
		// recovered, it has only stopped being asked.
		// Said once, when the pause starts. A backup can hold a container
		// paused for twenty minutes, and a line per poll would bury everything
		// else in the log for as long as it lasted.
		if c.State == "paused" {
			if strikes.firstMention(c.ID, c.State) {
				log.Info().Msgf("%s is paused, so leaving it alone until it is resumed", name)
			}
			continue
		}
		strikes.firstMention(c.ID, c.State)

		// Once confirmed crash-looping, further blind restarts aren't
		// helping — stop attempting them and just keep the container
		// flagged for a human. Still counted as "seen unhealthy" above so
		// its strike state isn't cleared out from under it while we wait.
		if strikes.isGivenUp(c.ID) {
			continue
		}

		// Read before restarting: once the container comes back its health log
		// belongs to the new process, and whatever it said while failing --
		// the thing worth putting in the alert -- is gone.
		because := client.lastHealthOutput(c.ID)

		// Both modes are a heal attempt and are counted, notified and
		// crash-loop tracked identically below; they differ only in how the
		// container is brought back.
		var healErr error
		if mode == healRecreate {
			log.Info().Msgf("%s is unhealthy — recreating it from its compose file now", name)
			healErr = recreateService(log, name, c.Labels)
			if healErr != nil {
				strikes.strand(name, c.Labels, now())
			} else {
				strikes.unstrand(name)
			}
		} else {
			log.Info().Msgf("%s is unhealthy — restarting it now (%ds timeout)", name, timeout)
			healErr = client.restart(c.ID, timeout)
		}
		if err := healErr; err != nil {
			log.Error().Err(err).Msgf("Tried to %s %s but the attempt itself failed", mode, name)
			// The reason travels with the alert, not just the log. Somebody
			// reading "the restart failed" on their phone cannot go and grep,
			// and the daemon's own message is usually the whole diagnosis
			// ("No such container", "device or resource busy").
			go notify(log, cfg, withHealthOutput(
				fmt.Sprintf("%s (%s) is unhealthy and the %s attempt failed: %s", name, shortIDStr, mode, err),
				because))
			continue
		}

		// A recreate replaces the container object, so every later reference to
		// it -- the strike count, the recovery check next cycle -- has to move
		// to the new ID. Looked up by name because that is what survives:
		// container_name is pinned, the ID is not.
		//
		// A failure here is not a failed heal. The container is recreated and
		// running; all that is lost is the crash-loop history, so say so and
		// carry on rather than reporting the recreate as failed.
		healedID := c.ID
		if mode == healRecreate {
			newID, err := client.containerID(name)
			if err != nil {
				log.Warn().Err(err).Msgf("Recreated %s but couldn't find its new container ID — its crash-loop history starts over", name)
			} else {
				strikes.transfer(c.ID, newID)
				healedID = newID
			}
		}

		past := "restarted"
		if mode == healRecreate {
			past = "recreated"
		}
		log.Info().Msgf("Successfully %s %s", past, name)
		go notify(log, cfg, withHealthOutput(
			fmt.Sprintf("%s (%s) was unhealthy and has been %s.", name, shortIDStr, past),
			because))
		go runPostRestartScript(log, cfg, name, shortIDStr, c.State, timeout)

		// The replacement inherits the old container's place in this cycle's
		// seen set, so the recovery sweep at the end does not immediately ask
		// whether a container recreated seconds ago is healthy yet -- it will
		// still be in "starting", and clearing on that would wipe the history
		// that was just carried across.
		if healedID != c.ID {
			seenUnhealthy[healedID] = true
		}

		// Crash-loop tracking: only increments when unhealthy AND we just
		// restarted it — a container that's unhealthy but "restarting"
		// (skipped above) or that fails the restart call itself doesn't
		// count as a strike here, since we didn't successfully complete a
		// heal attempt for it.
		// Keyed on healedID, not c.ID: after a recreate the strike history has
		// just been moved there, and counting against the old ID would leave
		// the replacement permanently at zero.
		if crossed, count := strikes.recordRestart(healedID); crossed {
			log.Warn().Msgf("%s has now been restarted %d times in a row without recovering — this looks like a crash loop. Giving up on further automatic restarts until it's confirmed healthy again", name, count)
			go notify(log, cfg, fmt.Sprintf("%s (%s) has crash-looped: %d unhealthy restarts in a row with no recovery. Giving up on further automatic restarts — needs manual attention.", name, shortIDStr, count))
		}
	}

	retryStranded(log, client, cfg, strikes, seenUnhealthy)

	// Clear strikes only for tracked containers confirmed genuinely
	// healthy, not merely absent from the unhealthy-filtered list — a
	// restarting container passes through Docker's "starting" health state
	// (during health-start-period), which drops it out of that list
	// without it having actually recovered. Confirmed live: a container
	// with any health-start-period at all would otherwise have its strike
	// count wiped on every other poll, before it could ever reach a
	// threshold — see isHealthy()'s doc comment for the full story.
	for id := range strikes.tracked() {
		if seenUnhealthy[id] {
			continue
		}
		healthy, err := client.isHealthy(id)
		if err != nil {
			log.Error().Err(err).Msgf("Couldn't check whether %s has recovered — will try again next cycle", shortID(id))
			continue
		}
		if healthy {
			log.Info().Msgf("%s is healthy again — clearing its crash-loop history", shortID(id))
			strikes.clear(id)
		}
	}
}

// recreateService and now are seams for tests: the real recreate needs a
// compose binary and a project on disk, and the retry pacing needs a clock.
var (
	recreateService = recreate
	now             = time.Now
)

// defaultStrandedIntervals paces retries of a failed recreate. Every poll would
// be every five seconds by default, and each attempt is a full compose run that
// may sit waiting on a dependency's health; the thing that made the recreate
// fail (usually that dependency) does not recover on that timescale.
//
// Retries slow down the longer a recreate keeps failing, but never stop: what
// eventually brings a service back after its dependency does is one more
// attempt. So the list is how long to wait after the first failure, the
// second, and so on, and the last entry repeats for as long as it takes.
// (Hence no strikes either -- see retryStranded.)
var defaultStrandedIntervals = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}

// defaultStrandedNotifyAfter is how long a service must stay stranded before
// one notification says so. Shorter than that is a dependency restarting,
// which the recovery notice already covers.
const defaultStrandedNotifyAfter = 5 * time.Minute

// retryAfter is the wait before the next retry once attempts recreates have
// failed: the attempts'th interval, or the last one once the list runs out.
func retryAfter(intervals []time.Duration, attempts int) time.Duration {
	if len(intervals) == 0 {
		intervals = defaultStrandedIntervals
	}
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(intervals) {
		i = len(intervals) - 1
	}
	return intervals[i]
}

// parseIntervals reads a comma-separated list of durations such as
// "30s,1m,2m". Anything unreadable, empty or not positive is an error rather
// than a quietly shortened list: a retry interval of zero would be a busy loop.
func parseIntervals(raw string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a duration", part)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%q is not positive", part)
		}
		out = append(out, d)
	}
	return out, nil
}

func getEnvIntervals(log zerolog.Logger, key string, def []time.Duration) []time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	out, err := parseIntervals(v)
	if err != nil {
		log.Warn().Msgf("invalid intervals for %s=%q (%v), using default %v", key, v, err, def)
		return def
	}
	return out
}

func getEnvDuration(log zerolog.Logger, key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Warn().Msgf("invalid duration for %s=%q, using default %s", key, v, def)
		return def
	}
	return d
}

// retryStranded tries again to recreate the services a failed recreate left
// down -- see strikeTracker.strand for why the unhealthy filter cannot find
// them by itself.
//
// One that is running again, by our hand or anyone else's, is forgotten. One
// that is restarting or paused is somebody else's business for now and left
// alone. Anything else -- created, exited, dead, or gone entirely -- is
// recreated again.
//
// These retries do not count as strikes. The crash-loop count is about a
// container that comes up and fails its healthcheck; a stranded one never got
// far enough to have one, and counting would give up on a service that is
// only waiting for its dependency to come back.
func retryStranded(log zerolog.Logger, client dockerAPI, cfg config, strikes *strikeTracker, seenUnhealthy map[string]bool) {
	for name, svc := range strikes.strandedServices() {
		state, found, err := client.containerState(name)
		if err != nil {
			log.Error().Err(err).Msgf("Couldn't check on %s, which a failed recreate left down — will try again next cycle", name)
			continue
		}
		log.Debug().Msgf("Checking on %s, left down by a failed recreate: found=%t state=%q", name, found, state)
		if found {
			id, _ := client.containerID(name)
			// Listed as unhealthy this cycle means it is running and the
			// normal path has it; that path also re-strands it if the
			// recreate it just tried failed again.
			if seenUnhealthy[id] {
				continue
			}
			switch state {
			case "running":
				log.Info().Msgf("%s is running again — no longer retrying its recreate", name)
				strikes.unstrand(name)
				continue
			case "restarting", "paused":
				continue
			}
		}
		// Said once per stranding, and only while it is still down: a service
		// that is running, restarting or paused was skipped above.
		if due, ok := strikes.dueStrandedNotice(name, now(), cfg.strandedNotifyAfter()); ok {
			down := now().Sub(due.since).Round(time.Second)
			log.Warn().Msgf("%s has been down for %s across %d failed recreates — still retrying, no more notices about it until it recovers", name, down, due.attempts)
			go notify(log, cfg, fmt.Sprintf("%s has been down for %s after %d failed recreates. Autoheal is still retrying.", name, down, due.attempts))
		}
		if now().Sub(svc.lastTried) < retryAfter(cfg.StrandedRetryIntervals, svc.attempts) {
			continue
		}

		what := "missing"
		if found {
			what = "in state " + state
		}
		err = recreateService(log, name, svc.labels)
		if err != nil {
			at := now()
			strikes.strand(name, svc.labels, at)
			// Logged every attempt, though only the first notice is sent: the
			// log is where the whole history of an outage is read back, and
			// backing off keeps it to a few lines a minute.
			attempts := svc.attempts + 1
			log.Error().Err(err).Msgf("%s is still down (%s) after %d failed recreates over %s — will keep trying, next in %s", name, what, attempts, at.Sub(svc.since).Round(time.Second), retryAfter(cfg.StrandedRetryIntervals, attempts))
			continue
		}
		strikes.unstrand(name)
		log.Info().Msgf("Successfully recreated %s, which a failed recreate had left %s", name, what)
		go notify(log, cfg, fmt.Sprintf("%s was left %s by a failed recreate and has now been recreated.", name, what))
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// notify sends text to any configured webhook/Apprise/ntfy endpoint. The
// human-readable log line describing what happened is logged by the caller
// (runOnce) right before this is invoked — this only logs when it actually
// has somewhere external to send it, so nothing duplicates when no webhook
// is configured.
func notify(log zerolog.Logger, cfg config, text string) {
	if cfg.WebhookURL == "" && cfg.AppriseURL == "" && cfg.NtfyURL == "" {
		return
	}
	// Which machine is this? Container names repeat across hosts -- everybody
	// has an `nginx` -- so an alert saying one was restarted does not say
	// where, and upstream/118 is somebody running autoheal on several hosts
	// unable to tell the notifications apart. Only the notification is
	// prefixed: the log is already read in the context of the host it came
	// from, so stamping every line there would be noise.
	if cfg.NotifyPrefix != "" {
		text = "[" + cfg.NotifyPrefix + "] " + text
	}
	log.Debug().Msgf("Sending notification: %s", text)
	if cfg.WebhookURL != "" {
		payload, _ := json.Marshal(map[string]string{cfg.WebhookJSONKey: text})
		postJSON(log, cfg.WebhookURL, payload)
	}
	if cfg.AppriseURL != "" {
		payload, _ := json.Marshal(map[string]string{"title": "Autoheal", "body": text})
		postJSON(log, cfg.AppriseURL, payload)
	}
	if cfg.NtfyURL != "" {
		postNtfy(log, cfg.NtfyURL, cfg.NtfyToken, text)
	}
}

func postJSON(log zerolog.Logger, url string, payload []byte) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		log.Error().Err(err).Msgf("Couldn't build the notification request to %s", url)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Error().Err(err).Msgf("Couldn't send the notification to %s", url)
		return
	}
	defer resp.Body.Close()
}

// postNtfy publishes to ntfy's plain-text publish endpoint
// (POST <server>/<topic>, body = the message) with the token as a proper
// Authorization header, never embedded in the URL. NTFY_URL is the full
// topic URL (e.g. https://ntfy.sh/mytopic); NTFY_TOKEN is optional (public
// topics don't need one).
func postNtfy(log zerolog.Logger, url, token, text string) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(text))
	if err != nil {
		log.Error().Err(err).Msgf("Couldn't build the ntfy request to %s", url)
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Title", "Autoheal")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Error().Err(err).Msgf("Couldn't send the ntfy notification to %s", url)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Error().Msgf("ntfy returned %d publishing to %s", resp.StatusCode, url)
	}
}

func runPostRestartScript(log zerolog.Logger, cfg config, name, shortIDStr, state string, timeout int) {
	if cfg.PostRestartScript == "" {
		return
	}
	cmd := exec.Command(cfg.PostRestartScript, name, shortIDStr, state, strconv.Itoa(timeout))
	if err := cmd.Run(); err != nil {
		log.Error().Err(err).Msg("post-restart script failed")
	}
}
