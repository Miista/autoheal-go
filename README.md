# autoheal-go

A Go rewrite of [`willfarrell/autoheal`](https://github.com/willfarrell/docker-autoheal)
that restarts unhealthy Docker containers, plus one addition: **crash-loop
detection**. Upstream restarts an unhealthy container forever with no memory
of how many times it's already tried; this port stops after too many
failed attempts in a row and alerts once instead of retrying forever.

Same env-var surface, defaults, and restart/notify flow as upstream —
drop-in compatible, see below.

## Environment variables

All upstream variables, unchanged:

| Variable | Default | Notes |
|---|---|---|
| `DOCKER_SOCK` | `/var/run/docker.sock` | Also accepts `tcp://host:port`. `tcps://` (TLS) is not supported. |
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
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`. |
| `TZ` | unset (UTC) | IANA zone name, e.g. `Europe/Stockholm`, for local log timestamps. |
| `NTFY_URL` | unset | Full [ntfy](https://ntfy.sh) topic URL, e.g. `https://ntfy.sh/mytopic`. Sends a plain-text message, not JSON. |
| `NTFY_TOKEN` | unset | Bearer token for `NTFY_URL`, sent as a proper `Authorization` header. Omit for a public topic. |

## Crash-loop behavior

Once a container crosses `AUTOHEAL_CRASHLOOP_STRIKES` consecutive restarts
without becoming healthy, autoheal-go fires a single crash-loop
notification and stops attempting further restarts on it. It keeps
checking that container's health every cycle regardless — once confirmed
genuinely healthy again, the strike count clears and normal restart/
crash-loop behavior resumes on any future relapse.

## Drop-in compatibility

Same env vars, same defaults as `willfarrell/autoheal:1.2.0`; the extra
vars above are additive and no-ops when unset. Two cosmetic-only
differences: log line format, and how a nameless container is detected
internally (same outcome either way).

## Building

```sh
docker build -t autoheal-go .
```

Multi-stage build producing a `FROM scratch` static binary — no shell, no
package manager in the final image.

## Testing

```sh
go test ./... -cover   # unit tests: strike tracking + restart/give-up/clear logic
hack/e2e.sh             # builds the real image, runs it against a real target container
```

`hack/e2e.sh` needs Docker and covers: an unhealthy container being
restarted and recovering, a crash-looping container getting notified,
restarts stopping once past the strike threshold, and a given-up
container recovering and resuming normal behavior on a later relapse.
