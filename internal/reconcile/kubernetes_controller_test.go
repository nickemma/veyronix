package reconcile

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nickemma/plinth/internal/backend"
	kubebackend "github.com/nickemma/plinth/internal/backend/kubernetes"
	"github.com/nickemma/plinth/internal/manifest"
	"github.com/nickemma/plinth/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func newKubernetesController(t *testing.T) (*Controller, kubernetes.Interface) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset()
	return NewController(store, kubebackend.New(client, "plinth-test")), client
}

func TestControllerWithKubernetesBackendWaitsForReadinessAndRepairsDrift(t *testing.T) {
	controller, client := newKubernetesController(t)
	ctx := context.Background()
	m := testManifest("ghcr.io/example/tessera:v1")
	m.Namespace = "plinth-test"
	m.Replicas = 2

	service, err := controller.Apply(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if service.Phase != state.PhaseReconciling {
		t.Fatalf("expected controller to wait for Kubernetes readiness, got %+v", service)
	}
	deployment, err := client.AppsV1().Deployments("plinth-test").Get(ctx, m.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Status.ReadyReplicas != 0 {
		t.Fatalf("fake Kubernetes deployment should start unready, got status %+v", deployment.Status)
	}

	deployment.Status.UpdatedReplicas = 2
	deployment.Status.ReadyReplicas = 2
	deployment.Status.AvailableReplicas = 2
	deployment.Status.ObservedGeneration = deployment.Generation
	if _, err := client.AppsV1().Deployments("plinth-test").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	service, err = controller.Reconcile(ctx, m.Name)
	if err != nil || service.Phase != state.PhaseReady || service.ActiveRevision != 1 {
		t.Fatalf("expected ready service after observed Kubernetes status, service=%+v err=%v", service, err)
	}

	service, err = controller.Drift(ctx, m.Name, "Deployment")
	if err != nil {
		t.Fatal(err)
	}
	if service.Phase != state.PhaseReconciling {
		t.Fatalf("expected drift repair to wait for replacement readiness, got %+v", service)
	}
	if _, err := client.AppsV1().Deployments("plinth-test").Get(ctx, m.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("drift repair did not recreate deployment: %v", err)
	}

	deployment, err = client.AppsV1().Deployments("plinth-test").Get(ctx, m.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployment.Status.UpdatedReplicas = 2
	deployment.Status.ReadyReplicas = 2
	deployment.Status.AvailableReplicas = 2
	deployment.Status.ObservedGeneration = deployment.Generation
	if _, err := client.AppsV1().Deployments("plinth-test").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	service, err = controller.Reconcile(ctx, m.Name)
	if err != nil || service.Phase != state.PhaseReady {
		t.Fatalf("expected repaired deployment to converge, service=%+v err=%v", service, err)
	}
}

type readyKubernetesBackend struct {
	target    *kubebackend.Backend
	client    kubernetes.Interface
	namespace string
	steps     []int
}

func (b *readyKubernetesBackend) Ensure(ctx context.Context, m manifest.Manifest, revision int) (backend.ApplyResult, error) {
	b.steps = append(b.steps, m.Replicas)
	result, err := b.target.Ensure(ctx, m, revision)
	if err != nil {
		return result, err
	}
	deployment, err := b.client.AppsV1().Deployments(b.namespace).Get(ctx, m.Name, metav1.GetOptions{})
	if err != nil {
		return result, err
	}
	deployment.Status.UpdatedReplicas = int32(m.Replicas)
	deployment.Status.ReadyReplicas = int32(m.Replicas)
	deployment.Status.AvailableReplicas = int32(m.Replicas)
	deployment.Status.ObservedGeneration = deployment.Generation
	if _, err := b.client.AppsV1().Deployments(b.namespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		return result, err
	}
	result.Ready = true
	return result, nil
}

func (b *readyKubernetesBackend) Delete(ctx context.Context, name string) error {
	return b.target.Delete(ctx, name)
}

func (b *readyKubernetesBackend) DeleteResource(ctx context.Context, name, kind string) error {
	return b.target.DeleteResource(ctx, name, kind)
}

func (b *readyKubernetesBackend) Resources(ctx context.Context, name string) ([]backend.Resource, error) {
	return b.target.Resources(ctx, name)
}

func TestControllerWithKubernetesBackendAppliesProgressiveRollout(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset()
	target := &readyKubernetesBackend{target: kubebackend.New(client, "plinth-test"), client: client, namespace: "plinth-test"}
	controller := NewController(store, target)
	m := testManifest("ghcr.io/example/tessera:v2")
	m.Namespace = "plinth-test"
	m.Replicas = 10
	m.Rollout = manifest.Rollout{Enabled: true, Steps: []int{10, 50, 100}, MaxErrorRate: 0.05}

	service, err := controller.Apply(context.Background(), m)
	if err != nil || service.Phase != state.PhaseReady {
		t.Fatalf("expected progressive rollout to converge, service=%+v err=%v", service, err)
	}
	if got, want := fmt.Sprint(target.steps), "[1 5 10]"; got != want {
		t.Fatalf("expected Kubernetes backend to receive every rollout stage, got %s", got)
	}
	if len(service.Observed) != 9 {
		t.Fatalf("expected complete golden path after rollout, got %v", service.Observed)
	}
}

var _ backend.Backend = (*readyKubernetesBackend)(nil)
