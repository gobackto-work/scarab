# The per-workspace broker: a static Go binary and nothing else.
#
# It holds the only Kubernetes credential on the tenant side, plus a
# namespace-scoped Role in exactly one tenant namespace, so the image is kept
# minimal deliberately: distroless (no shell, no package manager), non-root.
#
# Pod Security Admission `restricted` applies in the scarab namespace, so the
# image must run as a non-root user and need no capabilities. The PodSpec sets
# the RuntimeDefault seccomp profile and drops ALL capabilities.

# Digest-pinned: a tag can be moved, a digest cannot. Shared with pestilence's image.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Cross-compiled by hack/load-image.sh into this build context.
COPY scarab-broker /scarab-broker

# Numeric, not `nonroot`: with runAsNonRoot the kubelet must be able to verify
# the user is non-root, and it cannot resolve a named user from the image
# config. 65532 is distroless's nonroot uid.
USER 65532:65532
ENTRYPOINT ["/scarab-broker"]
