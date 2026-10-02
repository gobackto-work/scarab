package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/gobackto-work/scarab/internal/agentpod"
	"github.com/gobackto-work/scarab/internal/contract"
)

// KubeSpawner is the real Spawner. It creates worker Jobs in exactly one
// namespace, the one it was constructed with.
//
// It holds no workspace identifier from the request path: the namespace is fixed
// at construction from the broker's own configuration, which is derived from the
// capability token by the platform. A request can therefore never influence
// which workspace it acts as.
type KubeSpawner struct {
	client    kubernetes.Interface
	cfg       agentpod.Config
	podBudget int
	// memoryBudget mirrors the quota's requests.memory. Memory requests, not the
	// pod count, are the dimension that actually binds concurrency (handoff §4.3),
	// so this is the pre-flight that matters. Zero disables the check.
	memoryBudget resource.Quantity
	now          func() time.Time
}

// NewKubeSpawner returns a Spawner scoped to cfg.Namespace. memoryBudget is the
// workspace's requests.memory; a zero quantity disables the memory pre-flight.
func NewKubeSpawner(client kubernetes.Interface, cfg agentpod.Config, podBudget int, memoryBudget resource.Quantity) *KubeSpawner {
	return &KubeSpawner{
		client:       client,
		cfg:          cfg,
		podBudget:    podBudget,
		memoryBudget: memoryBudget,
		now:          time.Now,
	}
}

func jobName(agentID string) string { return "worker-" + agentID }

// Spawn builds and creates the worker Job.
//
// The PodSpec comes from internal/agentpod, never from the request, so there is
// no path by which an agent-supplied ServiceAccount, volume, securityContext,
// node selector or label reaches the API server.
func (k *KubeSpawner) Spawn(ctx context.Context, agentID string, req agentpod.Request) error {
	job, err := agentpod.BuildWorkerJob(agentID, req, k.cfg)
	if err != nil {
		// A rejected request is the agent's fault, not an internal error.
		return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: err.Error()}
	}

	// Pre-flight the budget so the common case produces a useful message. This is
	// advisory: the ResourceQuota is the real enforcement, and a concurrent spawn
	// can still be refused by the API server, which mapKubeError handles.
	if err := k.checkBudget(ctx, job); err != nil {
		return err
	}

	if _, err := k.client.BatchV1().Jobs(k.cfg.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return k.mapKubeError(err)
	}
	return nil
}

// List returns every worker in this workspace.
func (k *KubeSpawner) List(ctx context.Context) ([]Agent, error) {
	jobs, err := k.client.BatchV1().Jobs(k.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: contract.LabelComponent + "=" + contract.ComponentWorker,
	})
	if err != nil {
		return nil, k.mapKubeError(err)
	}
	agents := make([]Agent, 0, len(jobs.Items))
	for i := range jobs.Items {
		agents = append(agents, agentFromJob(&jobs.Items[i]))
	}
	return agents, nil
}

// Get returns one worker.
func (k *KubeSpawner) Get(ctx context.Context, agentID string) (Agent, error) {
	job, err := k.client.BatchV1().Jobs(k.cfg.Namespace).Get(ctx, jobName(agentID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Agent{}, errNotFound(agentID)
		}
		return Agent{}, k.mapKubeError(err)
	}
	return agentFromJob(job), nil
}

// Logs streams the worker pod's output.
func (k *KubeSpawner) Logs(ctx context.Context, agentID string) (io.ReadCloser, error) {
	pod, err := k.podForJob(ctx, jobName(agentID))
	if err != nil {
		return nil, err
	}
	rc, err := k.client.CoreV1().Pods(k.cfg.Namespace).
		GetLogs(pod, &corev1.PodLogOptions{}).
		Stream(ctx)
	if err != nil {
		return nil, k.mapKubeError(err)
	}
	return rc, nil
}

// Stop deletes the worker Job. Kubernetes garbage-collects its pods, so a single
// delete stops the worker and frees its share of the pod budget.
func (k *KubeSpawner) Stop(ctx context.Context, agentID string) error {
	policy := metav1.DeletePropagationBackground
	err := k.client.BatchV1().Jobs(k.cfg.Namespace).Delete(ctx, jobName(agentID), metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return errNotFound(agentID)
		}
		return k.mapKubeError(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (k *KubeSpawner) podForJob(ctx context.Context, name string) (string, error) {
	pods, err := k.client.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + name,
	})
	if err != nil {
		return "", k.mapKubeError(err)
	}
	if len(pods.Items) == 0 {
		return "", errNotFound(strings.TrimPrefix(name, "worker-"))
	}
	// A Job with backoffLimit 0 has at most one pod; if several exist, the
	// newest is the one that matters.
	newest := pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = pods.Items[i]
		}
	}
	return newest.Name, nil
}

// checkBudget refuses a spawn that would exceed the workspace's pod count or
// memory-request budget.
//
// The broker cannot read the ResourceQuota (its Role forbids it), so both numbers
// are supplied as configuration and must be kept in step with the quota. Memory is
// the dimension that binds first: with the §5.6/§5.7 sizes, the pod count allows
// five workers but requests.memory allows three.
//
// The running pods' own requests are summed from their PodSpecs, which the broker
// can read.
func (k *KubeSpawner) checkBudget(ctx context.Context, job *batchv1.Job) error {
	if k.podBudget <= 0 && k.memoryBudget.IsZero() {
		return nil
	}

	pods, err := k.client.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return k.mapKubeError(err)
	}

	active := 0
	memoryUsed := resource.Quantity{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		}
		active++
		for _, container := range pod.Spec.Containers {
			if request, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				memoryUsed.Add(request)
			}
		}
	}

	if k.podBudget > 0 && active+1 > k.podBudget {
		return &Error{
			Status:  http.StatusConflict,
			Code:    CodeQuotaExceeded,
			Message: fmt.Sprintf("workspace pod quota reached (%d/%d)", active, k.podBudget),
		}
	}

	if !k.memoryBudget.IsZero() {
		total := memoryUsed.DeepCopy()
		total.Add(jobMemoryRequests(job))
		if total.Cmp(k.memoryBudget) > 0 {
			return &Error{
				Status: http.StatusConflict,
				Code:   CodeQuotaExceeded,
				Message: fmt.Sprintf("workspace memory quota reached (%s of %s requested)",
					total.String(), k.memoryBudget.String()),
			}
		}
	}
	return nil
}

// jobMemoryRequests sums the memory requests of a Job's containers.
func jobMemoryRequests(job *batchv1.Job) resource.Quantity {
	total := resource.Quantity{}
	for _, container := range job.Spec.Template.Spec.Containers {
		if request, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
			total.Add(request)
		}
	}
	return total
}

// mapKubeError translates a Kubernetes error into an agent-visible *Error.
//
// A quota or admission rejection must read as an ordinary broker error so the
// agent can recover by stopping an idle worker (handoff §6.5). Everything else
// becomes a generic internal error, because a raw Kubernetes error can name
// resources the agent should not see.
func (k *KubeSpawner) mapKubeError(err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsForbidden(err) || apierrors.IsConflict(err) || isQuotaError(err) {
		return &Error{
			Status:  http.StatusConflict,
			Code:    CodeQuotaExceeded,
			Message: fmt.Sprintf("workspace quota reached (%d pods allowed)", k.podBudget),
		}
	}
	return err
}

func isQuotaError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "exceeded quota") || strings.Contains(msg, "forbidden: exceeded")
}

func agentFromJob(job *batchv1.Job) Agent {
	agent := Agent{
		ID:        strings.TrimPrefix(job.Name, "worker-"),
		Task:      job.Annotations[contract.AnnotationTask],
		State:     StatePending,
		CreatedAt: formatTime(job.CreationTimestamp.Time),
	}
	if job.Status.StartTime != nil {
		agent.StartedAt = formatTime(job.Status.StartTime.Time)
	}
	if job.Status.CompletionTime != nil {
		agent.FinishedAt = formatTime(job.Status.CompletionTime.Time)
	}
	for _, c := range job.Status.Conditions {
		switch {
		case c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue:
			agent.State = StateSucceeded
			agent.Message = c.Reason
		case c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue:
			agent.State = StateFailed
			// Name a deadline failure precisely: the raw message is "Job was active
			// longer than specified deadline", which does not tell the agent how
			// long it allowed, and that is exactly what it needs to fix the request.
			if c.Reason == "DeadlineExceeded" && job.Spec.ActiveDeadlineSeconds != nil {
				agent.Message = fmt.Sprintf("worker timed out after %ds", *job.Spec.ActiveDeadlineSeconds)
			} else {
				agent.Message = c.Message
			}
		}
	}
	if agent.State == StatePending && job.Status.Active > 0 {
		agent.State = StateRunning
	}
	return agent
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

var _ Spawner = (*KubeSpawner)(nil)

// ErrNoSpawner is returned by wiring code when no cluster client is available.
var ErrNoSpawner = errors.New("broker: no spawner configured")
