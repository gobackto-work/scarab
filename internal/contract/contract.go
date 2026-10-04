// Package contract holds the values that cross the pestilence <-> scarab
// boundary.
//
// Every constant here also appears in pestilence/internal/tenant or in a
// template pestilence applies. The normative description is
// scarab/docs/architecture.md §5; this package exists so scarab's code has one
// source of truth instead of literals scattered through the broker and the
// PodSpec builder. When pestilence is ready to vendor scarab's templates
// (handoff §8.1), it should import this package rather than re-type the values.
package contract

// Namespaces and naming.
const (
	// PlatformNamespace hosts the per-workspace broker. It is cluster-level and
	// declared in my-opps.
	PlatformNamespace = "scarab"

	// TenantNamespacePrefix is prepended to a slug to form the workspace
	// namespace, ws-<slug>.
	TenantNamespacePrefix = "ws-"
)

// Labels. LabelWorkspace and LabelComponent are load-bearing: NetworkPolicy and
// Service selectors match on them, and a missing LabelWorkspace on the broker
// pod silently makes the broker unreachable (handoff §5.2).
const (
	LabelWorkspace = "agents.gobackto.work/workspace"
	LabelComponent = "agents.gobackto.work/component"
	LabelManagedBy = "app.kubernetes.io/managed-by"

	ManagedByScarab = "scarab"
)

// AnnotationTask carries the worker's task string on the Job, so listAgents can
// report it without reading the PodSpec. It is tenant data inside the tenant
// namespace, not a secret.
const AnnotationTask = "agents.gobackto.work/task"

// Component label values.
const (
	ComponentRoot   = "root-agent"
	ComponentWorker = "worker"
	ComponentBroker = "broker"
)

// ServiceAccount names. The tenant admission policy allows exactly these two
// (handoff §4.1).
const (
	SARoot   = "pi-root"
	SAWorker = "pi-worker"
)

// Ports.
const (
	HTTPPort   = 8000
	BrokerPort = 8443
)

// Mount paths. These are a contract because the root agent and its workers
// share them (handoff §5.8).
const (
	MountWorkspace = "/workspace"
	MountMemory    = "/memory"
	MountScratch   = "/scratch"
	MountTmp       = "/tmp"

	// MountToken is where the root agent's capability token is mounted. It is
	// never on the shared volume.
	MountToken = "/var/run/scarab/token"

	// MountBrokerTLS is where pestilence mounts the broker's serving certificate
	// and key, read-only (handoff §8.5).
	MountBrokerTLS = "/var/run/scarab/broker-tls"

	// MountBrokerCA is where the agent gets the certificate to trust. It projects
	// ca.crt ALONE: the mount is the boundary, not the Secret, so the root agent
	// never receives the broker's private key (handoff §8.5).
	MountBrokerCA = "/var/run/scarab/broker-ca"
)

// Broker TLS file names, inside the mounts above. The certificate is its own CA,
// so ca.crt and tls.crt are the same bytes, and one agent pins exactly one
// certificate that only ever speaks for one broker.
const (
	BrokerTLSCertFile = MountBrokerTLS + "/tls.crt"
	BrokerTLSKeyFile  = MountBrokerTLS + "/tls.key"
	BrokerCAFile      = MountBrokerCA + "/ca.crt"
)

// Workspace paths.
const (
	// ResultRoot is the directory, on the shared volume, under which each worker
	// writes <agent-id>/result.md.
	ResultRoot = "/workspace/.agents"

	// SessionDir is where the root agent persists its conversation.
	SessionDir = "/workspace/.pi/sessions"

	// AgentDir is Pi's agent directory (PI_CODING_AGENT_DIR), on the workspace
	// volume. Its auth.json is where a user-supplied model credential is persisted,
	// so it survives a pod restart and is shared with the workspace's workers
	// (handoff §8.4). It is deliberately NOT ephemeral.
	AgentDir = "/workspace/.pi/agent"
)

// Environment variable names shared between the bridge and the broker.
const (
	EnvAgentID    = "SCARAB_AGENT_ID"
	EnvResultPath = "SCARAB_RESULT_PATH"
	EnvSessionDir = "PI_SESSION_DIR"
	EnvAgentDir   = "PI_CODING_AGENT_DIR"
	EnvHome       = "HOME"
)

// Image entrypoints.
const (
	// AgentEntrypoint is the worker entrypoint in the scarab-agent image. It
	// creates the result directory from SCARAB_RESULT_PATH and then execs
	// `pi "$@"`. The broker cannot do this itself: it runs in the scarab
	// namespace and does not mount the workspace volume (handoff §5.9).
	AgentEntrypoint = "/usr/local/bin/scarab-agent"
)

// Broker environment variable names. pestilence sets these on the broker
// Deployment (handoff §6.4); cmd/broker reads them. A value must not be typed
// twice, so both sides use these constants.
const (
	EnvWorkspaceSlug   = "SCARAB_WORKSPACE_SLUG"
	EnvWorkspaceNS     = "SCARAB_WORKSPACE_NAMESPACE"
	EnvBrokerPort      = "SCARAB_BROKER_PORT"
	EnvAgentImage      = "SCARAB_AGENT_IMAGE"
	EnvResultRoot      = "SCARAB_RESULT_ROOT"
	EnvPodBudget       = "SCARAB_POD_BUDGET"
	EnvMemoryBudget    = "SCARAB_MEMORY_BUDGET"
	EnvContainerCPUMax = "SCARAB_CONTAINER_CPU_MAX"
	EnvContainerMemMax = "SCARAB_CONTAINER_MEM_MAX"
	EnvTokenPublicKey  = "SCARAB_TOKEN_PUBLIC_KEY"
	EnvTokenAudience   = "SCARAB_TOKEN_AUDIENCE"
	// Run state reporting. The broker is the only component that reports to the control
	// plane, because it is the only one that outlives the agent processes: an agent
	// cannot report the state it reaches by dying.
	//
	// EnvPlatformURL is the control plane's base URL, and EnvReportTokenPath is a file
	// whose contents are the reporting token. It is a path and not the value, for the
	// same reason the bridge's token is: the control plane rotates it at half its TTL and
	// the kubelet replaces a mounted file in place.
	EnvPlatformURL     = "SCARAB_PLATFORM_URL"
	EnvReportTokenPath = "SCARAB_REPORT_TOKEN_PATH"
	// Broker TLS (handoff §8.5). pestilence sets both together or neither; the
	// broker refuses to start with half a configuration rather than quietly
	// serving plaintext.
	EnvBrokerTLSCert = "SCARAB_BROKER_TLS_CERT"
	EnvBrokerTLSKey  = "SCARAB_BROKER_TLS_KEY"
)

// Bridge environment variable names (handoff §6.4). pestilence sets these on the
// root agent Deployment; cmd/bridge reads them.
const (
	EnvBridgePort   = "SCARAB_BRIDGE_PORT"
	EnvBrokerURL    = "SCARAB_BROKER_URL"
	EnvWorkspaceDir = "SCARAB_WORKSPACE_DIR"
	EnvMemoryDir    = "SCARAB_MEMORY_DIR"
	EnvScratchDir   = "SCARAB_SCRATCH_DIR"
	EnvTokenPath    = "SCARAB_TOKEN_PATH"
	EnvPIExtension  = "SCARAB_PI_EXTENSION"
	EnvHostname     = "SCARAB_HOSTNAME"
	// EnvBrokerCA points the agent at the certificate to trust, which is
	// BrokerCAFile. Unset means the broker is on plaintext (handoff §8.5).
	EnvBrokerCA = "SCARAB_BROKER_CA"
	// EnvAssertionPubkey points at town's assertion public key, mounted read-only
	// from ConfigMap/town-assertion-pubkey. Unset disables the assertion gate,
	// which is what a dev run and a workspace created before the key was delivered
	// both get; the bridge says so at startup rather than leaving it implicit
	// (handoff §6.4).
	EnvAssertionPubkey = "SCARAB_ASSERTION_PUBKEY"
)

// The workspace assertion. town mints it in its ForwardAuth handler, Traefik
// copies it onto the upstream request, and the bridge verifies it. It is the one
// value in the chain the browser never sees (handoff §6.4, §8.8).
const (
	// HeaderAssertion is the header carrying the assertion. It is listed in the
	// edge middleware's authResponseHeaders, so it reaches the bridge and nothing
	// else does.
	HeaderAssertion = "X-Scarab-Assertion"

	// AssertionIssuer is the only acceptable `iss`. It is town, not pestilence:
	// the edge asks town who the user is, and town is what signs for it.
	AssertionIssuer = "town"

	// MountAssertion is where pestilence mounts the public key, read-only.
	MountAssertion = "/var/run/scarab/assertion"

	// AssertionKeyFile is the key inside MountAssertion, and the default value of
	// SCARAB_ASSERTION_PUBKEY.
	AssertionKeyFile = MountAssertion + "/assertion-key.pub"
)

// PlatformToolsExtension is the root agent's Pi extension, baked into the
// scarab-agent image at this path. The bridge loads it with `pi --extension` for
// the root agent only; workers get no orchestration capability (handoff §6.3,
// §8.3).
const PlatformToolsExtension = "/opt/scarab/platform-tools/index.js"

// Object names that are part of the contract.
const (
	NameRootService  = "root-pi"
	NameRootDeploy   = "root-agent"
	NameWorkspacePVC = "workspace"
	NameMemoryPVC    = "memory"
	NameWorkerSA     = "pi-worker"
)

// Resource defaults, mirroring the workspace LimitRange (handoff §4.3).
const (
	PodBudget = 6 // root agent + up to 5 workers; the broker is not charged
)

// The vocabulary of a run, from pestilence/docs/event-record.md. The normative
// description is that document; these constants exist so the two repositories cannot
// spell a state differently.
const (
	// TokenAudienceReport is the audience of the token the control plane accepts a report
	// with. It is not the broker audience, so an agent's token is refused at the
	// reporting endpoint and a reporting token is refused at the broker.
	TokenAudienceReport = "pestilence-ingest"

	// ReportPathTemplate is the reporting endpoint. The verb takes the workspace id.
	ReportPathTemplate = "/api/workspaces/%s/events"
)

// Mode says whether a run can block on a person. A batch run cannot reach StateWaiting,
// because a run that waits for a person who is not there waits for ever.
const (
	ModeInteractive = "interactive"
	ModeBatch       = "batch"
)

// The states a run can be in.
const (
	StateRunning         = "running"
	StateWaiting         = "waiting"
	StateSucceeded       = "succeeded"
	StateFailed          = "failed"
	StateCancelled       = "cancelled"
	StateBudgetExhausted = "budget_exhausted"
)
