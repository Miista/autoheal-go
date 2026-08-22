#!/usr/bin/env bash
# End-to-end tests against the real Docker daemon: builds the actual
# autoheal-go image and runs it as a real container against a real
# controllable target, asserting on real docker inspect/logs output —
# not calling Go functions in-process. Covers the four scenarios this
# tool exists for:
#   1. unhealthy container is restarted (and recovers)
#   2. crash-looping container: crash-loop notification is sent
#   3. crash-looping container crosses threshold: further restarts stop
#   4. a given-up container later becomes genuinely healthy: strikes clear
#      and normal restart behavior resumes
#
# The target's healthcheck outcome is controlled by writing a plain integer
# exit code into a file bind-mounted into the container; the healthcheck
# script just `exit`s with that code. Scratch dirs live under the repo (not
# /tmp) because Docker Desktop only reliably shares /Users paths, and a
# single-file bind mount into a not-yet-existing path gets created as a
# directory instead of a file (hit this live once already) — so we always
# mount a real, pre-existing parent directory.
#
# Usage: hack/e2e.sh   (run from the repo root; needs docker)
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD/.e2e"
IMAGE=autoheal-go:e2e

pass=0 fail=0

echo "== building $IMAGE"
docker build -q -t "$IMAGE" . >/dev/null

set_health() { # $1=dir $2=exitcode
  echo "$2" > "$1/healthcheck.status"
}

cleanup_scenario() {
  local dir=$1
  docker rm -f "${dir##*/}-target" "${dir##*/}-autoheal" >/dev/null 2>&1 || true
}

start_target() { # $1=dir $2=name -- boots the controllable target container
  local dir=$1 name=$2
  set_health "$dir" 0
  docker run -d --name "$name" \
    -l autoheal=true \
    -v "$dir:$dir" \
    --health-cmd="sh -c 'exit \$(cat $dir/healthcheck.status)'" \
    --health-interval=1s --health-retries=1 --health-start-period=1s --health-timeout=2s \
    alpine:3 sleep 3600 >/dev/null
}

start_autoheal() { # $1=dir $2=name $3=extra_env(space separated KEY=VAL)
  local dir=$1 name=$2 extra_env=$3
  local env_args=(-e AUTOHEAL_INTERVAL=2 -e AUTOHEAL_DEFAULT_STOP_TIMEOUT=1)
  for kv in $extra_env; do env_args+=(-e "$kv"); done
  docker run -d --name "$name" \
    "${env_args[@]}" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    "$IMAGE" >/dev/null
}

wait_for() { # $1=description $2=timeout_secs $3=check_command(eval'd)
  local i=0
  while [ "$i" -lt "$2" ]; do
    if eval "$3"; then return 0; fi
    sleep 1
    i=$((i + 1))
  done
  return 1
}

# --- Scenario 1: unhealthy container is restarted and recovers ---
scenario_restart_and_recover() {
  local name=restart-recover
  local dir="$ROOT/$name"
  echo "== scenario: $name"
  rm -rf "$dir" && mkdir -p "$dir"
  cleanup_scenario "$dir"

  start_target "$dir" "$name-target"
  target_id=$(docker inspect -f '{{.Id}}' "$name-target")
  set_health "$dir" 1
  wait_for "target becomes unhealthy" 15 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = unhealthy ]" \
    || { echo "   FAIL: target never went unhealthy"; fail=$((fail+1)); cleanup_scenario "$dir"; return; }

  start_autoheal "$dir" "$name-autoheal" ""

  local ok=1
  wait_for "autoheal restarts target" 20 "docker logs $name-autoheal 2>&1 | grep -q 'Successfully restarted the container'" \
    || { echo "   FAIL: autoheal never restarted target"; ok=0; }

  # recovery: flip healthy, confirm it settles healthy and no crash-loop fired
  set_health "$dir" 0
  wait_for "target becomes healthy again" 15 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = healthy ]" \
    || { echo "   FAIL: target never recovered to healthy"; ok=0; }

  docker logs "$name-autoheal" 2>&1 | grep -q "crash loop" && { echo "   FAIL: crash-loop notification fired on a one-off restart"; ok=0; }

  # same container identity throughout — restart, not recreate
  now_id=$(docker inspect -f '{{.Id}}' "$name-target")
  [ "$now_id" = "$target_id" ] || { echo "   FAIL: target container was recreated, not restarted"; ok=0; }

  cleanup_scenario "$dir"
  if [ "$ok" = 1 ]; then echo "   PASS"; pass=$((pass+1)); else fail=$((fail+1)); fi
}

# --- Scenario 2: crash-looping container gets a crash-loop notification ---
scenario_crashloop_notifies() {
  local name=crashloop-notify
  local dir="$ROOT/$name"
  echo "== scenario: $name"
  rm -rf "$dir" && mkdir -p "$dir"
  cleanup_scenario "$dir"

  start_target "$dir" "$name-target"
  set_health "$dir" 1
  wait_for "target becomes unhealthy" 15 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = unhealthy ]" \
    || { echo "   FAIL: target never went unhealthy"; fail=$((fail+1)); cleanup_scenario "$dir"; return; }

  # threshold=2, stays unhealthy throughout (never flips healthy) — a real crash loop
  start_autoheal "$dir" "$name-autoheal" "AUTOHEAL_CRASHLOOP_STRIKES=2"

  local ok=1
  wait_for "crash-loop notification fires" 30 "docker logs $name-autoheal 2>&1 | grep -q 'looks like a crash loop'" \
    || { echo "   FAIL: crash-loop notification never fired"; ok=0; }

  cleanup_scenario "$dir"
  if [ "$ok" = 1 ]; then echo "   PASS"; pass=$((pass+1)); else fail=$((fail+1)); fi
}

# --- Scenario 3: past threshold, autoheal stops attempting restarts ---
scenario_crashloop_gives_up() {
  local name=crashloop-giveup
  local dir="$ROOT/$name"
  echo "== scenario: $name"
  rm -rf "$dir" && mkdir -p "$dir"
  cleanup_scenario "$dir"

  start_target "$dir" "$name-target"
  set_health "$dir" 1
  wait_for "target becomes unhealthy" 15 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = unhealthy ]" \
    || { echo "   FAIL: target never went unhealthy"; fail=$((fail+1)); cleanup_scenario "$dir"; return; }

  start_autoheal "$dir" "$name-autoheal" "AUTOHEAL_CRASHLOOP_STRIKES=2"

  local ok=1
  wait_for "crash-loop notification fires" 30 "docker logs $name-autoheal 2>&1 | grep -q 'looks like a crash loop'" \
    || { echo "   FAIL: crash-loop notification never fired"; ok=0; fail=$((fail+1)); cleanup_scenario "$dir"; return; }

  restarts_at_giveup=$(docker logs "$name-autoheal" 2>&1 | grep -c "Successfully restarted the container")

  # give it several more poll cycles worth of time while still unhealthy —
  # restart count must NOT climb further once given up
  sleep 8
  restarts_after_wait=$(docker logs "$name-autoheal" 2>&1 | grep -c "Successfully restarted the container")
  [ "$restarts_after_wait" = "$restarts_at_giveup" ] || { echo "   FAIL: autoheal kept restarting after giving up ($restarts_at_giveup -> $restarts_after_wait)"; ok=0; }

  docker logs "$name-autoheal" 2>&1 | grep -q "found to be restarting - don't restart" && { echo "   FAIL: unexpected 'restarting' skip log — check scenario assumptions"; }

  cleanup_scenario "$dir"
  if [ "$ok" = 1 ]; then echo "   PASS"; pass=$((pass+1)); else fail=$((fail+1)); fi
}

# --- Scenario 4: a given-up container later becomes genuinely healthy ---
scenario_recovers_after_giveup() {
  local name=recover-after-giveup
  local dir="$ROOT/$name"
  echo "== scenario: $name"
  rm -rf "$dir" && mkdir -p "$dir"
  cleanup_scenario "$dir"

  start_target "$dir" "$name-target"
  set_health "$dir" 1
  wait_for "target becomes unhealthy" 15 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = unhealthy ]" \
    || { echo "   FAIL: target never went unhealthy"; fail=$((fail+1)); cleanup_scenario "$dir"; return; }

  start_autoheal "$dir" "$name-autoheal" "AUTOHEAL_CRASHLOOP_STRIKES=2"

  local ok=1
  wait_for "crash-loop notification fires (given up)" 30 "docker logs $name-autoheal 2>&1 | grep -q 'looks like a crash loop'" \
    || { echo "   FAIL: crash-loop notification never fired"; ok=0; fail=$((fail+1)); cleanup_scenario "$dir"; return; }

  restarts_at_giveup=$(docker logs "$name-autoheal" 2>&1 | grep -c "Successfully restarted the container")

  # now genuinely recover: flip the healthcheck to pass. The health-check's
  # own start-period + retries means "healthy" isn't instant — wait for it
  # for real, then hold there a beat so autoheal's own poll (on its own
  # separate interval) has a chance to observe and clear strikes before we
  # relapse it again.
  set_health "$dir" 0
  wait_for "target settles healthy" 20 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = healthy ]" \
    || { echo "   FAIL: target never became healthy"; ok=0; }
  sleep 5

  # relapse: go unhealthy again — if strikes were cleared, autoheal should
  # attempt a fresh restart rather than staying silently given-up
  set_health "$dir" 1
  wait_for "target becomes unhealthy again" 15 "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $name-target)\" = unhealthy ]" \
    || { echo "   FAIL: target never relapsed to unhealthy"; ok=0; }

  wait_for "autoheal attempts a fresh restart after recovery+relapse" 20 \
    "[ \"\$(docker logs $name-autoheal 2>&1 | grep -c 'Successfully restarted the container')\" -gt \"$restarts_at_giveup\" ]" \
    || { echo "   FAIL: autoheal never restarted again after the container was confirmed healthy and relapsed — still stuck given-up"; ok=0; }

  cleanup_scenario "$dir"
  if [ "$ok" = 1 ]; then echo "   PASS"; pass=$((pass+1)); else fail=$((fail+1)); fi
}

scenario_restart_and_recover
scenario_crashloop_notifies
scenario_crashloop_gives_up
scenario_recovers_after_giveup

echo "== e2e: $pass passed, $fail failed"
rm -rf "$ROOT"
exit $((fail > 0))
