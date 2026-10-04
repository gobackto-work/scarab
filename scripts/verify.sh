#!/usr/bin/env bash
#
# The verification gate. One command, identical for every agent and (eventually)
# for CI.
#
# Design rules, each learned the hard way:
#
#   * STATELESS. Every check re-reads the tree. Nothing here is an editor, an LSP,
#     or a daemon, so nothing can serve a stale answer -- including across an agent
#     boundary, which is the case that matters when two agents share a working tree.
#   * FAST. If it is not quick enough to run before every claim of "it works", it
#     will not be run. The slow checks are behind --deep.
#   * VERSION-ASSERTED. With no CI, agents install tools ad hoc. A version mismatch
#     makes "it passed for me" meaningless, so a mismatch is a FAILURE here, not a
#     warning.
#   * EXIT-CODE DRIVEN, with a quotable summary line.
#
# Usage:
#   scripts/verify.sh            # the gate
#   scripts/verify.sh --deep     # adds the slow checks (mutation testing)
#
# Tools and the versions this was derived with:
#
#   go install honnef.co/go/tools/cmd/staticcheck@2026.2.1
#   go install golang.org/x/tools/cmd/deadcode@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
#   npm ci                                   # eslint, knip, jscpd
#   plus the binaries: 0.11.0 shellcheck, 2.15.1 hadolint, 8.30.1 gitleaks
#   (see docs/verification.md for where each comes from)
#
# The Go checks are scoped to ./cmd/... and ./internal/... rather than ./...
# because npm installs a vendored Go file inside node_modules, which ./... would
# otherwise treat as part of this module.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

GO_PKGS=(./cmd/... ./internal/...)

export PATH="$HOME/.local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH"

DEEP=0
[ "${1:-}" = "--deep" ] && DEEP=1

FAILED=0
PASSED=0
OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok() { PASSED=$((PASSED + 1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	FAILED=$((FAILED + 1))
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	if [ -s "$OUT" ]; then sed 's/^/       /' "$OUT" | head -25; fi
}

# check NAME CMD... — passes when CMD exits zero AND prints nothing.
check_quiet() {
	local name="$1"
	shift
	if "$@" >"$OUT" 2>&1 && [ ! -s "$OUT" ]; then ok "$name"; else bad "$name"; fi
}

# check NAME CMD... — passes when CMD exits zero, output ignored.
check() {
	local name="$1"
	shift
	if "$@" >"$OUT" 2>&1; then ok "$name"; else bad "$name"; fi
}

# require TOOL VERSION_ARGS EXPECTED — a missing or mismatched tool is a failure,
# because otherwise the gate is not the same gate on both sides.
require() {
	local tool="$1" got="$2" want="$3"
	if ! command -v "$tool" >/dev/null 2>&1; then
		bad "$tool not installed (want $want)"
		return 1
	fi
	case "$got" in
	*"$want"*) return 0 ;;
	*)
		bad "$tool is $got, want $want"
		return 1
		;;
	esac
}

step "tool versions"
require go "$(go version 2>&1)" "go1.27" || true
require staticcheck "$(staticcheck -version 2>&1)" "2026.2.1" || true
require golangci-lint "$(golangci-lint --version 2>&1)" "2.14.0" || true
require govulncheck "$(govulncheck -version 2>&1 | head -1)" "go1.27" || true
require shellcheck "$(shellcheck --version 2>&1 | sed -n 's/^version: //p')" "0.11.0" || true
require hadolint "$(hadolint --version 2>&1)" "2.15.1" || true
require gitleaks "$(gitleaks version 2>&1)" "8.30.1" || true

step "tool installer"
# Every tool the gate REQUIRES must also be installed by scripts/install-tools.sh. A
# tool the installer omits falls back to whatever the runner image happens to ship --
# which is how shellcheck 0.9.0 (ubuntu-24.04) shadowed the required 0.11.0: green on
# a developer host, red in CI. The gate is the same everywhere, or it is not a gate.
missing=""
while read -r tool; do
	case "$tool" in
	go | node | gofmt) continue ;; # from the toolchain, not the installer
	esac
	grep -q -- "$tool" scripts/install-tools.sh || missing="$missing $tool"
done < <(sed -n 's/^require \([A-Za-z][A-Za-z0-9_-]*\) .*/\1/p' scripts/verify.sh | sort -u)
if [ -n "$missing" ]; then
	bad "scripts/install-tools.sh does not install:$missing"
else
	ok "scripts/install-tools.sh covers every required tool"
fi

step "go: format, vet, lint, dead code"
check_quiet "gofmt" gofmt -l .
check "go vet" go vet "${GO_PKGS[@]}"
check "staticcheck" staticcheck "${GO_PKGS[@]}"
check_quiet "deadcode (unreachable functions)" deadcode "${GO_PKGS[@]}"
check "golangci-lint (complexity, duplication, security)" golangci-lint run "${GO_PKGS[@]}"

step "go: tests and vulnerabilities"
check "go test" go test "${GO_PKGS[@]}"
# Race detection needs cgo, so it runs where a C toolchain exists (the Linux host).
if [ "$(go env CGO_ENABLED)" = "1" ]; then
	check "go test -race" go test -race "${GO_PKGS[@]}"
else
	printf '  \033[33mskip\033[0m go test -race (CGO_ENABLED=0; run on the Linux host)\n'
fi
check "govulncheck" govulncheck "${GO_PKGS[@]}"

step "javascript"
check "node --test (bridge UI + platform tools)" npm test
check "eslint (complexity, sonarjs)" npx --no-install eslint .
check "knip (unused files, exports, deps)" npx --no-install knip --reporter compact
check "jscpd (duplication, all languages)" npx --no-install jscpd --config .jscpd.json .

step "shell, dockerfile, secrets"
check "shellcheck (entrypoint + scripts)" shellcheck -s bash image/agent/scarab-agent scripts/*.sh cluster-setup-scripts/*.sh
check "hadolint (Dockerfiles)" hadolint image/agent/Dockerfile image/broker/Dockerfile
check "gitleaks (tree + git history)" gitleaks detect --source . --no-banner --redact

if [ "$DEEP" = "1" ]; then
	step "deep: mutation testing (slow, and it measures test STRENGTH, not this change)"
	if command -v gremlins >/dev/null 2>&1; then
		check "gremlins (agentpod)" gremlins unleash ./internal/agentpod
	else
		printf '  \033[33mskip\033[0m gremlins not installed\n'
	fi
fi

printf '\n\033[1m%s: %d passed, %d failed\033[0m\n' "$(basename "$0")" "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
	printf 'not verified\n'
	exit 1
fi
printf 'verified: %d checks, 0 failures\n' "$PASSED"
