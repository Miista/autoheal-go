# autoheal-go

A from-scratch Go rewrite of [`willfarrell/autoheal`](https://github.com/willfarrell/docker-autoheal)
(restarts unhealthy Docker containers), with one behavioral addition:
**crash-loop detection**. Upstream restarts an unhealthy container forever
with no memory of how many times it's already tried — fine for a one-off
fix, but it gives no signal when restarting isn't actually helping.

Everything else mirrors upstream's surface and logic: same env vars, same
defaults, same restart/notify/post-restart-script flow.

## Drop-in compatibility

Verified against both `willfarrell/autoheal:1.2.0` (the version actually
deployed on optiplex at the time of writing) and `:latest` (which added
webhook/apprise/post-restart-script/only-monitor-running — 1.2.0 predates
all of them). This port targets `:latest`'s feature set but is a safe
drop-in for a 1.2.0 deployment: every env var 1.2.0 reads
(`AUTOHEAL_CONTAINER_LABEL`, `DOCKER_SOCK`, `AUTOHEAL_START_PERIOD`,
`AUTOHEAL_INTERVAL`, `AUTOHEAL_DEFAULT_STOP_TIMEOUT`) behaves the same way
with the same default, and the extra vars this port adds beyond 1.2.0 are
all additive with no-op defaults — unset, they don't change behavior for
an existing 1.2.0-style setup (same `docker.sock` mount, same restart
policy, no new required config).

Two cosmetic differences, neither functional:
- Log line timestamp format differs (`log.Printf`'s default vs. upstream's
  `DD-MM-YYYY HH:MM:SS`) — nothing in this fleet parses autoheal's log
  lines today, confirmed no Gatus/alerting rule depends on the format.
- The "container has no name" skip case is detected differently
  (`len(Names) == 0` here vs. upstream's string-compare against the
  literal `"null"` its `jq` pipeline produces) — same underlying Docker
  API condition, same outcome (skip, don't restart), different code path
  to get there.

## Why a rewrite instead of a patch

Upstream is a ~180-line POSIX `sh` script (`docker-entrypoint`) with no
persistent state across loop iterations beyond what it can stuff in a file.
Docker's own `RestartCount` looked like a natural fit for tracking
consecutive failures, but it does **not** cover this case: it only
increments on automatic restart-policy-triggered restarts (the daemon's own
`RestartManager`, on container process exit), not on the manual
`POST /containers/{id}/restart` API call this tool (and upstream) uses.
Confirmed against moby/moby's `daemon/restart.go` and `daemon/monitor.go`.

That ruled out reusing Docker's counter, and ruled out a state file on disk
(a stale on-disk count surviving this watcher's own restart could falsely
re-arm or falsely suppress an alert — see `strikes.go`'s doc comment for the
full reasoning). A real long-running process with in-memory state was the
only option that gives an honest signal, hence the Go rewrite.

## Crash-loop behavior

- Tracks consecutive restart attempts per container, in memory only (not
  persisted — resets if the watcher itself restarts).
- Once a container crosses `AUTOHEAL_CRASHLOOP_STRIKES` (default `3`)
  restarts in a row without becoming genuinely healthy, autoheal-go:
  1. Fires one crash-loop notification (not one per restart past the
     threshold — a single alert, not a flood).
  2. **Stops attempting further restarts** on that container — repeated
     blind restarts that aren't fixing anything just add noise and risk.
- A given-up container is **not** ignored forever: every cycle,
  autoheal-go still checks whether it has become genuinely healthy (a real
  `healthy` health status, not merely "dropped out of the unhealthy list" —
  see below). Once confirmed healthy, its strike count clears and it's
  eligible for normal restart + crash-loop detection again on any future
  relapse.

### Why "genuinely healthy," not "not currently unhealthy"

A container passes through Docker's `starting` health state (during
`health-start-period`) after every restart — neither `healthy` nor
`unhealthy`, and absent from the `health=unhealthy`-filtered container
list. Treating "absent from that list" as recovery was a real bug caught
during testing: a container with any start-period at all had its strike
count silently wiped on every other poll cycle, so it could restart
forever without ever crossing even a threshold of 2. Fixed by checking
each tracked container's actual health status directly
(`GET /containers/{id}/json`) before clearing its strikes.

## Environment variables

All upstream variables are supported, unchanged:

| Variable | Default | Notes |
|---|---|---|
| `DOCKER_SOCK` | `/var/run/docker.sock` | Also accepts `tcp://host:port`. `tcps://` (TLS) is **not** ported — upstream's cert paths were hardcoded to a setup this deployment never had. |
| `CURL_TIMEOUT` | `30` (seconds) | HTTP client timeout to the Docker API. |
| `WEBHOOK_URL` | unset | POSTed a JSON body `{"<key>": "<text>"}` on each notification. |
| `WEBHOOK_JSON_KEY` | `text` | The JSON key used above. |
| `APPRISE_URL` | unset | POSTed `{"title": "Autoheal", "body": "<text>"}`. |
| `POST_RESTART_SCRIPT` | unset | Run after every successful restart, args: `name shortID state timeoutSeconds`. |
| `AUTOHEAL_CONTAINER_LABEL` | `autoheal` | Set to `all` to monitor every unhealthy container regardless of label. |
| `AUTOHEAL_START_PERIOD` | `0` (seconds) | Delay before the first check. |
| `AUTOHEAL_INTERVAL` | `5` (seconds) | Poll interval. |
| `AUTOHEAL_DEFAULT_STOP_TIMEOUT` | `10` (seconds) | Restart timeout; overridable per-container via the `autoheal.stop.timeout` label. |
| `AUTOHEAL_ONLY_MONITOR_RUNNING` | `false` | Only consider containers currently in `running` state. |

New in this rewrite:

| Variable | Default | Notes |
|---|---|---|
| `AUTOHEAL_CRASHLOOP_STRIKES` | `3` | Consecutive restarts without recovery before giving up and alerting once. |

`notify()` also always logs the notification text via the container's own
log output (`docker logs`), regardless of whether `WEBHOOK_URL`/`APPRISE_URL`
are configured — so crash-loop and restart events are visible even with no
webhook set up.

## Timezone

`FROM scratch` has no `/usr/share/zoneinfo`. The binary embeds Go's
`time/tzdata` package, so setting `-e TZ=Europe/Stockholm` (or any IANA
zone name) resolves correctly and affects log timestamps. Without `TZ` set,
logs are in UTC — same as most `FROM scratch` tools in this fleet (see the
Gatus wiki page for the same gotcha).

## Building

```sh
docker build -t autoheal-go .
```

Multi-stage: `golang:1.26-alpine` compiles a static binary
(`CGO_ENABLED=0`), then `FROM scratch` copies in just that binary. No
shell, no package manager, no `curl`/`jq` in the final image — smaller
attack surface than upstream's Alpine-based image, though not smaller in
raw size (~6.2 MB here vs. ~4 MB for `willfarrell/autoheal:1.2.0`; Alpine
+ `curl`/`jq` compresses tighter than a Go static binary with its runtime).

## Testing

```sh
go test ./...          # unit tests: strike-tracking logic
hack/e2e.sh             # integration: builds the real image, runs it as a
                        # real container against a real controllable target
```

`hack/e2e.sh` needs Docker and covers, against the actual containerized
binary (not in-process function calls):

1. An unhealthy container is restarted and recovers — no crash-loop noise
   on a one-off.
2. A crash-looping container gets a crash-loop notification.
3. Past the strike threshold, autoheal-go stops attempting further
   restarts while the container stays unhealthy.
4. A given-up container that later becomes genuinely healthy has its
   strikes cleared, and a subsequent relapse gets a fresh restart attempt
   — proving it's not permanently stuck given-up.

Scratch dirs live under the repo (`.e2e/`, gitignored), not `/tmp` — Docker
Desktop only reliably shares paths under `/Users`, and a single-file bind
mount into a not-yet-existing path gets silently created as a directory
instead of a file.
