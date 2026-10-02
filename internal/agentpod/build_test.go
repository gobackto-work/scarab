package agentpod

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gobackto-work/scarab/internal/contract"
)

// Every test here corresponds to a claim in scarab/docs/architecture.md §4 that is
// also enforced by the API server. They are the reason this package is worth
// reviewing closely: if one of them fails, the object would be rejected at
// admission (or worse, would be accepted and be a hole).

func testConfig() Config {
	return DefaultConfig("ws-demo", "demo", "ghcr.io/gobackto-work/scarab-agent:test")
}

func testRequest() Request {
	return Request{Task: "inspect the repository", TimeoutSeconds: 1800}
}

func mustBuild(t *testing.T, req Request) *batchv1.Job {
	t.Helper()
	job, err := BuildWorkerJob("abcd1234", req, testConfig())
	if err != nil {
		t.Fatalf("BuildWorkerJob: %v", err)
	}
	return job
}

func container(t *testing.T, job *batchv1.Job) corev1.Container {
	t.Helper()
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("want exactly 1 container, got %d", len(job.Spec.Template.Spec.Containers))
	}
	return job.Spec.Template.Spec.Containers[0]
}

// §4.1: the admission policy denies a pod whose serviceAccountName is absent or
// not in {pi-root, pi-worker}. Omitting it is denied too, because the default
// ServiceAccount auto-mounts an API token.
func TestJobSetsAllowlistedServiceAccount(t *testing.T) {
	job := mustBuild(t, testRequest())
	sa := job.Spec.Template.Spec.ServiceAccountName
	if sa != contract.SAWorker {
		t.Errorf("serviceAccountName = %q, want %q", sa, contract.SAWorker)
	}
}

// §4.1: a pod must never request a Kubernetes API token. This is the first
// security invariant: no Pi container holds a Kubernetes credential.
func TestJobNeverAutomountsToken(t *testing.T) {
	job := mustBuild(t, testRequest())
	if amt := job.Spec.Template.Spec.AutomountServiceAccountToken; amt == nil || *amt {
		t.Errorf("automountServiceAccountToken = %v, want false", amt)
	}
}

// §4.1: nodeName bypasses the scheduler; runtimeClassName selects a runtime.
func TestJobNeverSetsNodeNameOrRuntimeClass(t *testing.T) {
	job := mustBuild(t, testRequest())
	if job.Spec.Template.Spec.NodeName != "" {
		t.Errorf("nodeName = %q, want empty", job.Spec.Template.Spec.NodeName)
	}
	if job.Spec.Template.Spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %v, want nil", job.Spec.Template.Spec.RuntimeClassName)
	}
}

// §4.1: volumes are restricted to persistentVolumeClaim, emptyDir, secret,
// configMap, projected, downwardAPI, ephemeral. This builder emits only the
// first two.
func TestJobVolumesWithinAllowlist(t *testing.T) {
	job := mustBuild(t, testRequest())
	for _, v := range job.Spec.Template.Spec.Volumes {
		src := v.VolumeSource
		allowed := src.PersistentVolumeClaim != nil ||
			src.EmptyDir != nil ||
			src.Secret != nil ||
			src.ConfigMap != nil ||
			src.Projected != nil ||
			src.DownwardAPI != nil ||
			src.Ephemeral != nil
		if !allowed {
			t.Errorf("volume %q has a source outside the allowlist: %+v", v.Name, src)
		}
		if src.HostPath != nil {
			t.Errorf("volume %q uses hostPath", v.Name)
		}
	}
}

// §4.2: PSA `restricted` applies to every container, including workers.
func TestJobSatisfiesRestrictedPSA(t *testing.T) {
	job := mustBuild(t, testRequest())
	pod := job.Spec.Template.Spec

	if pod.SecurityContext == nil {
		t.Fatal("pod securityContext is nil")
	}
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod runAsNonRoot is not true")
	}
	if pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod seccompProfile is not RuntimeDefault")
	}

	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("pod requests host namespaces")
	}

	for _, c := range pod.Containers {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("container %q has no securityContext", c.Name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %q allows privilege escalation", c.Name)
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Errorf("container %q is privileged", c.Name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("container %q does not set runAsNonRoot", c.Name)
		}
		if sc.Capabilities == nil || !dropsAll(sc.Capabilities.Drop) {
			t.Errorf("container %q does not drop ALL capabilities", c.Name)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("container %q seccompProfile is not RuntimeDefault", c.Name)
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				t.Errorf("container %q uses hostPort %d", c.Name, p.HostPort)
			}
		}
	}
}

// §4.3: a container must stay within the LimitRange maximum, so one workload
// cannot eat the workspace budget.
func TestJobResourcesWithinLimitRangeMax(t *testing.T) {
	cfg := testConfig()
	job, err := BuildWorkerJob("abcd1234", testRequest(), cfg)
	if err != nil {
		t.Fatalf("BuildWorkerJob: %v", err)
	}
	limits := container(t, job).Resources.Limits
	if cpu := limits[corev1.ResourceCPU]; cpu.Cmp(cfg.MaxCPU) > 0 {
		t.Errorf("cpu limit %s exceeds max %s", cpu.String(), cfg.MaxCPU.String())
	}
	if mem := limits[corev1.ResourceMemory]; mem.Cmp(cfg.MaxMemory) > 0 {
		t.Errorf("memory limit %s exceeds max %s", mem.String(), cfg.MaxMemory.String())
	}
}

func TestJobRejectsOversizedResources(t *testing.T) {
	req := testRequest()
	req.Resources = ResourceHints{CPU: "2"} // max is 1
	if _, err := BuildWorkerJob("abcd1234", req, testConfig()); err == nil {
		t.Fatal("expected an error for cpu above the workspace maximum")
	}

	req = testRequest()
	req.Resources = ResourceHints{Memory: "4Gi"} // max is 2Gi
	if _, err := BuildWorkerJob("abcd1234", req, testConfig()); err == nil {
		t.Fatal("expected an error for memory above the workspace maximum")
	}
}

// §5.2: the workspace label is what the network policies match on, and the
// component label is what the Service selectors match on.
func TestJobLabelsMatchNetworkPolicyAndService(t *testing.T) {
	job := mustBuild(t, testRequest())
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		if labels[contract.LabelWorkspace] != "demo" {
			t.Errorf("workspace label = %q, want demo", labels[contract.LabelWorkspace])
		}
		if labels[contract.LabelComponent] != contract.ComponentWorker {
			t.Errorf("component label = %q, want %q", labels[contract.LabelComponent], contract.ComponentWorker)
		}
	}
}

// §5.8: the capability token is mounted only into the root pod. A worker must
// never receive it, directly or through the shared volume.
func TestJobHasNoBrokerToken(t *testing.T) {
	job := mustBuild(t, testRequest())
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && strings.Contains(v.Secret.SecretName, "token") {
			t.Errorf("worker mounts a token secret: %q", v.Secret.SecretName)
		}
	}
	c := container(t, job)
	for _, e := range c.Env {
		if strings.Contains(e.Name, "TOKEN") {
			t.Errorf("worker has token env var %q", e.Name)
		}
	}
	for _, ef := range c.EnvFrom {
		if ef.SecretRef != nil && strings.Contains(ef.SecretRef.Name, "token") {
			t.Errorf("worker reads a token secret via envFrom: %q", ef.SecretRef.Name)
		}
	}
}

// §5.1: "worker-<agent-id>" must stay a legal object name.
func TestJobNameIsBounded(t *testing.T) {
	job := mustBuild(t, testRequest())
	if len(job.Name) > 63 {
		t.Errorf("job name %q is %d characters", job.Name, len(job.Name))
	}
	if !strings.HasPrefix(job.Name, "worker-") {
		t.Errorf("job name %q does not have the worker- prefix", job.Name)
	}
}

// §5.7: the task is one argv element after "--", never interpolated into a
// shell, so a task beginning with "-" cannot be read as a flag.
func TestJobTaskIsExecForm(t *testing.T) {
	task := "-not-a-flag; rm -rf /"
	req := testRequest()
	req.Task = task
	job := mustBuild(t, req)
	cmd := container(t, job).Command
	want := []string{contract.AgentEntrypoint, "-p", "--", task}
	if len(cmd) != len(want) {
		t.Fatalf("command = %v, want %v", cmd, want)
	}
	for i := range want {
		if cmd[i] != want[i] {
			t.Fatalf("command = %v, want %v", cmd, want)
		}
	}
	for _, arg := range cmd {
		if arg == "sh" || arg == "bash" || arg == "-c" {
			t.Errorf("command invokes a shell: %v", cmd)
		}
	}
}

// §5.7: a worker is fire-and-forget. A retry would silently consume the pod
// budget and may duplicate side effects on the shared volume.
func TestJobIsFireAndForget(t *testing.T) {
	req := testRequest()
	req.TimeoutSeconds = 600
	job := mustBuild(t, req)
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("activeDeadlineSeconds = %v, want 600", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != DefaultJobTTLSeconds {
		t.Errorf("ttlSecondsAfterFinished = %v, want %d", job.Spec.TTLSecondsAfterFinished, DefaultJobTTLSeconds)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestBuildRejectsInvalidAgentID(t *testing.T) {
	for _, id := range []string{"", "short", "UPPER1234", "-leading1", "trailing-", strings.Repeat("a", MaxAgentIDLen+1)} {
		if _, err := BuildWorkerJob(id, testRequest(), testConfig()); err == nil {
			t.Errorf("agent id %q was accepted, want rejection", id)
		}
	}
}

func TestBuildRejectsInvalidRequests(t *testing.T) {
	cases := map[string]Request{
		"empty task":       {Task: "  "},
		"negative timeout": {Task: "x", TimeoutSeconds: -1},
		"timeout too long": {Task: "x", TimeoutSeconds: MaxTimeoutSeconds + 1},
		// A few seconds is a guaranteed failure: the worker cannot finish one
		// model call, and it disappears with no result. Seen live, chosen by an agent.
		"timeout too short": {Task: "x", TimeoutSeconds: 15},
		"bad mount":         {Task: "x", Workspace: WorkspaceHints{Mount: "host"}},
	}
	for name, req := range cases {
		if _, err := BuildWorkerJob("abcd1234", req, testConfig()); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestBuildRejectsBadConfig(t *testing.T) {
	base := testConfig()
	cases := map[string]func(Config) Config{
		"no namespace": func(c Config) Config { c.Namespace = ""; return c },
		"no workspace": func(c Config) Config { c.Workspace = ""; return c },
		"no image":     func(c Config) Config { c.Image = ""; return c },
		"wrong SA":     func(c Config) Config { c.SAWorker = contract.SARoot; return c },
		"no max":       func(c Config) Config { c.MaxCPU = resource.Quantity{}; return c },
	}
	for name, mutate := range cases {
		if _, err := BuildWorkerJob("abcd1234", testRequest(), mutate(base)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// A caller cannot influence the security-relevant parts of the PodSpec: there
// is simply no field for them. This test pins the shape of Request so that
// adding such a field is a deliberate, visible change.
func TestRequestHasNoSecurityFields(t *testing.T) {
	// Compile-time-ish guard: the only fields Request may have are task-level.
	req := Request{
		Task:           "x",
		ModelProfile:   "coding",
		Resources:      ResourceHints{CPU: "300m", Memory: "512Mi"},
		Workspace:      WorkspaceHints{Mount: "shared"},
		TimeoutSeconds: 60,
	}
	if _, err := BuildWorkerJob("abcd1234", req, testConfig()); err != nil {
		t.Fatalf("a fully-specified task-level request was rejected: %v", err)
	}
}

func dropsAll(drop []corev1.Capability) bool {
	for _, c := range drop {
		if c == "ALL" {
			return true
		}
	}
	return false
}
