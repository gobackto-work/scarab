# Verification

The gate is `scripts/verify.sh`. One command, identical for every agent and
(eventually) for CI:

```bash
bash scripts/verify.sh          # the gate — seconds
bash scripts/verify.sh --deep   # adds the slow checks (mutation testing)
```

It prints a per-check result and ends with a quotable line:

```
verify.sh: 16 passed, 0 failed
verified: 16 checks, 0 failures
```

## The four design rules

1. **Stateless.** Every check re-reads the tree. Nothing in the gate is an editor,
   an LSP, or a daemon, so nothing can serve a stale answer — including across an
   agent boundary, which is the case that matters when two agents share a working
   tree.
2. **Fast enough to actually run.** A gate an agent skips is worse than no gate.
   Anything slow goes behind `--deep`.
3. **Version-asserted.** With no CI, agents install tools ad hoc, and a version
   mismatch makes "it passed for me" meaningless. A mismatch is a **failure**, not
   a warning.
4. **Exit-code driven**, with a summary line worth pasting.

## Why not LSP / pi-lens as the gate

pi-lens is genuinely useful *while writing* — it hooks every write and edit and
runs language-aware checks. It is not usable as the authority, and the reason is
structural rather than a bug awaiting a fix. From its own issue tracker:

| Issue | What it means for us |
|---|---|
| **#1668** | Servers never learn about files changed or deleted **outside open documents** — no type-3 Deleted watched-files event exists. A `git checkout`, an `npm install`, or **another agent's edit** desyncs the LSP invisibly |
| #1641 | Diagnostics served with line numbers past EOF — the in-memory document diverged from disk |
| #1669 | `workspace/diagnostic/refresh` goes unanswered: the server says "my results are stale" and the client does not invalidate |
| #1993 | `mode=full` replays stale mid-edit diagnostics as blocking despite a fresh clean sweep |

#1668 is this repository's topology exactly. So the split is:

- **Gate** — stateless, disk-derived, identical for both agents.
- **Authoring aid** — LSP and/or pi-lens, for navigation and fast feedback. If it
  reports something, re-run the corresponding gate check before believing it.

## What the gate runs

| Check | Version | Catches | Install |
|---|---|---|---|
| `gofmt -l` | go1.27.1 | formatting | toolchain |
| `go vet` | go1.27.1 | standard analyzers | toolchain |
| `staticcheck` | 2026.2.1 | correctness, simplifications, unused | `go install honnef.co/go/tools/cmd/staticcheck@2026.2.1` |
| `deadcode` | x/tools v0.50.0 | functions unreachable from a binary | `go install golang.org/x/tools/cmd/deadcode@latest` |
| `golangci-lint` | v2.14.0 | **complexity** (`gocognit`, `gocyclo`, `funlen`, `nestif`, `maintidx`), **duplication within the module** (`dupl`), `revive`, `gocritic`, `misspell`, `unparam`, `gosec` | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0` |
| `go test` | — | behaviour | toolchain |
| `go test -race` | — | data races | **Linux host** (needs cgo) |
| `govulncheck` | v1.8.0 | known vulnerabilities in dependencies | `go install golang.org/x/vuln/cmd/govulncheck@latest` |
| `node --test` | node 26 | the bridge UI module and the Pi extension | toolchain |
| `eslint` | 10.11.0 | unused vars, `sonarjs` rules, **cognitive complexity**, `max-depth`, `max-lines-per-function` | `npm ci` |
| `knip` | 6.38.0 | unused files, exports, dependencies | `npm ci` |
| `jscpd` | 5.3.2 | **duplication across every language**, which is the class no single-language tool sees | `npm ci` |
| `shellcheck` | 0.11.0 | the container entrypoint, `scripts/*.sh` and `cluster-setup-scripts/*.sh` | release zip from `koalaman/shellcheck` |
| `helm lint`, `helm template` | v4 | a chart that does not render | release tarball |
| `hadolint` | 2.15.1 | the Dockerfiles | release exe from `hadolint/hadolint` |
| `gitleaks` | 8.30.1 | secrets in the tree **and in git history** | release zip from `gitleaks/gitleaks` |

`shellcheck`, `hadolint` and `gitleaks` go in `~/.local/bin`; the script adds
`~/.local/go/bin`, `~/go/bin` and `~/.local/bin` to `PATH` itself.

## Thresholds are hard limits, deliberately

For a human, a cyclomatic-complexity number is close to useless. For an agent it is
a **forcing function**: it makes the code get restructured rather than annotated.
So the limits are set low enough to bite and the failures are not suppressed:

- Go: `gocognit` 20, `gocyclo` 15, `funlen` 60, `nestif` 5, `maintidx` 20.
- JS: `complexity` 12, `sonarjs/cognitive-complexity` 20, `max-depth` 4,
  `max-lines-per-function` 60.

When the gate was first run at these settings it reported 17 findings, and the
code changed as a result.

Tests are exempt from the complexity and error-checking rules (see the exclusions
in `.golangci.yml`), because suppressing them there is what keeps the thresholds
low enough to bite in production code.

## Why mutation testing is `--deep`

It runs the suite once per mutant: O(mutants × suite time), so a package is minutes
and the repository is an hour. And the signal is not about the current change — a
surviving mutant almost always means a *pre-existing* gap in the tests, which as a
gate is a large, mostly-irrelevant backlog that tempts an agent to write tests that
kill mutants rather than tests that describe behaviour.

It measures test **strength**, which moves when tests change, not when code does. So
it is run deliberately: before trusting a security-critical package, after adding an
invariant, or before a milestone.

```bash
go install github.com/go-gremlins/gremlins/cmd/gremlins@latest
bash scripts/verify.sh --deep
```

## Refreshing the Pi provider list

`KNOWN_PROVIDERS` in `internal/bridge/app.mjs` is a snapshot of the pinned Pi
version, extracted from Pi's own bundle. A hand-written list is what caused the bug
it fixed: nine providers offered, every token-auth provider missing. Refresh with:

```bash
node -e '
const s = require("fs").readFileSync(process.argv[1], "utf8");
const a = s.indexOf("google:\"gemini-3.1-pro-preview\"");
const b = s.slice(s.lastIndexOf("{", a) + 1, s.indexOf("}", a));
console.log([...b.matchAll(/(?:^|,)"?([a-z0-9-]+)"?:/g)].map((m) => m[1]).join(" "));
' <pi>/dist/bundle/chunks/chunk-*.js
```

## Interop checks the gate cannot run

The gate tests scarab against scarab. Some of the values that matter most cross a
repository boundary, and a unit test with scarab's own keypair cannot catch a
disagreement about them — only the other implementation can. These stay manual,
because they need the cluster's keys.

### The workspace assertion

town signs, the bridge verifies, and the thing that proves the two agree is minting
with town's real signer and verifying with the key the cluster actually delivered.
Run it after touching either side, and first when a workspace that should be
reachable answers 401:

```sh
# The key the cluster delivered.
ssh dev 'kubectl -n ws-<slug> get cm town-assertion-pubkey \
  -o jsonpath="{.data.assertion-key\.pub}"' > /tmp/pub.pem

# Minted by town's own code -- node strips the TypeScript types, so this is the
# real Assertions class and not a re-implementation of it.
TOK=$(cd ~/town && KEY_PEM=$(ssh dev 'kubectl -n town get secret town-assertion-key \
  -o jsonpath="{.data.assertion-key\.pem}"' | base64 -d) \
  node --input-type=module -e '
    import { Assertions } from "./src/server/assertion.ts";
    const aud = process.argv[1];
    const a = await Assertions.fromPem(process.env.KEY_PEM, aud, 120);
    console.log(await a.mintFor("github#1234", aud));
  ' <slug>.gobackto.work)

go run ./cmd/verify-assertion -key /tmp/pub.pem -audience <slug>.gobackto.work <<<"$TOK"
go run ./cmd/verify-assertion -key /tmp/pub.pem -audience other.gobackto.work  <<<"$TOK"
```

Expected: the first is **accepted**, the second is **rejected as an invalid
audience**. The second half is the point. An accepted token shows only that the
happy path agrees; the rejection shows `aud` is really checked, which is what stops
an assertion minted for one workspace being replayed at another's bridge.

### Broker TLS trust

The broker's certificate is self-signed, so the question is whether the agent
actually trusts it. The mechanism is `NODE_EXTRA_CA_CERTS`, which the bridge sets
from `SCARAB_BROKER_CA`; Node reads it at process start, so it is worth checking on
its own — when it is wrong, the symptom is a connection error that says nothing at
all about trust.

```sh
D=$(mktemp -d)
# MSYS_NO_PATHCONV, or git-bash rewrites the -subj value into a Windows path.
MSYS_NO_PATHCONV=1 openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$D/tls.key" -out "$D/tls.crt" -days 1 \
  -subj "/CN=broker-test.scarab.svc.cluster.local" \
  -addext "subjectAltName=DNS:broker-test.scarab.svc.cluster.local,IP:127.0.0.1"

# A TLS server with that certificate, then the same client with and without the anchor:
#   node --input-type=module -e 'console.log((await fetch("https://127.0.0.1:8443/")).status)'
#   NODE_EXTRA_CA_CERTS="$D/tls.crt" node --input-type=module -e '...'
```

Expected: `DEPTH_ZERO_SELF_SIGNED_CERT` without the variable, and `200` with it. That
is the whole of the trust path, and it fails closed — a certificate the process was
never told to trust is rejected, so a missing anchor cannot silently become an
unverified connection.

### The broker capability token

The same shape, with pestilence minting instead of town: mint with pestilence's
signer and verify with `ConfigMap/broker-<slug>-token-pubkey`, then confirm a token
for another workspace is refused. **Not yet run** — it needs a workspace whose broker
has a token to present, and pestilence's signing key is the control plane's.

## Deliberately not in the gate

| Not included | Why |
|---|---|
| pi-lens / LSP | stateful; see above. Use it while writing, verify with the gate |
| `goleak` | the bridge tests deliberately leave supervisor goroutines running; a leak checker would report them and make the gate flaky. `-race` is the higher-value concurrency check |
| `kubeconform`, `conftest`, `kube-linter` | no Kubernetes manifests live in this repository. They belong to `my-opps`, and the strongest check there is `kubectl --dry-run=server` against the real API server |
| `trivy`, SBOM | no published images yet |
| the live cluster check | the least automatable and the most valuable. `kubectl --dry-run=server` and provisioning a real workspace found more than every linter combined. It stays a deliberate, occasional, hand-run pass |

## When the gate fails because a tool is missing

That is intentional. Install the version named in the failure and re-run; do not
lower the requirement to match what happens to be installed, or the two agents stop
running the same gate.
