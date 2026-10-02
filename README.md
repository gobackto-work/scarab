# scarab

**Tenant/agent side** of the self-hosted Pi agent platform: the agent broker, the
Pi agent container image, and the Pi `platform-tools` extension.

> **Starting work here? Read [`docs/architecture.md`](docs/architecture.md) first.**
> It is the single source of truth: the decisions already made, the
> contracts that are *enforced by the Kubernetes API server* (so violating them
> fails at runtime, not in review), and the pinned pestilence↔scarab interface
> (names, labels, ports, mount paths, env, tokens, and the PodSpec templates
> pestilence applies on scarab's behalf).
>
> **Before claiming anything works, run `bash hack/verify.sh`.** It is the shared
> gate — stateless, version-asserted, seconds to run. [`docs/verification.md`](docs/verification.md)
> says what each check is for and why LSP-based tooling is an authoring aid rather
> than the authority.

## Role

| Repo | Role |
|---|---|
| [`pestilence`](https://github.com/gobackto-work/pestilence) | **Trusted platform plane.** Workspace lifecycle, provisioner, authentication, audit. The only component with cross-namespace privilege. |
| [`town`](https://github.com/gobackto-work/town) | **Management UI.** The control plane's web interface: sign-in, workspace list/create/delete, inspection. Talks to pestilence's API. |
| **scarab** (this repo) | **Tenant/agent side.** Broker, Pi agent image, Pi extension, and the workspace UI. Namespace-scoped privilege only. |
| [`my-opps`](https://github.com/gobackto-work/my-opps) | Cluster build and cluster-level policy. |

## The security model in one line

> **The tenant namespace is the unit of compromise.**

Anything running inside a workspace may attack everything else inside it.
Compromise must not imply Kubernetes administrative access, host access, another
tenant, the platform control plane, home-LAN services, the ability to alter its
own quota or policy, or persistence after deletion.

Two consequences shape this repository completely:

1. **scarab is the only component that constructs a PodSpec.** pestilence never
   emits one, so no tenant-supplied `securityContext`, volume, ServiceAccount
   name, or node selector can reach the API server.
2. **The platform exposes intent APIs, not Kubernetes.** Pi calls
   `spawnAgent(...)`; it never sees a namespace, a pod, or a Role.

## Status

First slice in place and green:

- the merged interface contract — [`docs/architecture.md`](docs/architecture.md);
- the worker PodSpec builder — `internal/agentpod` — with its invariants as
  executable tests;
- the broker — `internal/broker` and `cmd/broker` — capability-token
  verification, the HTTP surface, and the Kubernetes-backed spawner;
- the bridge — `internal/bridge` and `cmd/bridge` — supervises `pi --mode rpc`
  and serves the workspace endpoint;
- the platform-tools extension — `image/agent/platform-tools` — the root agent's
  `agents_spawn`/`list`/`logs`/`stop` tools, talking to the broker;
- the scarab-agent and scarab-broker images — `image/agent`, `image/broker` — built
  and loaded by `hack/load-image.sh`;
- the shared boundary constants — `internal/contract`.

```bash
go build ./... && go vet ./... && go test ./...         # 133 tests
node --test image/agent/platform-tools/broker.test.js   # 13 tests
node --test internal/bridge/app.test.mjs                # 25 tests
```

The module pins `k8s.io/api v0.36.5` and `github.com/golang-jwt/jwt/v5`,
matching the cluster and pestilence.
pestilence emits the broker and root-agent Deployments and the Ingress from this
contract (`42c9d00`).

Not yet implemented: nothing in scarab. The remaining platform work is audit
logging in pestilence. The workspace endpoint already carries ForwardAuth and the
bridge's own signed-assertion gate.
