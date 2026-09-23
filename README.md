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
| `AUTOHEAL_CONTAINER_LABEL` | `autoheal` | The label to read on watched containers. Set to `all` to watch every unhealthy container by default; see [Container labels](#container-labels). |
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
| `AUTOHEAL_NOTIFY_PREFIX` | the hostname | Stamped on every notification so alerts say which host they came from. Set to an empty string to turn it off. |

## Container labels

Whether a container is autoheal's to restart is decided by a label on **that
container** (`AUTOHEAL_CONTAINER_LABEL` is an environment variable on the
autoheal container, and names the label to look for — it is never itself a
label on a watched container).

Below, "the label" means the one `AUTOHEAL_CONTAINER_LABEL` names — `autoheal`
unless you changed it, and `autoheal` in `all` mode too, since `all` is a mode
rather than a label name.

| Label | `AUTOHEAL_CONTAINER_LABEL=all` | Default (label) mode |
|---|---|---|
| absent | **restarted** — `all` means everything is in by default | ignored, never even named in the log |
| `true` `True` `1` `yes` `on` | restarted | **restarted** |
| `false` `False` `0` `no` `off` | **left alone**, silently | left alone, silently |
| anything else | **left alone, and logged as an error** | left alone, and logged as an error |

Both capitalisations are accepted on purpose. `docker compose` coerces an
unquoted YAML boolean, so `autoheal: true` reaches Docker as the string
`"True"` while `autoheal: "true"` stays lowercase — the same intent written two
ways, and upstream honouring only one of them is the whole of
[upstream #110](https://github.com/willfarrell/docker-autoheal/issues/110) and
[#46](https://github.com/willfarrell/docker-autoheal/issues/46).

Excluding a container with `autoheal=false` works in `all` mode, which is the
point of it — opting one container out of a hundred rather than labelling the
other ninety-nine in
([upstream #126](https://github.com/willfarrell/docker-autoheal/issues/126),
[#73](https://github.com/willfarrell/docker-autoheal/issues/73)).

### Unreadable configuration is refused, not guessed at

A label that is **present but not one of the values above** — `ture`, `falsch`,
`maybe` — is a mistake somebody made while trying to say something, and
autoheal-go will not guess which. The container is left alone and the problem
is logged once, when it is first seen:

```
container-name has autoheal="ture", which is not a value I understand — leaving it alone until that is fixed (use true or false)
```

### Restarting or recreating

`autoheal.mode` on a watched container chooses how it is healed:

| Value | Meaning |
|---|---|
| absent, `restart` | `docker restart` — stop and start the same container. The default, and what upstream does. |
| `recreate` | `docker compose up -d --force-recreate <service>` — rebuild the container from its compose file. |
| anything else | left alone, and logged as an error (same rule as the `autoheal` label above) |

**Recreate exists because a restart cannot fix a container whose stored
configuration has gone stale.** The case it was written for is
`network_mode: service:<other>`: docker resolves that to a concrete container
ID once, when the container is created, and stores it. Recreate the peer — a
VPN sidecar being updated, say — and the ID it points at no longer exists. A
restart replays the same stored ID and fails with `No such container` every
time, so the container sits at `Exited (128)` while autoheal retries and gets
nowhere. Only rebuilding from the compose file re-resolves the peer.

There is no recreate endpoint in the Docker API, and there cannot be one: the
daemon stores a container's finished configuration, not the desired state it
came from. The compose file lives on the client's disk and the daemon has
never seen it. So recreate runs compose, and needs two mounts:

```yaml
autoheal:
  image: ghcr.io/miista/autoheal-go:latest
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
    # The host's own compose plugin. Mounted rather than shipped so the
    # version always matches the host's -- compose computes the config hash
    # that decides what a recreate produces.
    - /usr/libexec/docker/cli-plugins/docker-compose:/docker-compose:ro
    # The project directory, at the SAME path it has on the host.
    - /home/you/docker/myproject:/home/you/docker/myproject:ro
```

The project path has to match the host's, because compose resolves `.env`
files, relative bind mounts and `include:` against it — a translated path
would not fail, it would silently resolve to a different configuration and
rebuild the container wrong. Read-only is enough; compose writes nothing to
the project.

Everything else is read from the container's own
`com.docker.compose.*` labels, so there is nothing to configure per container
and no project path to tell autoheal about. A container that was not created
by compose has no compose file to rebuild it from, and is reported rather than
guessed at — as is a project directory that is not mounted, which compose
would otherwise describe as a missing file.

The compose binary Docker ships is statically linked, so it runs in this
image's `FROM scratch` container with no shell and no docker CLI. Verified
against compose v5.1.4 and v5.5.1.

Crash-loop strikes follow the container across a recreate. Rebuilding gives
the service a new container ID, and a count keyed on the old one would restart
from zero every cycle — so a service that recreates and fails forever would
never be recognised as a crash loop.

The same applies to `autoheal.stop.timeout`: a value that is not a whole number
of seconds skips the container rather than silently falling back to
`AUTOHEAL_DEFAULT_STOP_TIMEOUT`. Restarting on a 10-second default when the
author asked for `240` is its own kind of wrong, and is what
[upstream #119](https://github.com/willfarrell/docker-autoheal/issues/119)
reports as the label "not working".

Absence is not a fault — a container with no label at all has expressed no
intent, and in `all` mode the default still applies to it. Only a value that is
there and unreadable is refused.

One consequence worth knowing: because `AUTOHEAL_CONTAINER_LABEL` carries both
meanings — a label name, or the mode `all` — setting it to `all` leaves no
label name configured, and the conventional `autoheal` is read for exclusions.
So a custom label name and `all` mode cannot currently be combined. Upstream
overloads the variable the same way; separating the two would need a new
variable and is deliberately not done here, to keep existing compose files
working unchanged.

**Note for `all`-mode upgrades:** a container carrying a misspelled label is
restarted by upstream (and by earlier builds of this port) and is *not*
restarted here. That is deliberate — healing something whose owner may have
been trying to exclude it is the wrong way to be wrong — but it is a behaviour
change, and the error log is how you find the containers it applies to.

## What a notification says

Notifications name the host and carry the healthcheck's own explanation, so an
alert can be acted on without going to the machine to find out what happened:

```
[prod-db-1] nginx (a1b2c3d4e5f6) was unhealthy and has been restarted. The last healthcheck said: curl: (7) Failed to connect to localhost port 8080
```

- **Which host** — container names repeat across machines, and an alert about
  `nginx` restarting does not say which `nginx`
  ([upstream #118](https://github.com/willfarrell/docker-autoheal/issues/118)).
  `AUTOHEAL_NOTIFY_PREFIX` overrides the hostname; an empty string turns it off.
- **Why** — the healthcheck already worked out the diagnosis and it was being
  thrown away
  ([upstream #81](https://github.com/willfarrell/docker-autoheal/issues/81)).
  The first line of its last output is included, trimmed to 200 characters. A
  healthcheck that prints nothing (a bare `exit 1`) leaves the clause off
  rather than trailing an empty one. Read before the restart, since afterwards
  the health log belongs to the new process.
- **What went wrong, when it did** — a failed restart quotes the daemon's own
  message (`No such container`, `device or resource busy`) instead of a bare
  status code
  ([upstream #111](https://github.com/willfarrell/docker-autoheal/issues/111)).

## Crash-loop behavior

Once a container crosses `AUTOHEAL_CRASHLOOP_STRIKES` consecutive restarts
without becoming healthy, autoheal-go fires a single crash-loop
notification and stops attempting further restarts on it. It keeps
checking that container's health every cycle regardless — once confirmed
genuinely healthy again, the strike count clears and normal restart/
crash-loop behavior resumes on any future relapse.

### Stopping a container to investigate it

A container you stop by hand stays in Docker's unhealthy list — stopping it
freezes its health at whatever it last reported, it does not clear it — so
autoheal sees an unhealthy container and starts it again, out from under you
([upstream #22](https://github.com/willfarrell/docker-autoheal/issues/22)).

There is no flag on a stopped container saying who stopped it. `exited` looks
the same whether an admin ran `docker stop`, the process crashed, or it was
OOM-killed — and restarting it is the whole point of autoheal in the latter
two. So this is not something autoheal-go decides for you. Three ways to get
what you want, in rough order of how narrow they are:

- **`autoheal=false` on that container.** Takes it out of scope entirely until
  you take the label off. The narrowest option and the one to reach for if the
  container is going to sit stopped for a while.
- **`AUTOHEAL_ONLY_MONITOR_RUNNING=true`.** Autoheal then only considers
  containers currently `running`, so anything stopped is left alone —
  including containers that died and would otherwise have been restarted.
  A host-wide setting, so it changes the answer for everything.
- **Nothing.** The crash-loop behaviour above is usually the reason people
  reach for this: a container restarting forever is exactly what
  `AUTOHEAL_CRASHLOOP_STRIKES` stops, and once autoheal has given up on a
  container it will not restart it again until it is confirmed healthy. You
  can stop it and take your time.

## Drop-in compatibility

Same env vars, same defaults as `willfarrell/autoheal:1.2.0`; the extra
vars above are additive and no-ops when unset. `AUTOHEAL_CONTAINER_LABEL`
keeps upstream's double duty — normally the name of the label to read, with
`all` reserved as a mode — rather than splitting the two apart, so an existing
compose file needs no edit.

Two cosmetic-only differences: log line format, and how a nameless container is
detected internally (same outcome either way).

One deliberate behavioural difference: a container whose label is present but
unreadable is left alone and reported, where upstream restarts it (in `all`
mode) or silently ignores it (in label mode). See [Container
labels](#container-labels). Correct configuration behaves identically to
upstream in both modes; only a value neither of us can read is treated
differently.

## Building

```sh
docker build -t autoheal-go .
```

Multi-stage build producing a `FROM scratch` static binary — no shell, no
package manager in the final image.

## Testing

```sh
make test-unit          # needs nothing but Go
make test-integration   # needs docker; runs everything locally, works offline
make test               # both
```

The integration suite in `test/integration` builds the real shipped image and
runs it against real containers, one compose fixture per behaviour
(`hack/fixtures/`). It covers: an unhealthy container being restarted and
recovering, a healthy one being left alone, recreate mode replacing the
container where restart mode keeps it, crash-loop detection (including across
the new container IDs a recreate produces), paused and unlabelled containers,
label parsing, and notification content.

The recreate scenarios need a Linux compose binary to mount into the subject.
On Linux the host's own plugin is used, which is what a real deployment does;
on macOS, where the Docker Desktop CLI is a Mach-O binary that cannot run in a
Linux container, one is extracted from `docker/compose-bin` and cached under
`testbed/`.
