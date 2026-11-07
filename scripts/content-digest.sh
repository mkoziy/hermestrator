#!/usr/bin/env bash
# Starts one content-factory digest Issue and waits for its terminal state.
# Invoked by the content-digest-* dagu DAGs. Only `completed` is success.
# The app has no auth (VPN-only): CSRF double-submit, same as its SPA.
set -Eeuo pipefail

: "${FORMAT:?FORMAT is required}"
: "${TOPIC:?TOPIC is required}"
: "${RUN_ID:?RUN_ID is required}"
readonly cf_url=http://content-factory.content-factory.svc.cluster.local:8080
readonly cf_host=content-factory.vpn.internal # must be in the app's APP_ALLOWED_HOSTS
readonly poll_seconds=30

command -v curl >/dev/null || { printf 'ERROR: curl is required\n' >&2; exit 1; }
command -v jq >/dev/null || { printf 'ERROR: jq is required\n' >&2; exit 1; }

jar="$(mktemp)"
trap 'rm -f "$jar"' EXIT
# On 4xx/5xx the app's error body goes to stderr (dagu shows it), not into the caller's $(...).
cf() {
  local out
  out="$(curl --fail-with-body -sS -H "Host: $cf_host" -b "$jar" -c "$jar" "$@")" || { printf '%s\n' "$out" >&2; return 1; }
  printf '%s' "$out"
}

cf -m 30 -o /dev/null "$cf_url/healthz" # any GET sets the csrf cookie
csrf="$(awk '$6 == "csrf" { print $7 }' "$jar")"
[[ -n "$csrf" ]] || { printf 'ERROR: no csrf cookie from %s\n' "$cf_url" >&2; exit 1; }

# The start is synchronous (feed + source capture, up to ~5 min); the key makes
# a re-run of the same dagu run return the existing Issue instead of a new one.
issue="$(cf -m 400 -X POST "$cf_url/api/digests" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg k "dagu-$RUN_ID" --arg f "$FORMAT" --arg t "$TOPIC" '{idempotency_key:$k, format:$f, topic:$t}')")"
id="$(jq -er '.data.issue.id' <<<"$issue")"
printf 'Issue %s (%s/%s) started\n' "$id" "$FORMAT" "$TOPIC"

# A manual dagu Retry reuses the key, so the app returns the old, already
# failed Issue: ask the app to retry it (fresh sources) instead of re-polling.
case "$(jq -r '.data.issue.state' <<<"$issue")" in
  failed | completed_with_failures)
    cf -m 400 -X POST "$cf_url/api/digests/$id/retry" -H "X-CSRF-Token: $csrf" -o /dev/null
    printf 'Issue %s was %s, retry requested\n' "$id" "$(jq -r '.data.issue.state' <<<"$issue")" ;;
esac

last=""
while :; do
  body="$(cf -m 30 "$cf_url/api/digests/$id")"
  state="$(jq -r '.data.issue.state' <<<"$body")"
  progress="$state $(jq -r '[.data.targets[]? | "\(.language)=\(.state)"] | join(" ")' <<<"$body")"
  [[ "$progress" == "$last" ]] || { printf '%s\n' "$progress"; last="$progress"; }
  case "$state" in
    completed) exit 0 ;;
    failed | completed_with_failures)
      jq -r '(.data.attempts[]? | select(.diagnostic != "") | "attempt \(.number): \(.diagnostic)"),
             (.data.targets[]? | select(.diagnostic != "") | "\(.language): \(.diagnostic)")' <<<"$body" >&2
      exit 1 ;;
  esac
  sleep "$poll_seconds"
done
