#!/usr/bin/env bash
#
# Build the scarab images and load them into the k0s node's containerd.
#
# Pipeline: cross-compile (Go) -> buildah (WSL) -> docker-archive -> scp ->
# k0s ctr images import.
#
# Two images, both needed for a workspace to function:
#   scarab-agent    the root agent and its workers (Pi + bridge + platform tools)
#   scarab-broker   the per-workspace broker
#
# The registry is deferred, so the images are imported locally and the
# Deployments must reference the same tags with imagePullPolicy: IfNotPresent
# (which pestilence already sets).
#
# Requirements
#   - buildah inside WSL. Rootless is fine.
#   - an ssh alias for the node that works non-interactively.
#   - NOPASSWD for the k0s ctr image commands, because the k0s containerd socket
#     is root-only (srw-rw---- root:root). Add to /etc/sudoers.d/k0s-ctr-images
#     on the node:
#
#       $USER ALL=(root) NOPASSWD: /usr/local/bin/k0s ctr images import *, \
#                                   /usr/local/bin/k0s ctr images ls, \
#                                   /usr/local/bin/k0s ctr images rm *
#
# Usage
#   cluster-setup-scripts/load-image.sh                 # build + load both images
#   TAG=v2 cluster-setup-scripts/load-image.sh
#   SKIP_BUILD=1 cluster-setup-scripts/load-image.sh    # reuse existing archives

set -euo pipefail

TAG="${TAG:-dev}"
NODE="${NODE:-k0s-node}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# git-bash paths (/c/Users/...) are /mnt/c/Users/... inside WSL.
WSL_ROOT="$(printf '%s' "$REPO_ROOT" | sed 's|^/\([a-zA-Z]\)/|/mnt/\1/|')"

GO_BIN="${GO:-}"
if [ -z "$GO_BIN" ]; then
	if [ -x "$HOME/.local/go/bin/go" ]; then GO_BIN="$HOME/.local/go/bin/go"; else GO_BIN=go; fi
fi

mkdir -p "$REPO_ROOT/image/dist"

if [ "${SKIP_BUILD:-0}" != "1" ]; then
	echo "==> cross-compile linux/amd64 binaries"
	(cd "$REPO_ROOT" &&
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -trimpath \
			-o image/agent/scarab-bridge ./cmd/bridge &&
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -trimpath \
			-o image/broker/scarab-broker ./cmd/broker)
fi

# build_and_load NAME CONTEXT
build_and_load() {
	local name="$1" context="$2"
	local archive="$REPO_ROOT/image/dist/$name-$TAG.tar"
	local wsl_archive="$WSL_ROOT/image/dist/$name-$TAG.tar"

	if [ "${SKIP_BUILD:-0}" != "1" ]; then
		echo "==> buildah bud $name:$TAG"
		wsl.exe -e bash -lc "set -e
			cd '$WSL_ROOT/$context'
			# A CRLF shebang would not execute in the container.
			sed -i 's/\r\$//' scarab-agent 2>/dev/null || true
			buildah bud -t '$name:$TAG' -f Dockerfile .
			rm -f '$wsl_archive'
			buildah push '$name:$TAG' docker-archive:'$wsl_archive':'$name:$TAG'"
	fi

	echo "==> scp $name archive to $NODE:/tmp/"
	# scp runs on the host rather than inside the build environment: the build
	# environment has no ssh key, and copying a private key into it is unnecessary.
	scp -o BatchMode=yes "$archive" "$NODE:/tmp/$name-$TAG.tar"

	echo "==> k0s ctr images import $name:$TAG"
	ssh -o BatchMode=yes "$NODE" "sudo -n k0s ctr images import /tmp/$name-$TAG.tar"
}

build_and_load scarab-agent image/agent
build_and_load scarab-broker image/broker

echo "==> images on $NODE"
ssh -o BatchMode=yes "$NODE" "sudo -n k0s ctr images ls | grep scarab || true"

echo "==> loaded. Reference '$TAG' tags with imagePullPolicy: IfNotPresent."
# The tag does not change, so a running pod keeps the image it started with.
echo "==> NOTE: running pods keep the OLD image until restarted. To pick this up:"
echo "         kubectl -n <ns> delete pod -l agents.gobackto.work/workspace=<slug>"
echo "         kubectl -n ws-<slug> delete pod -l agents.gobackto.work/component=root-agent"
