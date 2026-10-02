package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gobackto-work/scarab/internal/agentpod"
	"github.com/gobackto-work/scarab/internal/contract"
)

// These tests use the fake clientset, so they exercise the translation between
// the broker's intent and the Kubernetes objects it is allowed to touch, without
// a cluster.

const (
	kubeWorkspace = "demo"
	kubeNamespace = "ws-demo"
	kubeImage     = "ghcr.io/gobackto-work/scarab-agent:test"
)

func kubeSpawner(budget int) (*KubeSpawner, *fake.Clientset) {
	client := fake.NewSimpleClientset()
	cfg := agentpod.DefaultConfig(kubeNamespace, kubeWorkspace, kubeImage)
	return NewKubeSpawner(client, cfg, budget, resource.MustParse("1Gi")), client
}

func TestKubeSpawnCreatesJobInTheFixedNamespace(t *testing.T) {
	sp, client := kubeSpawner(6)
	if err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{Task: "x"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	job, err := client.BatchV1().Jobs(kubeNamespace).Get(context.Background(), "worker-abcd1234abcd1234", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job was not created in %s: %v", kubeNamespace, err)
	}
	if job.Namespace != kubeNamespace {
		t.Errorf("namespace = %q, want %q", job.Namespace, kubeNamespace)
	}
	if job.Labels[contract.LabelWorkspace] != kubeWorkspace {
		t.Errorf("workspace label = %q", job.Labels[contract.LabelWorkspace])
	}
	if job.Labels[contract.LabelComponent] != contract.ComponentWorker {
		t.Errorf("component label = %q", job.Labels[contract.LabelComponent])
	}
	if job.Annotations[contract.AnnotationTask] != "x" {
		t.Errorf("task annotation = %q", job.Annotations[contract.AnnotationTask])
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != contract.SAWorker {
		t.Errorf("serviceAccountName = %q, want %q", got, contract.SAWorker)
	}
}

// A request cannot move the broker into another namespace: the namespace is
// fixed at construction.
func TestKubeSpawnIsScopedToItsNamespace(t *testing.T) {
	sp, client := kubeSpawner(6)
	if err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{Task: "x"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	jobs, err := client.BatchV1().Jobs("ws-other").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a job appeared in another namespace: %v", jobs.Items)
	}
}

func TestKubeSpawnRejectsOversizedResources(t *testing.T) {
	sp, _ := kubeSpawner(6)
	err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{
		Task:      "x",
		Resources: agentpod.ResourceHints{CPU: "2"}, // max is 1
	})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("err = %v, want a 400 *Error", err)
	}
	if apiErr.Code != CodeInvalidRequest {
		t.Errorf("code = %q, want %q", apiErr.Code, CodeInvalidRequest)
	}
}

// The pod budget is the broker's own accounting, because its Role forbids
// reading the ResourceQuota.
func TestKubeSpawnRefusesWhenBudgetIsFull(t *testing.T) {
	sp, client := kubeSpawner(2)
	for i := 0; i < 2; i++ {
		_, err := client.CoreV1().Pods(kubeNamespace).Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p" + string(rune('a'+i)), Namespace: kubeNamespace},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("seed pod: %v", err)
		}
	}
	err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{Task: "x"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeQuotaExceeded {
		t.Fatalf("err = %v, want a quota_exceeded *Error", err)
	}
	if apiErr.Status != 409 {
		t.Errorf("status = %d, want 409", apiErr.Status)
	}
}

// Finished pods do not count against the budget.
func TestKubeBudgetIgnoresFinishedPods(t *testing.T) {
	sp, client := kubeSpawner(1)
	_, err := client.CoreV1().Pods(kubeNamespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: kubeNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed pod: %v", err)
	}
	if err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{Task: "x"}); err != nil {
		t.Fatalf("Spawn should have been allowed: %v", err)
	}
}

func TestKubeGetAndStopUnknownAgent(t *testing.T) {
	sp, _ := kubeSpawner(6)
	if _, err := sp.Get(context.Background(), "deadbeefdeadbeef"); !isNotFound(err) {
		t.Errorf("Get err = %v, want 404", err)
	}
	if err := sp.Stop(context.Background(), "deadbeefdeadbeef"); !isNotFound(err) {
		t.Errorf("Stop err = %v, want 404", err)
	}
}

func TestKubeStopDeletesTheJob(t *testing.T) {
	sp, client := kubeSpawner(6)
	id := "abcd1234abcd1234"
	if err := sp.Spawn(context.Background(), id, agentpod.Request{Task: "x"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := sp.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := client.BatchV1().Jobs(kubeNamespace).Get(context.Background(), "worker-"+id, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("job still present after Stop: %v", err)
	}
}

func TestKubeListReportsWorkers(t *testing.T) {
	sp, _ := kubeSpawner(6)
	if err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{Task: "inspect"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	agents, err := sp.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %v, want one", agents)
	}
	if agents[0].ID != "abcd1234abcd1234" {
		t.Errorf("id = %q", agents[0].ID)
	}
	if agents[0].Task != "inspect" {
		t.Errorf("task = %q", agents[0].Task)
	}
	if agents[0].State != StatePending {
		t.Errorf("state = %q, want pending", agents[0].State)
	}
}

// Memory requests, not the pod count, are the dimension that binds concurrency, so
// the broker pre-flights them: the agent gets the stable §6.5 error instead of a
// raw admission failure from the API server.
func TestKubeSpawnRefusesWhenMemoryBudgetIsExhausted(t *testing.T) {
	client := fake.NewSimpleClientset()
	cfg := agentpod.DefaultConfig(kubeNamespace, kubeWorkspace, kubeImage)
	sp := NewKubeSpawner(client, cfg, 6, resource.MustParse("1Gi"))

	// Four pods at 256Mi requests fill the 1Gi budget, while the pod count (6)
	// still has room — so only the memory check can reject this spawn.
	for i := 0; i < 4; i++ {
		_, err := client.CoreV1().Pods(kubeNamespace).Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("seed-%d", i), Namespace: kubeNamespace},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "worker",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("seed pod: %v", err)
		}
	}

	err := sp.Spawn(context.Background(), "abcd1234abcd1234", agentpod.Request{Task: "x"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeQuotaExceeded {
		t.Fatalf("err = %v, want a quota_exceeded *Error", err)
	}
	if !strings.Contains(apiErr.Message, "memory") {
		t.Errorf("message = %q, want it to mention memory", apiErr.Message)
	}
}

func isNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == 404
}

// A deadline failure must name the deadline. The raw Job message is "Job was
// active longer than specified deadline", which does not tell the agent how long
// it allowed -- and that is exactly what it needs to fix the request.
func TestAgentFromJobNamesADeadlineFailure(t *testing.T) {
	deadline := int64(15)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-abc"},
		Spec:       batchv1.JobSpec{ActiveDeadlineSeconds: &deadline},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:    batchv1.JobFailed,
			Status:  corev1.ConditionTrue,
			Reason:  "DeadlineExceeded",
			Message: "Job was active longer than specified deadline",
		}}},
	}

	agent := agentFromJob(job)
	if agent.State != StateFailed {
		t.Errorf("state = %q, want failed", agent.State)
	}
	if !strings.Contains(agent.Message, "timed out after 15s") {
		t.Errorf("message = %q, want it to name the deadline", agent.Message)
	}
	if agent.ID != "abc" {
		t.Errorf("id = %q", agent.ID)
	}
}
