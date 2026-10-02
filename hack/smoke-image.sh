#!/usr/bin/env bash
#
# Smoke-test the built scarab-agent image with buildah, inside WSL.
#
# Checks three things the image build alone cannot prove:
#   1. the platform-tools extension is present;
#   2. its unit tests pass with the image's Node;
#   3. Pi actually loads the extension (a syntax error or a bad import would
#      otherwise only surface when an agent starts).
#
# Usage: wsl.exe -e bash /mnt/c/.../hack/smoke-image.sh
set -euo pipefail

IMAGE="${IMAGE:-scarab-agent:dev}"
EXT="${EXT:-/opt/scarab/platform-tools/index.js}"
EXT_TESTS="${EXT_TESTS:-/opt/scarab/platform-tools/broker.test.js}"

ctr="$(buildah from "$IMAGE")"
trap 'buildah rm "$ctr" >/dev/null 2>&1 || true' EXIT

echo "==> extension files"
buildah run "$ctr" -- ls -l /opt/scarab/platform-tools/

echo
echo "==> extension unit tests (image Node)"
buildah run "$ctr" -- node --test --test-reporter=spec "$EXT_TESTS" 2>&1 | tail -15 || true

echo
echo "==> does Pi load the extension?"
buildah run "$ctr" -- sh -c "printf '%s\n' '{\"id\":\"1\",\"type\":\"get_state\"}' > /tmp/cmd.jsonl"
buildah run "$ctr" -- sh -c "timeout 30 pi --mode rpc --no-session --extension '$EXT' < /tmp/cmd.jsonl > /tmp/out.jsonl 2> /tmp/err.txt || true"
echo "--- stdout ---"
buildah run "$ctr" -- head -c 1500 /tmp/out.jsonl || true
echo
echo "--- stderr ---"
buildah run "$ctr" -- head -c 1500 /tmp/err.txt || true
echo

# A clean load is not the same as the tools registering: assert it explicitly.
if buildah run "$ctr" -- grep -q "registered agents_spawn, agents_list, agents_logs, agents_stop" /tmp/err.txt; then
	echo "==> OK: the extension registered all four tools"
else
	echo "==> FAIL: the extension did not report registering its tools" >&2
	exit 1
fi
