# The scarab-agent image: Pi plus the worker entrypoint.
#
# One image, two roles. The root agent runs /usr/local/bin/scarab-bridge (added
# when the bridge lands); a worker runs /usr/local/bin/scarab-agent, which creates
# the result directory and then execs pi.
#
# Pod Security Admission `restricted` applies in both ws-<slug> and scarab, so the
# image must run as a non-root user and need no capabilities. The node image
# already provides a non-root `node` user at uid 1000.

FROM docker.io/library/node:24-bookworm-slim

# Pin Pi. The agent's behaviour is part of the platform contract, not a floating
# dependency: a Pi upgrade changes what the agent does.
ARG PI_VERSION=1.0.0

# PI_OFFLINE keeps the install from making a version-check request.
RUN PI_OFFLINE=1 npm install --global --omit=dev \
        "@earendil-works/pi-coding-agent@${PI_VERSION}" \
 && PI_OFFLINE=1 pi --version \
 && npm cache clean --force

COPY scarab-agent /usr/local/bin/scarab-agent
COPY scarab-bridge /usr/local/bin/scarab-bridge
RUN chmod 0755 /usr/local/bin/scarab-agent /usr/local/bin/scarab-bridge

# The root agent's platform tools. Loaded with `pi --extension` by the bridge,
# for the root agent only: workers get no orchestration capability.
#
# Read-only for non-root, so a process in the workspace cannot rewrite the
# extension it runs under.
COPY platform-tools /opt/scarab/platform-tools
RUN chown -R root:root /opt/scarab/platform-tools \
 && chmod 0755 /opt/scarab/platform-tools \
 && chmod 0644 /opt/scarab/platform-tools/*

# /workspace is a PVC mounted at runtime; /scratch and /tmp are emptyDirs.
WORKDIR /workspace

# Numeric, not `node`: with runAsNonRoot the kubelet must be able to verify the
# user is non-root, and it cannot resolve a named user from the image config.
# A named USER fails the pod with "image has non-numeric user (node)".
USER 1000:1000

# The root agent runs the bridge; a worker runs the entrypoint. Both are in this
# image so the root agent costs one pod and there is no internal RPC port.
ENTRYPOINT ["/usr/local/bin/scarab-agent"]
CMD ["--version"]
