#!/usr/bin/env bash

set -euo pipefail

PORT="${PORT:-18080}"
TAG="${TAG:-v0.21.7}"
IMAGE="localhost:${PORT}/engine:${TAG}"
BASE_URL="http://localhost:${PORT}"
TMPDIR="$(mktemp -d)"

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  rm -rf "${TMPDIR}"
}
trap cleanup EXIT

if ! command -v jq >/dev/null; then
  echo "install jq to inspect manifest JSON" >&2
  exit 1
fi

if command -v crane >/dev/null; then
  CRANE=(crane)
else
  CRANE=(go run github.com/google/go-containerregistry/cmd/crane@v0.11.0)
fi

go build -o "${TMPDIR}/registry-redirect" .

PORT="${PORT}" \
  "${TMPDIR}/registry-redirect" --repo=dagger >"${TMPDIR}/server.log" 2>&1 &
SERVER_PID="$!"

for _ in {1..50}; do
  status="$(curl -s -o /dev/null -w "%{http_code}" "${BASE_URL}/v2" || true)"
  if [[ "${status}" == "401" ]]; then
    break
  fi
  if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    cat "${TMPDIR}/server.log" >&2
    exit 1
  fi
  sleep 0.2
done

if [[ "${status}" != "401" ]]; then
  echo "server did not become ready; last /v2 status was ${status}" >&2
  cat "${TMPDIR}/server.log" >&2
  exit 1
fi

curl -sS -D "${TMPDIR}/v2.headers" -o "${TMPDIR}/v2.body" "${BASE_URL}/v2"
grep -q "Www-Authenticate: Bearer realm=\"https://localhost:${PORT}/token\"" "${TMPDIR}/v2.headers"

curl -fsS -D "${TMPDIR}/token.headers" \
  -o "${TMPDIR}/token.json" \
  "${BASE_URL}/token?scope=repository:engine:pull&service=ghcr.io"
jq -e '.token | length > 0' "${TMPDIR}/token.json" >/dev/null
grep -q "X-Redirected: https://ghcr.io/token?scope=repository%3Adagger%2Fengine%3Apull&service=ghcr.io" "${TMPDIR}/token.headers"

curl -fsS -D "${TMPDIR}/index.headers" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  -o "${TMPDIR}/index.json" \
  "${BASE_URL}/v2/engine/manifests/${TAG}"
index_digest="$(sed -n 's/[Dd]ocker-[Cc]ontent-[Dd]igest: \(.*\)\r$/\1/p' "${TMPDIR}/index.headers")"
child_digest="$(jq -r '.manifests[0].digest' "${TMPDIR}/index.json")"
test -n "${index_digest}"
test -n "${child_digest}"

status="$(curl -sS -o /dev/null -w "%{http_code}" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  -H "If-None-Match: \"${index_digest}\"" \
  "${BASE_URL}/v2/engine/manifests/${TAG}")"
if [[ "${status}" != "304" ]]; then
  echo "cached conditional manifest status was ${status}, want 304" >&2
  exit 1
fi

curl -fsS -D "${TMPDIR}/tags.headers" \
  -o "${TMPDIR}/tags.json" \
  "${BASE_URL}/v2/engine/tags/list?n=5"
jq -e '.name == "engine" and (.tags | length) == 5' "${TMPDIR}/tags.json" >/dev/null
grep -q "Link: </v2/engine/tags/list" "${TMPDIR}/tags.headers"

curl -fsS -D "${TMPDIR}/child.headers" \
  -H "Accept: application/vnd.oci.image.manifest.v1+json" \
  -o "${TMPDIR}/child.json" \
  "${BASE_URL}/v2/engine/manifests/${child_digest}"
layer_digest="$(jq -r '.layers[0].digest' "${TMPDIR}/child.json")"
test -n "${layer_digest}"

status="$(curl -sS -I -o "${TMPDIR}/blob-head.headers" -w "%{http_code}" \
  "${BASE_URL}/v2/engine/blobs/${layer_digest}")"
if [[ "${status}" != "200" ]]; then
  echo "blob HEAD status was ${status}, want 200" >&2
  exit 1
fi

status="$(curl -sS -D "${TMPDIR}/blob-get.headers" -o /dev/null -w "%{http_code}" --max-time 20 \
  "${BASE_URL}/v2/engine/blobs/${layer_digest}")"
if [[ "${status}" != "307" ]]; then
  echo "blob GET status was ${status}, want 307" >&2
  exit 1
fi
grep -q "Location: https://" "${TMPDIR}/blob-get.headers"

crane_digest="$("${CRANE[@]}" digest "${IMAGE}")"
if [[ "${crane_digest}" != "${index_digest}" ]]; then
  echo "crane digest was ${crane_digest}, want ${index_digest}" >&2
  exit 1
fi
"${CRANE[@]}" manifest "${IMAGE}" >/dev/null

echo "local proxy e2e passed for ${IMAGE}"
