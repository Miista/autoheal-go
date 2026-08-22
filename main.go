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
	"net"
	"net/http"
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
	DockerSock                string
	CurlTimeout               time.Duration
	WebhookURL                string
	WebhookJSONKey            string
	AppriseURL                string
	NtfyURL                   string
	NtfyToken                 string
	PostRestartScript         string
	ContainerLabel            string
	StartPeriod               time.Duration
	Interval                  time.Duration
	DefaultStopTimeout        int
	OnlyMonitorRunning        bool
	CrashLoopStrikeThreshold  int
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
		PostRestartScript:        os.Getenv("POST_RESTART_SCRIPT"),
		ContainerLabel:           getEnv("AUTOHEAL_CONTAINER_LABEL", "autoheal"),
		StartPeriod:              time.Duration(getEnvInt(log, "AUTOHEAL_START_PERIOD", 0)) * time.Second,
		Interval:                 time.Duration(getEnvInt(log, "AUTOHEAL_INTERVAL", 5)) * time.Second,
		DefaultStopTimeout:       getEnvInt(log, "AUTOHEAL_DEFAULT_STOP_TIMEOUT", 10),
		OnlyMonitorRunning:       getEnv("AUTOHEAL_ONLY_MONITOR_RUNNING", "false") == "true",
		CrashLoopStrikeThreshold: getEnvInt(log, "AUTOHEAL_CRASHLOOP_STRIKES", 3),
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
	isHealthy(containerID string) (bool, error)
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
// health=unhealthy, plus the autoheal label (unless set to "all"), plus
// status=running if AUTOHEAL_ONLY_MONITOR_RUNNING is set.
func (d *dockerClient) listUnhealthy(cfg config) ([]containerSummary, error) {
	filters := map[string][]string{"health": {"unhealthy"}}
	if cfg.ContainerLabel != "all" {
		filters["label"] = []string{cfg.ContainerLabel + "=true"}
	}
	if cfg.OnlyMonitorRunning {
		filters["status"] = []string{"running"}
	}
	filterJSON, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/containers/json?filters=%s", d.endpoint, urlQueryEscape(string(filterJSON)))
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
		return fmt.Errorf("docker API returned %d restarting %s", resp.StatusCode, containerID)
	}
	return nil
}

func urlQueryEscape(s string) string {
	// net/url.QueryEscape would also do, but avoids importing net/url just
	// for this one call given http.NewRequest already needs it internally —
	// kept explicit for clarity that this is escaping a JSON blob into a
	// query string, exactly like upstream's inline sed/curl composition.
	q := make([]byte, 0, len(s)*3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			q = append(q, c)
		} else {
			q = append(q, '%', hexDigit(c>>4), hexDigit(c&0xf))
		}
	}
	return string(q)
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + (b - 10)
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
		if cfg.ContainerLabel != "all" && c.Labels["autoheal"] == "False" {
			continue
		}
		if len(c.Names) == 0 {
			log.Warn().Msgf("A container (%s) with no name showed up as unhealthy — it likely no longer exists, so leaving it alone", shortID(c.ID))
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		shortIDStr := shortID(c.ID)
		seenUnhealthy[c.ID] = true

		if c.State == "restarting" {
			log.Info().Msgf("%s is already restarting, so leaving it alone this cycle", name)
			continue
		}

		// Once confirmed crash-looping, further blind restarts aren't
		// helping — stop attempting them and just keep the container
		// flagged for a human. Still counted as "seen unhealthy" above so
		// its strike state isn't cleared out from under it while we wait.
		if strikes.isGivenUp(c.ID) {
			continue
		}

		timeout := cfg.DefaultStopTimeout
		if v, ok := c.Labels["autoheal.stop.timeout"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				timeout = n
			}
		}

		log.Info().Msgf("%s is unhealthy — restarting it now (%ds timeout)", name, timeout)
		if err := client.restart(c.ID, timeout); err != nil {
			log.Error().Err(err).Msgf("Tried to restart %s but the restart itself failed", name)
			go notify(log, cfg, fmt.Sprintf("%s (%s) is unhealthy and the restart attempt failed.", name, shortIDStr))
			continue
		}

		log.Info().Msgf("Restarted %s successfully", name)
		go notify(log, cfg, fmt.Sprintf("%s (%s) was unhealthy and has been restarted.", name, shortIDStr))
		go runPostRestartScript(log, cfg, name, shortIDStr, c.State, timeout)

		// Crash-loop tracking: only increments when unhealthy AND we just
		// restarted it — a container that's unhealthy but "restarting"
		// (skipped above) or that fails the restart call itself doesn't
		// count as a strike here, since we didn't successfully complete a
		// heal attempt for it.
		if crossed, count := strikes.recordRestart(c.ID); crossed {
			log.Warn().Msgf("%s has now been restarted %d times in a row without recovering — this looks like a crash loop. Giving up on further automatic restarts until it's confirmed healthy again", name, count)
			go notify(log, cfg, fmt.Sprintf("%s (%s) has crash-looped: %d unhealthy restarts in a row with no recovery. Giving up on further automatic restarts — needs manual attention.", name, shortIDStr, count))
		}
	}

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
