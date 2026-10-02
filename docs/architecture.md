# scarab — architecture and interface contract

This is the starting point for work in this repository. It does three things:

1. records the decisions already made, so they don't get re-litigated;
2. states the contracts that are **enforced by the Kubernetes API server**, so
   violating them fails at runtime rather than in review;
3. pins the **pestilence ↔ scarab interface** — every name, label, port, mount
   path, environment variable and token claim that crosses the boundary, plus
   the PodSpec templates pestilence applies on scarab's behalf.

scarab owns the *artifacts* (broker, agent image, bridge, PodSpec templates).
pestilence owns the *privilege* and applies them. The counterpart code is
`pestilence/internal/tenant` (`bundle.go`, `class.go`, `spec.go`) and
`pestilence/internal/provisioner` (`reconciler.go`).

**Status markers.** **[D]** decided; **[P]** proposed by scarab, awaiting the
counterpart's acknowledgement or counter; **[O]** open, blocking implementation
or live validation (collected in §8). Section numbers are the shared reference
vocabulary for both repositories, and are deliberately unchanged from the
original document: §11, §12, §16 and §17 were internal working notes (red-team
targets, first-slice plan, defect log, contract-enforcement proposal) and are not
published here.

---

## 1. What scarab is

scarab is the **tenant/agent side** of the platform:

- the **agent broker** — the only privileged interface a Pi agent has
- the **Pi agent container image** (root and worker)
- the **Pi `platform-tools` extension** that lets Pi call the broker
- the **bridge** and minimal workspace UI served at the workspace endpoint

It is **semi-trusted**. It holds namespace-scoped privilege only, never
cluster-admin.

### Repository roles

| Repo | Role |
|---|---|
| [`pestilence`](https://github.com/gobackto-work/pestilence) | **Trusted platform plane.** Workspace lifecycle, provisioner, authentication, audit. The only component with cross-namespace privilege. |
| [`town`](https://github.com/gobackto-work/town) | **Management UI.** The control plane's web interface — sign-in, workspace list/create/delete, inspection. Talks to pestilence's API. |
| **scarab** (this repo) | **Tenant/agent side.** Broker, Pi agent image, Pi extension, bridge and workspace UI. Namespace-scoped privilege only. |
| [`my-opps`](https://github.com/gobackto-work/my-opps) | Cluster build and cluster-level policy. |

### The boundary that matters most

An earlier draft said *"scarab is the only component that constructs a PodSpec.
pestilence never emits one."* After the §8.1 decision that pestilence applies the
broker and root-agent Deployments, that is no longer literally true, and leaving
it stated that way is how a reviewer approves the wrong thing. The precise rule
is **[P]**:

| PodSpec | Constructor | Inputs | Class (§4.6) |
|---|---|---|---|
| `broker-<slug>` Deployment | pestilence, from a scarab-authored template | the workspace slug only | platform (§5.5) |
| root agent Deployment | pestilence, from a scarab-authored template | the workspace slug + secret refs | runtime |
| worker Job | **the broker, at runtime** | task string, model profile, resource hints within bounds, timeout | n/a |

**[D]** The invariant that actually matters is unchanged: **no tenant-supplied
field ever reaches a PodSpec.** It is enforced independently of who builds the
PodSpec, by Pod Security Admission and the `ValidatingAdmissionPolicy` objects in
`my-opps`, so it holds even if the broker is bypassed entirely.

---

## 2. The security model

> **The tenant namespace is the unit of compromise.**

Anything running inside a workspace may attack everything else inside it.
Compromise must **not** imply Kubernetes administrative access, host access,
another tenant, the platform control plane, home-LAN services, the ability to
alter its own quota/policy/authorization, or persistence after deletion.

The target invariant: **no Kubernetes credential ever enters a Pi container.**

The central principle: **intent APIs, not Kubernetes.** Pi decides *what to
accomplish*; the trusted platform decides *what Kubernetes resources are
permissible*.

---

## 3. Decisions already made — do not re-litigate

| Decision | Value |
|---|---|
| Broker topology | **B** — one broker per workspace, running in the `scarab` namespace, with a `RoleBinding` into the tenant namespace |
| Why B | ServiceAccount references are namespace-local, so a tenant **cannot reference the broker's identity at all**, even with pod-create rights |
| CNI | Cilium 1.20.2 (`kubeProxyReplacement=false`, `cni.chainingMode=portmap`) |
| MVP egress model | deny-by-default; **"internet yes, LAN no"** (`0.0.0.0/0` minus RFC1918, loopback, link-local, CGNAT) |
| Concurrency | **3 workspaces** max on the single 6-CPU node |
| Storage | `openebs-hostpath`, node-local, **RWO**, no expansion |
| Ingress | Traefik via `hostPort` 80/443; wildcard `*.gobackto.work` TLS served as Traefik's default certificate |
| Worker execution | fire-and-forget Job, **no `pods/exec`** |
| Recursive delegation | worker → root → broker. Workers get no orchestration capability by default |
| CRDs | none in the MVP |
| Broker Deployment | **pestilence applies it** (§8.1) |
| Workspace endpoint | `https://<slug>.gobackto.work` serves the **root agent**; scarab's bridge is the default landing page (§8.2) |
| Reconciliation classes | **boundary + endpoint reconciled; runtime created once and tenant-mutable** (§4.6) |
| Ingress ports | HTTP/HTTPS only. Traefik 80/443 → container port 8000. No port ranges |
| `scarab` namespace | Declared in `my-opps/cluster/manifests/namespace-scarab.yaml`; PSA `restricted`; label `agents.gobackto.work/component: broker-platform` |
| PodSpec boundary | No tenant input reaches any PodSpec; pestilence emits the broker and root templates, the broker emits worker templates (§1) |
| Broker transport | plaintext HTTP on 8443 today; **TLS is proposed and open** (§8.5) |
| Broker Deployment class | `ClassPlatform`, reconciled (§8.9) |
| Object class annotation | `agents.gobackto.work/reconcile: <class>` on every object (§5.2) |
| Worker tokens | none; recursive delegation goes through the root agent (§8.3) |
| Token lifetime | **24h, rotated at half** by pestilence; the bridge re-reads it rather than caching (§8.7) |
| Images | two — `scarab-broker`, `scarab-agent`; GHCR planned (§8.6) |
| Endpoint auth | **signed assertion from the edge** (proposed, §8.8); `X-Auth-User` is ignored |
| Model credentials | supplied by the user in the session and persisted to the workspace volume in Pi's `auth.json`; shared with workers; the platform never holds one (§8.4) |

---

## 4. Hard contracts — enforced by the API server

These are **not** style preferences. Each one is enforced by Pod Security
Admission or by a `ValidatingAdmissionPolicy`, and a violation is rejected at
admission time. Verified live against the cluster.

### 4.1 Pod and workload-template invariants (ValidatingAdmissionPolicy)

Applied to every namespace labelled `agents.gobackto.work/workspace`, and to
`pods`, `pods/ephemeralcontainers`, and the pod templates of `jobs`, `cronjobs`,
`deployments`, `statefulsets`, `daemonsets`, `replicasets`.

| Rule | Consequence if violated |
|---|---|
| `serviceAccountName` **must** be `pi-root` or `pi-worker` | **Denied.** Omitting it is denied too — the `default` ServiceAccount auto-mounts an API token, which is exactly what the platform forbids |
| `automountServiceAccountToken` must not be `true` | Denied |
| `spec.nodeName` must not be set | Denied — it bypasses the scheduler entirely. On **CREATE**, or on UPDATE when it *changes*: the scheduler sets `nodeName` on every pod that runs, so denying it on every update also denies the Job controller's cleanup (see below) |
| `spec.runtimeClassName` must not be set | Denied |
| Volumes restricted to `persistentVolumeClaim`, `emptyDir`, `secret`, `configMap`, `projected`, `downwardAPI`, `ephemeral` | Denied |
| `pods/ephemeralcontainers` (i.e. `kubectl debug`) | **Denied outright** |

> **This is the single most likely thing to trip you up.** Every pod *and* every
> job template the broker creates must set `serviceAccountName` explicitly. A
> template that omits it will be rejected, not defaulted.

Note the asymmetry **[P]**: the workload-template policy checks
`serviceAccountName`, `automountServiceAccountToken`, `nodeName` and
`runtimeClassName`, but **not volumes** — only the `pods` policy checks those. A
Job with a `hostPath` volume is therefore admitted as a template and rejected
later, at pod creation, with a confusing error. Keeping broker-built templates
inside the volume allowlist is what avoids that late failure.

**[D] The `nodeName` rule must be CREATE-only, or change-aware.** `spec.nodeName`
is set by the *scheduler* on every pod that runs, so a bare
`!has(object.spec.nodeName)` applied to `UPDATE` denies every update to a
scheduled pod. That silently blocked the Job controller's orphan-pod cleanup and
the `batch.kubernetes.io/job-tracking` finalizer: a failed or stopped worker
stuck in `Terminating` forever and leaked the 6-pod budget. The rule is now
`!has(nodeName)` on CREATE and "unchanged" on UPDATE. Found live — the workspace
had two wedged worker pods and the kubelet log showed
`Error syncing orphan pod: … denied request: tenant pods must not pin a node via
spec.nodeName`.

### 4.2 Pod Security Admission `restricted`

Applies to all containers, including the root agent:

```
runAsNonRoot: true
allowPrivilegeEscalation: false
capabilities: { drop: ["ALL"] }
seccompProfile: { type: RuntimeDefault }
```

No privileged containers, `hostNetwork`, `hostPID`, `hostIPC`, `hostPath`, or
`hostPort`.

**It also applies to the `scarab` namespace itself**, which is declared in
`my-opps` with the same `restricted` labels. That is a contract on the broker
image: it must run as a non-root user, must not need added capabilities, and must
set a RuntimeDefault seccomp profile. Otherwise the broker Deployment pestilence
creates will be rejected at admission.

**[D] `runAsNonRoot: true` is not sufficient on its own.** The kubelet must be
able to *verify* the user is non-root, and it cannot resolve a **named** user from
the image config. A named `USER` fails the pod with
`image has non-numeric user (node), cannot verify user is non-root`.

So each workload pins a **numeric** `runAsUser`, and each image declares a
**numeric** `USER`:

| Workload | `runAsUser` | image `USER` |
|---|---|---|
| root agent | 1000 | `1000:1000` |
| worker | 1000 | `1000:1000` |
| broker | 65532 | `65532:65532` (distroless nonroot) |

Found by the first live workspace: both pods failed to start until this was fixed.
Pinning the number in the PodSpec is what makes it independent of the image.

### 4.3 Resource envelope

Per-workspace `ResourceQuota`:

| Resource | Limit |
|---|---|
| `requests.cpu` / `limits.cpu` | `500m` / `2` |
| `requests.memory` / `limits.memory` | `1Gi` / `3Gi` |
| `pods` | **6** |
| `requests.storage` | `20Gi` |
| `persistentvolumeclaims` | 2 |
| `services` | 3 |
| `count/jobs.batch` | 20 |

`LimitRange`: container defaults `300m`/`512Mi`, requests `100m`/`256Mi`,
**maximum per container `1` CPU / `2Gi`**.

> **The pod count is not the binding constraint — the memory requests are.** The
> broker runs in `scarab`, so it is **not** charged against this quota: the six
> pods are the root agent plus up to five workers. But with the §5.6/§5.7 template
> sizes, after the root agent (100m/256Mi requests, 500m/1Gi limits) the remaining
> quota admits only **three** more workers on `requests.memory` (768Mi ÷ 256Mi),
> four on `requests.cpu`, four on `limits.memory`, and five on `limits.cpu`. The
> real concurrent-worker ceiling is therefore **three**, and the broker must
> account for the resource dimensions, not just the pod count. Either raise the
> quota, lower per-agent requests, or accept three. Surface quota rejections as
> ordinary errors so the agent can recover — e.g.
> `spawnAgent failed: workspace memory quota reached`.

### 4.4 The broker's own RBAC

`Role/broker` in `ws-<slug>` grants exactly:

```
pods        get, list, watch, create, delete
pods/log    get
jobs        get, list, watch, create, delete   (batch)
```

It deliberately grants **nothing** on `resourcequotas`, `limitranges`,
`networkpolicies`, `serviceaccounts`, `roles`, `rolebindings`, `secrets`,
`services`, `configmaps`, or `namespaces`. That omission is how "a tenant cannot
alter its own security policy" is enforced at the RBAC layer rather than by
convention. **[P]** The broker must be implementable within exactly these verbs:

- it **cannot read its own quota**, so it enforces the pod budget and container
  ceilings from its own configuration (§6.4), and translates an API
  `Forbidden`/`exceeded quota` error into a typed agent error (§6.5);
- it **cannot read Secrets**, so anything it needs at runtime arrives via env or
  a mounted volume;
- it observes completion through `jobs` `get`/`watch` and `pods/log` `get`,
  never `pods/exec`.

The `RoleBinding` subject is `ServiceAccount <slug>-broker` in namespace
`scarab`. pestilence creates that ServiceAccount as well, together with the
broker `Deployment` and `Service` — see §5.5 and §8.1.

### 4.5 Network

Deny-by-default. The permitted flows are:

| Policy | Flow |
|---|---|
| `allow-intra-namespace` | pod ↔ pod within the workspace, all ports and protocols |
| `allow-dns` | → `kube-system`/`kube-dns`, port 53 UDP+TCP |
| `allow-broker-egress` | → namespace `scarab`, pods labelled `agents.gobackto.work/workspace: <slug>`, **TCP 8443** |
| `allow-ingress-to-workspace` | from namespace `traefik` → **TCP 8000** |
| `allow-internet-egress` | `0.0.0.0/0` minus private ranges |

Two consequences for scarab:

1. The **broker pod must carry the label
   `agents.gobackto.work/workspace: <slug>`** in the `scarab` namespace, or the
   tenant cannot reach it at all — a single missing label fails silently, with
   nothing rejected at admission time (§5.2).
2. The broker must listen on **8443** and the root agent's HTTP endpoint on
   **8000** (both configurable via the provisioner's `Spec`, but these are the
   defaults).

Note that `10.0.0.0/8` is excluded from internet egress, which also removes the
cluster service CIDR. That is intended: the tenant has no Kubernetes credential
and no business reaching cluster services. The API server is reached only by the
broker, from the `scarab` namespace.

### 4.6 Boundary, endpoint, runtime — what is reconciled and what is not

The provisioner is idempotent, but **not everything it creates should be
reconciled**. Three classes (`pestilence/internal/tenant/class.go`):

| Class | Objects | Behaviour |
|---|---|---|
| **Boundary** | namespace, PSA labels, ResourceQuota, LimitRange, NetworkPolicies, ServiceAccounts, Role/RoleBinding, PVCs, broker ServiceAccount | **Continuously reconciled.** Drift is corrected. A tenant must never be able to alter these |
| **Endpoint** | `Ingress/<slug>`, `Service/root-pi`, `Service/broker-<slug>` | **Reconciled.** The platform guarantees the workspace has a reachable HTTPS endpoint and a reachable broker |
| **Runtime** | the root agent Deployment (`root-agent`) | **Created once at provisioning, then owned by the tenant. Never reconciled back** |
| **Platform** | `Deployment/broker-<slug>` (in `scarab`) | **Reconciled.** Platform infrastructure outside the tenant namespace; the tenant cannot write it, so it must be healed |

Rule of thumb: **the provisioner guarantees the workspace is reachable and
confined; it does not guarantee what the tenant runs inside it.**

So a tenant may delete or replace the default landing page and it *stays*
deleted — the endpoint remains (returning 502 until something serves it) and the
boundary is untouched. But deleting the Ingress or the Service gets them restored,
because that is a platform guarantee rather than a tenant choice.

Consequence for scarab: if a tenant serves its own thing, it must keep
`Service/root-pi` pointing at whatever answers. The Service name, selector and
port are a **contract**, not an implementation detail.

**[D]** The broker Deployment is **`ClassPlatform`** — a fourth class, added in
pestilence for platform infrastructure outside the tenant namespace, reconciled
like boundary and endpoint. It is not `ClassRuntime`: the tenant cannot write
`scarab`, so `createIfAbsent` would mean a deleted `Deployment/broker-<slug>` is
never recreated, and the workspace would be permanently and silently unable to
spawn agents while every other health signal stayed green (§5.5, §8.9).

### 4.7 Ports

The public surface is HTTP/HTTPS only: Traefik listens on 80/443 and routes to
`Service/root-pi` on container port **8000**. There are no port ranges and no
pre-allocated pool.

Internally, `allow-intra-namespace` permits pod-to-pod traffic on **all** ports
and protocols, so workspace-internal agents can run whatever they like on
internal ports — a web app, a database, a queue — for sandboxed work that needs
to look like real internal comms. Only the ingress-facing port is fixed.

---

## 5. What pestilence creates, and the interface vocabulary

### 5.1 Names and identifiers

**[D]** All names derive from the slug.

| Thing | Value | Defined by |
|---|---|---|
| slug | DNS-1123 label, `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, ≤ 40 chars | `tenant.ValidateSlug` |
| tenant namespace | `ws-<slug>` | `tenant.Namespace` |
| public host | `<slug>.gobackto.work` | `Spec.Hostname` |
| broker ServiceAccount | `<slug>-broker` in `scarab` | `Spec.BrokerServiceAccountName` |
| broker Deployment | `broker-<slug>` in `scarab` | §5.5 |
| broker Service | `broker-<slug>` in `scarab` | `Spec.BrokerServiceName` |
| root Service | `root-pi` in `ws-<slug>` | `tenant.NameRootService` |
| root Deployment | `root-agent` in `ws-<slug>` | §5.6 |
| worker Job | `worker-<agent-id>` in `ws-<slug>` | §5.7 |
| root ServiceAccount | `pi-root` in `ws-<slug>` | `tenant.SARoot` |
| worker ServiceAccount | `pi-worker` in `ws-<slug>` | `tenant.SAWorker` |

**[P]** `agent-id` is generated by the broker: lowercase alphanumerics and `-`,
8–26 characters, unique within the workspace. It is opaque to the tenant.
`worker-<agent-id>` must stay ≤ 63 characters.

**[D]** `<slug>.gobackto.work` is **not** authentication. Unguessability is
defence in depth only. The ingress auth gate (§6.2, §8.2) is the gate.

### 5.2 Labels and annotations

**[D]** Two labels are load-bearing, because NetworkPolicy and Service selectors
match on them. Both must be present on every pod in `ws-<slug>`, and `workspace`
must also be present on the broker pod in `scarab`.

| Key | Value | Who sets it |
|---|---|---|
| `agents.gobackto.work/workspace` | the slug | pestilence (boundary objects); broker (worker pods/jobs); root and broker templates |
| `agents.gobackto.work/component` | `root-agent` \| `worker` \| `broker` | as above |
| `app.kubernetes.io/managed-by` | `pestilence` for provisioner objects, `scarab` for broker-created pods/jobs | both |

`Service/root-pi` selects on **both** labels, so a root agent pod without them
will not receive traffic. Likewise `Service/broker-<slug>` selects
`workspace=<slug>, component=broker`.

**[P]** If the broker pod template omits
`agents.gobackto.work/workspace: <slug>`, the tenant's `allow-broker-egress`
policy does not match it and **the tenant silently cannot reach its broker at
all**. There is no admission-time error for a missing label.

**[P]** Worker pods and Jobs must also carry both labels. A worker Job whose pod
lacks `component: worker` is still admitted (the VAP does not check labels), so
this is a convention the broker's tests must hold, not something the API server
catches.

**[D]** Every object pestilence applies carries the class as the annotation
`agents.gobackto.work/reconcile: "boundary" | "endpoint" | "runtime" |
"platform"`, so the class is observable on the cluster and not only in Go.

### 5.3 Ports and addresses

**[D]** Fixed ports.

| Flow | Port | Protocol | Endpoint |
|---|---|---|---|
| Traefik → root agent | **8000** | TCP/HTTP | `Service/root-pi`, container port 8000 |
| root agent → broker | **8443** | TCP | `broker-<slug>.scarab.svc.cluster.local:8443` |
| pod ↔ pod in workspace | any | any | permitted by `allow-intra-namespace` |

**[D]** The public surface is HTTP/HTTPS only. Traefik terminates TLS on 443;
everything inside the cluster is plaintext unless §8.5 decides otherwise.
`Spec.HTTPPort` must be 8000 for any workspace that has an endpoint.

**[P]** `Spec.HTTPPort` of 0 disables `Service/root-pi` **and** produces an
`allow-ingress-to-workspace` policy with port 0, which is not a valid port.
Either `HTTPPort` is always 8000, or the ingress policy is emitted
conditionally. `bundle.go` currently does the former for the Service but not for
the policy.

**[P]** The root agent must serve `GET /healthz` on 8000, returning 200 when the
bridge is ready to accept a session. The root Deployment's readiness probe uses
it, so the Service does not route to a dead bridge.

### 5.4 Objects per workspace

Per workspace, pestilence creates (see `pestilence/internal/tenant/bundle.go`):

- `Namespace ws-<slug>` with PSA `restricted` labels
- `ResourceQuota/workspace`, `LimitRange/workspace`
- six `NetworkPolicy` objects
- `ServiceAccount/pi-root`, `ServiceAccount/pi-worker`, both with
  `automountServiceAccountToken: false`
- `Role/broker` + `RoleBinding/broker` (granting to `<slug>-broker` in `scarab`)
- `PersistentVolumeClaim/workspace` (20Gi) and optionally `/memory` (5Gi)
- `Service/root-pi` — selector
  `agents.gobackto.work/workspace=<slug>, agents.gobackto.work/component=root-agent`,
  port 8000
- in the **`scarab` namespace**: `ServiceAccount/<slug>-broker`,
  `Service/broker-<slug>`, and `Deployment/broker-<slug>` (§5.5)
- the **endpoint** (reconciled): the `Ingress` for `<slug>.gobackto.work`
- the **initial runtime** (created once, then tenant-owned): the root agent
  Deployment, running scarab's bridge and serving the default landing page

scarab runs **no controller and watches nothing**. It does not create or manage
Kubernetes objects in any namespace; at runtime the broker writes only pods and
jobs, within its Role.

### 5.5 The broker (platform namespace)

**[D]** `Deployment/broker-<slug>` in `scarab`, class **platform** (reconciled;
§4.6, §8.9). One broker per workspace, so it is inherently scoped to one tenant.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: broker-<slug>
  namespace: scarab
  labels:
    agents.gobackto.work/workspace: <slug>      # REQUIRED (§5.2)
    agents.gobackto.work/component: broker
spec:
  replicas: 1
  strategy: { type: Recreate }
  selector:
    matchLabels:
      agents.gobackto.work/workspace: <slug>
      agents.gobackto.work/component: broker
  template:
    metadata:
      labels:
        agents.gobackto.work/workspace: <slug>
        agents.gobackto.work/component: broker
    spec:
      serviceAccountName: <slug>-broker
      containers:
        - name: broker
          image: <§8.6>
          ports: [{ name: broker, containerPort: 8443 }]
          env: <§6.4>
          volumeMounts:
            # The STANDARD service-account path. The broker builds its client with
            # rest.InClusterConfig(), which reads <mount>/token and <mount>/ca.crt;
            # a custom mount path leaves the broker unable to reach the API server.
            - { name: api-token,    mountPath: /var/run/secrets/kubernetes.io/serviceaccount, readOnly: true }
            - { name: token-pubkey, mountPath: /var/run/scarab/token-pubkey, readOnly: true }
          securityContext: <§4.2>
      volumes:
        - name: api-token
          projected:
            sources:
              - serviceAccountToken:
                  path: token
                  expirationSeconds: 3600
              - configMap:                       # the cluster CA, so TLS verifies
                  name: kube-root-ca.crt
                  items: [{ key: ca.crt, path: ca.crt }]
              - downwardAPI:
                  items: [{ path: namespace, fieldRef: { fieldPath: metadata.namespace } }]
        - name: token-pubkey
          configMap: { name: broker-<slug>-token-pubkey }
```

**[D] The broker must hold a Kubernetes API credential** — that is the entire
point of the design; it is the only tenant-side component that talks to the API
server. The broker ServiceAccount keeps `automountServiceAccountToken: false`,
and the Deployment mounts a **projected `serviceAccountToken` volume** with a
bounded `expirationSeconds`, as above. (Implemented in pestilence: 3600s, mounted
at `/var/run/secrets/scarab`.)

**[D] The broker also needs the token *verification* public key**, which is a
different thing from its own API token. pestilence mints a **per-workspace
Ed25519 keypair** (§6.2), keeps the private key in the control plane, and
publishes the public key as `ConfigMap/broker-<slug>-token-pubkey` in `scarab`
(class boundary). The broker mounts it read-only and reads
`SCARAB_TOKEN_PUBLIC_KEY` (§6.4). A public key is not secret, so a ConfigMap is
correct; the private key never leaves pestilence.

**[D] The API token must be mounted at the standard service-account path.** The
broker builds its Kubernetes client with `rest.InClusterConfig()`, which reads
`<mount>/token` and `<mount>/ca.crt`. An earlier revision of this contract mounted
the projected token at `/var/run/secrets/scarab`, which is why the first live
workspace came up with a broker that could not reach the API server:
`in-cluster config: open /var/run/secrets/kubernetes.io/serviceaccount/token: no such file or directory`.
The mount is now the standard path, and the projected volume also supplies the
cluster CA (`kube-root-ca.crt`) and the namespace.

**[P]** `Service/broker-<slug>` selects `workspace=<slug>, component=broker` and
exposes port 8443 → 8443. The broker container port, the Service port and
`Spec.BrokerPort` must stay equal (8443).

**[P]** The broker must validate that the token it received names *its own*
workspace (§6.2). One broker per workspace makes this nearly redundant, but it is
the highest-value red-team target (§11.1) and costs one comparison.

**[D]** The `scarab` namespace must not carry
`agents.gobackto.work/workspace`. If it did, the tenant VAP would apply to the
broker and reject it: the broker's ServiceAccount name is not in
`{pi-root, pi-worker}`, and the broker legitimately needs an API token that
`tenant-pod-invariants` forbids. The broker is platform infrastructure, not a
tenant pod. This is settled by
`my-opps/cluster/manifests/namespace-scarab.yaml`, which labels the namespace
`agents.gobackto.work/component: broker-platform` instead.

### 5.6 The root agent runtime (tenant namespace)

**[P]** pestilence creates `Deployment/root-agent` in `ws-<slug>` once, then never
reconciles it (`ClassRuntime`). It is the default landing page and the tenant may
replace it. This is the shape `pestilence/internal/tenant/bundle.go` is currently
waiting on.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: root-agent
  namespace: ws-<slug>
  labels:
    agents.gobackto.work/workspace: <slug>
    agents.gobackto.work/component: root-agent
spec:
  replicas: 1
  strategy: { type: Recreate }          # RWO volume, see below
  selector:
    matchLabels:
      agents.gobackto.work/workspace: <slug>
      agents.gobackto.work/component: root-agent
  template:
    metadata:
      labels:
        agents.gobackto.work/workspace: <slug>
        agents.gobackto.work/component: root-agent
    spec:
      serviceAccountName: pi-root       # REQUIRED by VAP; no token
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 30
      containers:
        - name: bridge
          image: <§8.6>
          command: ["/usr/local/bin/scarab-bridge"]
          args: ["--port", "8000", "--session-dir", "/workspace/.pi/sessions"]
          ports: [{ name: http, containerPort: 8000 }]
          env: <§6.4>
          envFrom: none — the model key is in the shared agent directory, not a Secret (§8.4)
          readinessProbe:
            httpGet: { path: /healthz, port: 8000 }
          livenessProbe:
            httpGet: { path: /healthz, port: 8000 }
          resources:
            requests: { cpu: 100m, memory: 256Mi }
            limits:   { cpu: 500m, memory: 1Gi }
          volumeMounts:
            - { name: workspace, mountPath: /workspace }
            - { name: memory,    mountPath: /memory }        # only if WithMemoryPVC
            - { name: scratch,   mountPath: /scratch }
            - { name: tmp,       mountPath: /tmp }
            - { name: cap-token, mountPath: /var/run/scarab/token, readOnly: true }
          securityContext: <§4.2>
      volumes:
        - name: workspace
          persistentVolumeClaim: { claimName: workspace }
        - name: memory
          persistentVolumeClaim: { claimName: memory }       # only if WithMemoryPVC
        - name: scratch
          emptyDir: { sizeLimit: 1Gi }
        - name: tmp
          emptyDir: { sizeLimit: 256Mi }
        - name: cap-token
          # 0400 plus fsGroup, not 0444: the kubelet leaves Secret files owned by
          # root, so a non-root container cannot read them unless the pod has an
          # fsGroup. The pod sets one (pestilence, finding 5 of red-team review 1),
          # which is what makes 0400 work. The 0444 this replaced was a workaround
          # for the missing fsGroup, and the missing fsGroup was the root cause.
          secret: { secretName: pi-root-token, defaultMode: 0400 }
```

**[P]** `strategy.type` must be `Recreate`. `openebs-hostpath` is **RWO**; the
default `RollingUpdate` would start a second pod that cannot attach the volume
while the first holds it, and the rollout would hang.

**[D]** One container. The bridge spawns `pi --mode rpc` as a child, so the root
agent is one pod (leaving the rest of the budget for workers) and there is no
internal RPC port to secure. The bridge is the reusable asset; the frontend is
replaceable.

### 5.7 The worker Job (broker-constructed at runtime)

**[P]** The broker creates `Job/worker-<agent-id>` in `ws-<slug>`.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: worker-<agent-id>
  namespace: ws-<slug>
  annotations:
    agents.gobackto.work/task: "<task>"      # so listAgents can report it
  labels:
    agents.gobackto.work/workspace: <slug>
    agents.gobackto.work/component: worker
spec:
  backoffLimit: 0                     # fire-and-forget
  activeDeadlineSeconds: <timeoutSeconds>   # minimum 60, default 1800
  ttlSecondsAfterFinished: 300        # keep the 20-job budget from filling
  template:
    spec:
      serviceAccountName: pi-worker   # REQUIRED by VAP
      automountServiceAccountToken: false
      restartPolicy: Never
      containers:
        - name: worker
          image: <§8.6>
          command: ["/usr/local/bin/scarab-agent", "-p", "--", "<task>"]  # entrypoint; §5.9
          envFrom:
            - secretRef: { name: <§8.4> }        # optional; workers read the persisted store (§8.4)
          env:
            - { name: SCARAB_AGENT_ID,      value: "<agent-id>" }
            - { name: SCARAB_RESULT_PATH,   value: "/workspace/.agents/<agent-id>/result.md" }
            - { name: PI_SESSION_DIR,       value: "/scratch/pi" }
            - { name: PI_CODING_AGENT_DIR,  value: "/workspace/.pi/agent" }
          resources:
            requests: { cpu: 100m, memory: 256Mi }
            limits:   { cpu: 300m, memory: 512Mi }
          volumeMounts:
            - { name: workspace, mountPath: /workspace }
            - { name: scratch,   mountPath: /scratch }
          securityContext: <§4.2>
      volumes:
        - name: workspace
          persistentVolumeClaim: { claimName: workspace }
        - name: scratch
          emptyDir: { sizeLimit: 512Mi }
```

**[P]** `backoffLimit: 0`. A retried worker silently consumes the pod budget and
may duplicate side effects on the shared volume. Failure is reported, not
retried.

**[P]** Resource values must stay within the `LimitRange` maximum (1 CPU / 2Gi per
container) **and** within the workspace quota. The root agent (500m/1Gi limits,
100m/256Mi requests) leaves 1.5 CPU / 2Gi on limits — four workers at
300m/512Mi — but only 400m CPU / 768Mi on requests, which is **three** workers at
100m/256Mi. Memory requests are the binding dimension, so the practical ceiling
is three concurrent workers, not the five the pod count allows. These values are
a starting point and must be validated against the live quota (§4.3).

**[P]** The task string is passed as an argv element after `--`, never
interpolated into a shell. The worker container has no shell in its `command`.
This removes an injection surface and stops a task beginning with `-` being
parsed as a flag.

**[D]** Workers receive **no** broker token (§8.3). Recursive delegation goes
through the root agent.

### 5.8 Storage and filesystem layout

**[D]** Mount points are a contract, because the root agent and workers share
them.

| Mount | Backing | Lifetime | Notes |
|---|---|---|---|
| `/workspace` | PVC `workspace` | workspace | shared by all agents |
| `/memory` | PVC `memory` (optional) | workspace | agent-managed extended memory |
| `/scratch` | `emptyDir` + `sizeLimit` | pod | worker sessions, temp |
| `/tmp` | `emptyDir` | pod | required if the root filesystem is read-only |
| `/workspace/.pi/agent` | subdirectory of PVC `workspace` | workspace | Pi's agent directory; `auth.json` holds the user-supplied model credential and is shared with workers (§8.4) |
| `/var/run/scarab/token` | Secret (root only) | pod | capability token, read-only |

**[P]** The capability token must be a **mounted Secret**, never a file on
`/workspace`. `/workspace` is shared with every worker, so a token on the PVC is
readable by every worker — and by anything the tenant runs.

> **RWO is a single-node assumption.** `openebs-hostpath` is node-local, so every
> pod in a workspace can mount `/workspace` **only because there is one node**.
> Adding a second node breaks this — the volume cannot follow a pod. Adding nodes
> scales *new* workspaces; it does not relieve an existing one.

### 5.9 Result handoff

**[D]** Workers write `/workspace/.agents/<agent-id>/result.md`. The root agent
reads it. The broker reports completion. Logs are read via `pods/log`.

**[D]** The **worker image** creates the `.agents/<agent-id>` directory, not the
broker. The broker runs in `scarab` and does not mount the workspace volume, so
it cannot. The worker container's command is the image entrypoint
(`/usr/local/bin/scarab-agent`), which creates the directory from
`SCARAB_RESULT_PATH` and then execs `pi "$@"` (§5.7). This keeps the result
directory's creation from being confused with a failed task.

**[D] The entrypoint captures the answer into `result.md`, it does not rely on the
model to write it.** Pi's stdout is teed into `SCARAB_RESULT_PATH` as well as the
pod log, so a worker that finishes successfully always hands back a result. Left
to the model, a worker could complete and hand back nothing — which is exactly
what happened in a live workspace, and it made the handoff look broken when it was
merely unenforced. Teeing rather than buffering keeps `agents_logs` useful while
the worker is still running.

---

## 6. Interfaces to implement

### 6.1 Broker API

**[D]** The broker exposes intent operations, never Kubernetes ones:

```
POST /agents            -> { "agentId": "..." }        # spawnAgent
GET  /agents            -> [ ... ]                     # listAgents
GET  /agents/<id>       -> { ... }                     # getAgent
GET  /agents/<id>/logs  -> text/stream                 # getAgentLogs
DELETE /agents/<id>     -> 204                         # stopAgent
```

Example `spawnAgent` request — every field is task-level:

```json
{
  "task": "Inspect the authentication subsystem and report likely race conditions.",
  "modelProfile": "coding",
  "resources": { "cpu": "300m", "memory": "512Mi" },
  "workspace": { "mount": "shared" },
  "timeoutSeconds": 1800
}
```

**The broker generates the Kubernetes object.** Pi must never be able to supply a
raw PodSpec, a ServiceAccount name, volume configuration, `securityContext`, node
selectors, or security-affecting labels. Unknown fields should be rejected, not
ignored.

**[P] `modelProfile` is reserved and currently ignored.** The broker accepts the
field and passes it no further; a worker's provider and model come from Pi's own
`settings.json` in the shared agent directory (§8.4). It is listed here rather than
silently dropped because a caller may reasonably expect it to do something, and
it does not. Either implement it as a per-spawn override or remove it.

### 6.2 Broker authentication

**[D]** Short-lived **workspace capability tokens**. The broker derives the
workspace from the authenticated token, never from a request field.

```
BAD:   POST /namespace/foo/spawn
GOOD:  POST /agents          Authorization: Bearer <workspace-capability-token>
```

The broker resolves token → `ws-<slug>` internally. A request that names a
workspace it was not issued for must be impossible or ignored. This is the
highest-value thing to get right and the first thing to attack.

**[P]** pestilence mints the token (it is the trusted plane and holds the signing
key) and mounts it into the root pod as `Secret/pi-root-token` (§5.6). The broker
verifies it and **must not** be able to mint tokens.

**[D]** Format: JWT (JWS), **Ed25519**, **per-workspace keypair**. pestilence
holds the private key; the public key is published as
`ConfigMap/broker-<slug>-token-pubkey` in `scarab` and mounted into that
workspace's broker (§5.5). A compromised broker cannot forge a token, and a
leaked public key exposes nothing.

**[D]** Pinned implementation: `github.com/golang-jwt/jwt/v5`, algorithm
**`EdDSA` only**, public key encoded as **PKIX PEM**
(`x509.MarshalPKIXPublicKey`). Both repositories use the same library. The broker
rejects every other algorithm (`WithValidMethods`), which closes the
HS256/`none` confusion attacks, and requires `exp`. The private key never leaves
pestilence.

**[P]** Claims:

| Claim | Value |
|---|---|
| `iss` | `pestilence` |
| `sub` | `pi-root@ws-<slug>` |
| `aud` | `broker-<slug>` |
| `workspace` | `<slug>` |
| `namespace` | `ws-<slug>` |
| `role` | `root-agent` (workers: never issued) |
| `iat`, `nbf`, `exp` | standard |
| `jti` | unique, for future revocation |

**[P]** The broker must reject any token whose `workspace` ≠ its own slug and
whose `aud` ≠ `broker-<slug>`, in addition to resolving the workspace from the
token. It must reject orchestration calls whose `role` is not `root-agent`.

**[O]** Token lifetime and revocation. The root agent is long-lived, so a truly
short TTL needs a refresh path; a long TTL needs revocation that is not just
"delete the namespace". MVP recommendation: TTL bounded by workspace lifetime,
revocation = delete `Secret/pi-root-token` and the namespace (§8.7).

### 6.3 Pi extension (`platform-tools`)

**[D]** Tools the root agent gets: `agents_spawn`, `agents_list`, `agents_logs`,
`agents_stop`, and later `workspace_usage`, `workspace_storage_request`.

**Pi must not know Kubernetes exists.** The extension talks to the broker's HTTP
API; nothing in the Pi-facing surface mentions namespaces, pods, or RBAC.

**[D] Implemented** in `image/agent/platform-tools/`:

- `broker.js` — the broker client: base URL, capability token, error mapping. It
  has no Pi dependency, so it is unit-tested with plain `node --test`.
- `index.js` — the Pi extension itself, four tools built with `defineTool`.
- The bridge loads it with `pi --extension /opt/scarab/platform-tools/index.js`,
  **for the root agent only**. A worker is started without the flag, so it has no
  orchestration capability (§8.3).
- The client reads `SCARAB_BROKER_URL` and `SCARAB_TOKEN_PATH` from the pod
  environment and calls the broker with `Authorization: Bearer <token>`. The
  token is read per call, never cached, never logged, and never echoed in an
  error.
- A broker error reaches the model as the broker's own agent-safe message, so a
  quota rejection reads as `workspace memory quota reached (…)` rather than a
  Kubernetes admission error (§6.5).

### 6.4 Environment contract

**[P]** Root agent container (bridge):

| Variable | Value |
|---|---|
| `SCARAB_BRIDGE_PORT` | `8000` |
| `SCARAB_BROKER_URL` | `http://broker-<slug>.scarab.svc.cluster.local:8443` (scheme per §8.5) |
| `SCARAB_WORKSPACE_SLUG` | `<slug>` |
| `SCARAB_WORKSPACE_DIR` | `/workspace` |
| `SCARAB_MEMORY_DIR` | `/memory` |
| `SCARAB_SCRATCH_DIR` | `/scratch` |
| `SCARAB_TOKEN_PATH` | `/var/run/scarab/token/token` |
| `SCARAB_RESULT_ROOT` | `/workspace/.agents` |
| `PI_SESSION_DIR` | `/workspace/.pi/sessions` |
| `PI_CODING_AGENT_DIR` | `/workspace/.pi/agent` (on the workspace PVC; Pi's credential store persists here — §8.4) |
| `SCARAB_HOSTNAME` | `<slug>.gobackto.work`; the bridge rejects a request whose `Host` is not this, and a WebSocket `Origin` outside it (§8.8) |
| `SCARAB_ASSERTION_PUBKEY` | `/var/run/scarab/assertion/assertion-key.pub`, mounted read-only from `ConfigMap/town-assertion-pubkey`; the bridge verifies town's `X-Scarab-Assertion` against it. Unset disables the gate, and the bridge says so at startup (§8.8) |
| `SCARAB_BROKER_CA` | `/var/run/scarab/broker-ca/ca.crt`; the certificate the agent trusts for the broker's TLS, passed to Pi as `NODE_EXTRA_CA_CERTS`. Unset means the broker is on plaintext (§8.5) |

**[P]** Broker container:

| Variable | Value |
|---|---|
| `SCARAB_WORKSPACE_SLUG` | `<slug>` |
| `SCARAB_WORKSPACE_NAMESPACE` | `ws-<slug>` |
| `SCARAB_BROKER_PORT` | `8443` |
| `SCARAB_AGENT_IMAGE` | the worker image ref |
| `SCARAB_RESULT_ROOT` | `/workspace/.agents` |
| `SCARAB_TOKEN_PUBLIC_KEY` | `/var/run/scarab/token-pubkey/ed25519.pub` |
| `SCARAB_BROKER_TLS_CERT` | `/var/run/scarab/broker-tls/tls.crt`; set with the key below, or neither (§8.5) |
| `SCARAB_BROKER_TLS_KEY` | `/var/run/scarab/broker-tls/tls.key` |
| `SCARAB_POD_BUDGET` | `6` |
| `SCARAB_MEMORY_BUDGET` | mirror the quota's `requests.memory` (the binding dimension) |
| `SCARAB_CONTAINER_CPU_MAX` / `_MEM_MAX` | mirror the `LimitRange` max |
| `SCARAB_TOKEN_AUDIENCE` | `broker-<slug>` |

> `SCARAB_AGENT_SA_ROOT` / `_WORKER` were removed from this contract. The broker
> never read them: it holds the ServiceAccount names in its own
> `internal/contract` package and hardcodes them in the PodSpec builder. Passing
> them as environment variables created a second source of truth for a value that
> must be identical on both sides, and nothing would have caught a divergence.
> If the broker ever wants them configurable, add them here and read them there
> in the same change.

**[D]** Model credentials are delivered as environment variables from a mounted
Secret (`envFrom: secretRef`) — e.g. `GEMINI_API_KEY` for the default `google`
provider. The image ships with no `auth.json`, and `PI_CODING_AGENT_DIR` is on
ephemeral storage, so no login is ever persisted to the shared volume (§8.4).

**[P]** Model-provider credentials come from `envFrom: secretRef`, never from
`env` literals, so they cannot appear in a rendered manifest. Because the broker
cannot read the `ResourceQuota` (§4.4), `SCARAB_POD_BUDGET`,
`SCARAB_MEMORY_BUDGET` and the container maxima are its only source of truth; if
the `LimitRange` or quota changes in pestilence without changing these, the
broker's accounting silently diverges. They should be derived from a shared
constant rather than typed twice.

### 6.5 Failure semantics

**[D]** Quota and admission failures surface to the agent as **ordinary broker
errors**, not Kubernetes errors, so Pi can recover:

```
spawnAgent failed: workspace pod quota reached (6/6)
```

**[P]** Stable, non-leaking codes:

| Condition | Agent-visible error |
|---|---|
| pod budget reached | `workspace pod quota reached (N/6)` |
| container max exceeded | `requested resources exceed workspace limit` |
| job template rejected at admission | `worker could not be admitted` |
| worker exceeded `activeDeadlineSeconds` | `worker timed out after <N>s` |
| `timeoutSeconds` below the minimum | `timeoutSeconds 15 is too short; a worker needs at least 60 seconds` |
| worker exited non-zero | `worker failed (exit <code>); see agents_logs` |

**[P]** No error, log line, or audit record may contain a model key, the
capability token, or a Kubernetes bearer token (§11.4).

---

## 7. Pi runtime facts you will need

- **Pi has no HTTP or web UI mode.** Its modes are CLI, `--mode rpc`
  (JSONL over stdin/stdout), `--mode json`, and the SDK.
- **Root agent**: a long-lived `pi --mode rpc` process. The browser endpoint
  (§8.2) drives it.
- **Worker**: `pi -p "<task>"`, fire-and-forget (§5.7).
- **Sessions**: persist with `--session-dir` onto the PVC, so a root agent's
  conversation survives a pod restart.
- **Credentials**: for the MVP, a Kubernetes Secret mounted into the pod is
  acceptable (§8.4). Treat it as temporary architecture — the target state is a
  model gateway with central credentials, per-workspace accounting, allowlists,
  and rate limits. Note that anything mounted into the pod is readable by any
  process in that pod.

---

## 8. Decisions and remaining open questions

8.1, 8.2, 8.3 and 8.5–8.9 are now **decided**. Only 8.4's provider choice
remains open; its mechanism is decided.

### 8.1 Who creates the per-workspace Kubernetes objects? — **DECIDED: pestilence applies everything**

pestilence applies every object, in both namespaces, from the same bundle as the
tenant namespace. One reconciler, one lifecycle, atomic teardown, no discovery
mechanism. The coupling — pestilence must know scarab's image, port and env — is
accepted deliberately: the two are meant to be deployed together.

**scarab owns the artifacts** — the broker binary, the agent image, the UI and
its routes. **pestilence owns the privilege** and applies them.

pestilence creates, in the `scarab` namespace:

| Object | Name | Class (§4.6) |
|---|---|---|
| `ServiceAccount` | `<slug>-broker` | boundary |
| `Service` | `broker-<slug>` | endpoint |
| `Deployment` | `broker-<slug>` | **platform — reconciled** (§4.6, §8.9), not runtime |

and in the tenant namespace the `Role`/`RoleBinding` granting that identity, plus
the endpoint and the initial runtime described in §5.

scarab runs **no controller and watches nothing**. It does not create or manage
Kubernetes objects in any namespace.

Consequences to honour:

- The broker pod **must** carry `agents.gobackto.work/workspace: <slug>`, or the
  tenant's `allow-broker-egress` policy will not match it and the tenant cannot
  reach it at all.
- The broker **must** listen on 8443. The root agent dials
  `broker-<slug>.scarab.svc.cluster.local:8443`.
- The `scarab` namespace must exist before the first workspace is provisioned. It
  is a cluster-level object, so it belongs in `my-opps` (§15).
- Because the runtime is created-once rather than reconciled (§4.6), pestilence
  must **not** fight a tenant that replaces the default landing page.

Why not let scarab apply its own objects: creating an Ingress in `ws-<slug>`
requires write access in **every** tenant namespace, which would hand the
semi-trusted component a cross-namespace privilege the design reserves for
pestilence — a compromise of scarab could then re-point any workspace's hostname.
The natural evolution, if the manifest shape starts churning, is for scarab to
ship templates that pestilence vendors and instantiates; that keeps the privilege
boundary intact.

### 8.2 The workspace web endpoint — **DECIDED**

`https://<slug>.gobackto.work` serves the **root agent**. scarab's bridge is the
default landing page, shipped as part of the default runtime (§4.6), and the
tenant may replace it.

Pi has no HTTP mode, so the bridge is net-new: a service that runs
`pi --mode rpc` as a child process and speaks WebSocket/SSE to the browser. Build
it as the **bridge proper plus a deliberately minimal frontend** that renders the
event stream as text. The bridge is the reusable asset; once it exists, a richer
chat UI is purely frontend work, and a browser terminal can slot in behind the
same contract.

Layout: **one container, bridge spawns `pi --mode rpc` as a child.** That keeps
the root agent at one pod (leaving the rest of the 6-pod budget for workers) and
means there is no internal RPC port to secure.

Auth: the ingress (Traefik ForwardAuth → pestilence) is the gate. If the endpoint
receives an identity header it should **ignore it** — any process inside the
tenant can forge it, which is acceptable for a single-owner workspace but wrong
the moment multi-user ACLs arrive. Record that as an explicit single-user
assumption.

**[D] The page needs a stop control.** Pi handles one message at a time, so a long
tool call — an `npm install` with a large dependency tree, say — blocks the agent
and silently queues anything typed meanwhile. Pi supports `abort` and the bridge
exposes it; a UI without a way to send it leaves waiting as the only option, which
reads as a hang. The page also says when a prompt was queued rather than sent.

The Ingress shape **[P]**:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: <slug>
  namespace: ws-<slug>
  annotations: <§8.8>
spec:
  ingressClassName: traefik
  rules:
    - host: <slug>.gobackto.work
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: root-pi, port: { number: 8000 } }
  tls:
    - hosts: [<slug>.gobackto.work]      # no secretName: default wildcard
```

TLS is the Traefik default certificate (`TLSStore/default` →
`wildcard-gobackto-work-tls` in `traefik`). The Ingress must not name a
`secretName` and must not carry a cert-manager annotation: the wildcard already
covers `*.<domain>`, and the tenant namespace must never hold the wildcard
private key.

**[O]** Traefik only serves the default certificate to a router that has TLS
enabled. The exact behaviour of `tls: [{hosts: [...]}]` with no `secretName`
against this Traefik version must be verified on the live cluster. If it does not
pick up the default, the fallback is an explicit `secretName` referencing a
per-workspace certificate — which contradicts the paragraph above and must not be
adopted silently.

### 8.3 Worker tokens — **DECIDED: none**

Workers get no broker token. They have no orchestration capability, and recursive
delegation goes through the root agent. The broker rejects orchestration calls
whose token role is not `root-agent`, and worker pods are not given the token
Secret (§5.7).

### 8.4 Model provider — **DECIDED: supplied in the session, persisted to the PV**

The tenant user supplies their own model credential. It is provided in the
session and **persisted to the workspace volume** in Pi's credential store, so it
survives a pod restart and is shared with the workspace's workers. That is the
point: a tenant brings their own API keys and logins, and the platform never
holds one.

Mechanism:

- Pi's agent directory is `PI_CODING_AGENT_DIR=/workspace/.pi/agent`, **on the
  workspace PVC**. Pi's credential store is `<agent-dir>/auth.json`.
- The bridge's workspace page accepts a provider and key, writes
  `{"<provider>": {"type": "api_key", "key": "..."}}` into `auth.json` (mode
  0600, atomic replace, other providers preserved), and restarts Pi so it reads
  the store. Pi cannot be told a key over RPC, which is why a restart is needed.
- The Pi session survives that restart because it is persisted with
  `--session-dir` on the same volume.
- Workers run with the same `PI_CODING_AGENT_DIR`, so they read the same store
  (§8.10).
- `pi --provider` is passed from the provider the user chose, or from the only
  stored provider, so the stored credential is the one used.

Consequences, including what this deliberately accepts:

- **The credential is on the tenant volume, and that is intended.** Anything in
  the workspace can read it, and it persists for the workspace lifetime. The
  tenant namespace is the unit of compromise and the credential is the tenant's
  own, so this is accepted rather than mitigated. It is destroyed with the
  namespace.
- **A missing credential must fail clearly.** An agent with no stored credential
  must surface an ordinary, non-leaking error (§6.5), not a crash loop. Reporting
  it belongs in the `platform-tools` extension.
- **A provider key must never appear** in a rendered manifest, a log line, or an
  audit record (§11.4). On the workspace volume it lives only in `auth.json`.
- **OAuth logins** persist to the same `auth.json`. Driving Pi's interactive
  `/login` from the bridge is not possible in RPC mode; a browser terminal behind
  the same endpoint is the path, and is a later slice.
- The owner-supplied Kubernetes `Secret` via `envFrom` is **gone**, from both sides.
  It was a third mechanism for what the session key already does and was unused in
  practice, and pestilence has deleted its half (`Spec.ModelSecretName`, the root
  agent's `envFrom`, the broker env var). `contract.EnvModelSecret` and
  `agentpod.Config.ModelSecret` are deleted here to match — the one thing to avoid is
  a contract entry that nothing sets, which reads as working configuration.

The provider is chosen per session, so the platform pins none. For Google Gemini
the provider id is `google` (not `gemini`).

**[D] The model is chosen with the credential and persisted.** A provider's
*default* model may not be available on the user's plan: a free Google AI Studio
key allows `gemini-3.1-pro` at `limit: 0`, so the first prompt fails with a 429
while the credential itself is valid (`pi auth check` reports `ready`). The bridge
therefore accepts a **model** alongside the provider, passes it to Pi as
`--model`, and remembers it in `<agent-dir>/scarab-model` so it survives a pod
restart.

An empty key in the same message means "keep the stored credential and only change
the provider or model", which is the common case when the credential is fine and
only the model is wrong.

**[D] The frontend renders model errors.** A failed model call produces no text at
all, so without this the workspace looks dead while the agent is in fact reporting
a precise failure. The error is unwrapped to the provider's own words —
`quota exceeded`, `model not found` — rather than a JSON blob.

**[D] Workers inherit the choice, because it is written to Pi's own
`settings.json`.** The bridge sets `defaultProvider` and `defaultModel` in
`<agent-dir>/settings.json`, merging rather than clobbering whatever else is
there. The agent directory is shared with the workspace's workers, so a worker
starts on the same provider and model as the root agent.

This was a real bug, and a confusing one: a user running the root agent on a free
OpenRouter model spawned workers that silently ran on Pi's *default* model — a
Gemini model their free tier allowed at limit 0 — so every worker died on a quota
error for a model they were not using. `agents_spawn` offered no way to set it,
because the field that looks like it (`modelProfile`) is unused (§6.1).

### 8.5 Broker transport — **DECIDED, built on both sides, and enabled: TLS**

Previously the broker served plaintext on 8443 and the agent dialled `http://`. The
NetworkPolicy confines the flow to the tenant namespace, so this was defence in depth
rather than the only control — but it is the same class of assumption that failed when
the edge gate turned out never to have been built (red-team finding 1). It is now TLS
(`my-opps` `80965b1`), verified end to end.

**The certificate is per workspace, self-signed, and its own CA.** pestilence mints
`Secret/broker-<slug>-tls` holding `tls.crt`, `tls.key` and `ca.crt` — the same bytes
as `tls.crt` — so there is no platform-wide CA key anywhere, and one agent pins
exactly one certificate that only ever speaks for one broker. The SANs are
`broker-<slug>.scarab.svc.cluster.local`, `broker-<slug>.scarab` and `broker-<slug>`.

**One switch, because half of it is worse than none.** pestilence's `-broker-tls`
gates all of: the broker's mount, the agent's trust anchor, the
environment variables, and the scheme in `SCARAB_BROKER_URL`. A broker told to serve
`https` with no CA fails every call, and a broker left on `http` against a TLS listener
never connects — so the scheme and the anchors move together or not at all.

| | |
|---|---|
| broker mount | `/var/run/scarab/broker-tls` (read-only, `tls.crt` + `tls.key`) |
| broker env | `SCARAB_BROKER_TLS_CERT`, `SCARAB_BROKER_TLS_KEY` |
| agent mount | `/var/run/scarab/broker-ca` (read-only, **`ca.crt` alone**) |
| agent env | `SCARAB_BROKER_CA` |
| URL | `https://broker-<slug>.scarab.svc.cluster.local:8443` |

**The mount is the boundary, not the Secret.** The agent's mount projects `ca.crt`
and nothing else, so the root agent never receives the broker's private key. It must
not be replaced with a plain `secretName` mount, which would hand it over.

**How the agent trusts it.** The certificate is self-signed, so trust has to be
arranged before the process that dials it starts. The bridge sets
`NODE_EXTRA_CA_CERTS` from `SCARAB_BROKER_CA` when it launches Pi, and the extension
refuses an `https` URL when no anchor is configured — so the failure names itself
rather than arriving as a connection error. Verified on its own: without the variable
the handshake fails with `DEPTH_ZERO_SELF_SIGNED_CERT`, with it the request succeeds
(`docs/verification.md`).

**Half a configuration is fatal.** The broker refuses to start with a certificate and
no key rather than quietly serving plaintext, and warns when there is no certificate
at all, so "TLS is off" is visible rather than inferred.

**The certificate is never rotated, deliberately.** It provides confidentiality for an
in-cluster hop; the authorization is the capability token, which does rotate. Rotating
the certificate would need the broker to reload it and the agent's trust anchor to
update in lockstep, through two mounts with the same sub-minute propagation delay the
token has — and a mismatch there is not a degraded connection, it is a broken
workspace. The cost is a dated item: these expire in five years.

**Enabling it needs recreated workspaces, not restarted pods.** The root agent
Deployment is `ClassRuntime`, so an existing workspace keeps the environment it
started with and would stay on `http` while its broker moved to TLS. Nothing changes
for the platform until pestilence flips the flag.

### 8.6 Images and registry — **DECIDED: two images; registry planned**

Two images: `scarab-broker` (a static Go binary) and `scarab-agent` (Node + Pi +
bridge). They share almost nothing and the broker should be as small as possible.

**[D]** Both images exist and load locally. `cluster-setup-scripts/load-image.sh` cross-compiles
the binaries, builds both images with buildah, and imports them into the node's
containerd:

| Image | Base | Size |
|---|---|---|
| `scarab-agent` | `node:24-bookworm-slim` + Pi | ~635 MiB |
| `scarab-broker` | `distroless/static` + one Go binary | ~40 MiB |

A registry is still deferred. The plan is **GHCR** with credentials added on the
dev machine and a GitHub workflow publishing the images, so the cluster pulls
from `ghcr.io/gobackto-work/{scarab-broker,scarab-agent}`. Until then the refs are
`localhost/scarab-{agent,broker}:dev`, imported by hand, and both Deployments must
keep `imagePullPolicy: IfNotPresent`.

### 8.7 Token TTL and revocation — **DECIDED: 24h, rotated at half**

The capability token's TTL is **24 hours**, and pestilence rotates it at half that
(`TokenNeedsRotation` plus a re-mint on the drift sweep). It was 90 days, which the
first red-team review flagged and was right to: the token authorizes broker calls and
is readable by every process in the workspace.

It was long because a short TTL needs a refresh path and there was none. The path now
exists, and it has a hard constraint the bridge must honour: **the bridge reads the
token per call rather than caching it at startup**, so a rotation is picked up without
a restart. The extension already does this. Anything in scarab that caches the token
breaks rotation.

The margin matters: rotation happens at half the TTL rather than at expiry because a
Secret reaches the agent through a **mount**, and the kubelet refreshes it on its own
schedule (measured ~24s here). During that window the stored token is new and the
mounted one is old — which is fine, because the old one is still valid. A TTL whose
half-life is shorter than the mount lag would break a working workspace with an error
that says nothing about TTLs.

Revocation is deleting `Secret/pi-root-token`, plus namespace deletion for teardown.

### 8.8 Endpoint authentication — **DECIDED, and built: signed assertion**

The bridge authenticated **nobody**. The first red-team review reached a tenant
workspace from a public IP, and found that the design's "the Traefik IP allowlist is
the real gate" described a Middleware that had never been created — a policy that
looked right and did not exist, which is the defect class this project keeps finding.

**The edge is the gate, and the bridge verifies it independently.** Three things
landed together, and the order is what kept workspaces up:

1. **town serves `/authz`** and answers the ForwardAuth question. town's session
   cookie is host-only for `town.gobackto.work`, so it never reaches a workspace
   subdomain; town therefore issues a **handoff** that the workspace exchanges for a
   host-scoped cookie (the flow is in the header of `town/src/server/edge.ts`). That is
   why this was a design
   rather than a middleware attachment.
2. **The Ingress carries the chain** — `strip-untrusted-user`, `security-headers`,
   `forwardauth` — and the IP allowlist was removed, so the gate is *who you are*,
   not *where you are*. `strip-untrusted-user` must stay **first**: after
   `forwardauth` it would delete the value the edge had just set.
3. **The bridge verifies the assertion itself.** This is the second, independent
   check, and the one that holds when the edge is not in front of the workspace: a
   middleware dropped from the chain, an Ingress the tenant adds, a port-forward. A
   rejection is a plain **401**, never a redirect — a redirect would hide a missing
   edge behind a login page that always works.

The contract, as built on both sides:

| | |
|---|---|
| header | `X-Scarab-Assertion: <Ed25519 JWT>`, set by town's `/authz` and copied by Traefik's `authResponseHeaders` |
| `alg` | `EdDSA`, pinned by the verifier, never inferred |
| `iss` | `town` |
| `aud` | **the workspace hostname**, so an assertion minted for one workspace cannot be replayed against another's bridge |
| `sub` | the owner id, `github#<numeric id>` |
| TTL | 120s, minted per request, never stored |
| key | `ConfigMap/town-assertion-pubkey` → `SCARAB_ASSERTION_PUBKEY` |
| checked on | every request, and the `/ws` upgrade |
| exempt | `GET /healthz` |

**`X-Auth-User` is ignored.** It is sent, it is unsigned, and the bridge does not
consume it: any process in the tenant could forge it, so it can never be an
authorization input. `strip-untrusted-user` stops a *browser* forging it, but that
makes it trustworthy for display only, and nothing branches on it.

**Fail-closed, and loud about it.** A key that cannot be read or parsed is fatal at
startup: a bridge that cannot verify assertions would serve an unauthenticated
workspace while looking gated, which is worse than not starting. An unset
`SCARAB_ASSERTION_PUBKEY` disables the gate and warns at startup — that is a dev run,
or a workspace created before the key was delivered.

**Verified against the other implementation, not just against itself.** town's own
signer mints an assertion and the bridge verifies it with the key the cluster actually
delivered, and the same token is refused under a different audience. A unit test with
scarab's own keypair cannot catch a disagreement about the audience string or the
issuer; this can (`docs/verification.md`).

**`Host` and `Origin` — built.** A request whose `Host` is not this workspace's own
hostname is rejected, which blunts DNS rebinding. A WebSocket upgrade whose `Origin` is
not this workspace's is rejected with 403: a browser always sends `Origin` on an
upgrade, so a foreign one is the cross-site case, where another page opens the socket
to drive this agent. `GET /healthz` is exempt from the `Host` check, because the
kubelet's probe sends the pod IP and readiness has no user to authenticate as. These
checks are only active when `SCARAB_HOSTNAME` is set, which is every workspace created
since §6.4 gained it.

**Closed:** the earlier accepted risk — that anyone who learned `<slug>.gobackto.work`
could drive the root agent, which can spawn workers and consume model budget — no
longer applies. The random slug suffix is still not authentication, and is still not
relied on.

### 8.9 Broker Deployment reconciliation class — **DECIDED: `ClassPlatform`**

pestilence added a fourth class, `ClassPlatform`, for platform infrastructure
outside the tenant namespace, and classifies `Deployment/broker-<slug>` as it. The
broker is reconciled, not created-once: the tenant cannot write `scarab`, so a
deleted broker must come back. See §4.6 and §8.1.

---

### 8.10 Worker model credentials — **DECIDED: the same persisted store**

Workers run with the same `PI_CODING_AGENT_DIR` on the shared volume, so they read
the same `auth.json` the user's key was written to. No bridge, no Secret, no
broker involvement (§8.4).

---

## 9. The milestone that defines "working"

The platform's acceptance test. scarab owns steps 8–13:

1. User signs into the control plane *(pestilence)*
2. Clicks **New Workspace** *(pestilence)*
3. Platform creates `ws-<random-name>` *(pestilence)*
4. Security policy + quota installed before any tenant workload *(pestilence)*
5. Workspace PVC created *(pestilence)*
6. Root Pi starts *(**scarab**)*
7. User is redirected to `https://<slug>.gobackto.work` *(pestilence + scarab)*
8. **User asks Pi: "Spawn another agent to inspect this repository"**
9. **Pi calls `agents_spawn()`**
10. **Broker starts a constrained worker Pi**
11. **Worker shares workspace storage**
12. **Worker completes**
13. **Root Pi consumes the result**
14. User deletes the workspace *(pestilence)*
15. Namespace, workloads, credentials, ingress and storage disappear *(pestilence)*

---

## 10. Non-goals for the MVP

Deliberately **not** in scope: arbitrary application deployment / service broker,
storage expansion, LAN service brokers, multi-user collaboration, recursive
worker authority, CRDs, an agent-to-agent message bus, a dynamic model gateway.

---

## 13. Environment notes

- Reference cluster: a single-node k0s deployment running Kubernetes **v1.36**
- CNI Cilium 1.20.2; kube-proxy retained; `hostPort` works via `cni.chainingMode=portmap`
- The `scarab` namespace is declared in
  `my-opps/cluster/manifests/namespace-scarab.yaml` and applied by
  `cluster/bootstrap-addons.sh` before the cluster-level policy (§15)
- Kubernetes Go modules: pin to `v0.36.x` to match the cluster

---

## 14. References

- [`my-opps/cluster/README.md`](https://github.com/gobackto-work/my-opps/blob/main/cluster/README.md) — cluster build, and the two CNI traps
- [`my-opps/docs/tls.md`](https://github.com/gobackto-work/my-opps/blob/main/docs/tls.md) — wildcard TLS via delegated DNS-01
- [`my-opps/cluster/manifests/namespace-scarab.yaml`](https://github.com/gobackto-work/my-opps/blob/main/cluster/manifests/namespace-scarab.yaml) — the platform namespace, PSA `restricted`
- [`my-opps/cluster/manifests/validatingadmissionpolicy-tenant-pods.yaml`](https://github.com/gobackto-work/my-opps/blob/main/cluster/manifests/validatingadmissionpolicy-tenant-pods.yaml) — the admission rules in §4.1, with the reasoning inline
- [`pestilence/internal/tenant/bundle.go`](https://github.com/gobackto-work/pestilence/blob/main/internal/tenant/bundle.go) — the tenant-side objects, with security reasoning inline
- [`pestilence/internal/tenant/class.go`](https://github.com/gobackto-work/pestilence/blob/main/internal/tenant/class.go) — the reconciliation classes in §4.6
- [`pestilence/internal/provisioner/reconciler.go`](https://github.com/gobackto-work/pestilence/blob/main/internal/provisioner/reconciler.go) — idempotent apply, ordered delete

---

## 15. Cluster prerequisites (my-opps)

**[D]** The `scarab` namespace is declared in
`my-opps/cluster/manifests/namespace-scarab.yaml` and applied by
`cluster/bootstrap-addons.sh` **before** the cluster-level policy. It carries PSA
`restricted` and `agents.gobackto.work/component: broker-platform`, and must
exist before the first workspace is provisioned.

**[D]** `scarab` requirements (satisfied by the manifest above):

- Must not carry `agents.gobackto.work/workspace` (§5.5) — confirmed.
- Enforces PSA `restricted` — confirmed; this is why the broker image must comply
  (§4.2).
- Should not have a default-deny NetworkPolicy for the MVP. If one is added, it
  must permit egress to `kubernetes.default.svc:443` (the broker's API calls) and
  ingress from `ws-*` pods on 8443 — otherwise the broker cannot reach the API
  server and the tenant cannot reach the broker.

**[D]** ForwardAuth middleware is **attached** (§8.8). `my-opps` defines
`forwardauth` (Traefik → town's `/authz`) and `strip-untrusted-user`, and pestilence
attaches both to every workspace Ingress by name; the IP allowlist was removed once
ForwardAuth could carry the load.

---

## Appendix A — canonical constants

```
slug                ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$   ≤ 40
tenantNamespace     ws-<slug>
platformNamespace   scarab
host                <slug>.gobackto.work
brokerSA            <slug>-broker         (in scarab)
brokerDeployment    broker-<slug>         (in scarab)
brokerService       broker-<slug>         (in scarab)
rootService         root-pi               (in ws-<slug>)
rootDeployment      root-agent            (in ws-<slug>)
workerJob           worker-<agent-id>     (in ws-<slug>)
saRoot              pi-root
saWorker            pi-worker
pvcWorkspace        workspace   20Gi
pvcMemory           memory       5Gi
httpPort            8000
brokerPort          8443
mountWorkspace      /workspace
mountMemory         /memory
mountScratch        /scratch
mountToken          /var/run/scarab/token
resultPath          /workspace/.agents/<agent-id>/result.md
sessionDir          /workspace/.pi/sessions
labelWorkspace      agents.gobackto.work/workspace
labelComponent      agents.gobackto.work/component
components          root-agent | worker | broker
podBudget           6   (root + up to 5 workers by pod count; broker is not charged)
memBudget           quota requests.memory — binds at 3 workers with §5.7 sizes
containerMax        1 CPU / 2Gi
```
