package kubernetes

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickemma/plinth/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testManifest() manifest.Manifest {
	return manifest.Manifest{
		Name: "tessera-gateway", Image: "ghcr.io/nickemma/tessera:v0.4.1", Port: 8080, Replicas: 3,
		Env: map[string]string{"LOG_LEVEL": "info"}, Secrets: []string{"DATABASE_URL"},
		Resources: manifest.Resources{CPU: "500m", Memory: "512Mi"},
	}
}

func TestEnsureCreatesGoldenPathResources(t *testing.T) {
	client := fake.NewSimpleClientset()
	target := New(client, "plinth-test").WithOwnerReference(metav1.OwnerReference{APIVersion: "plinth.dev/v1alpha1", Kind: "PlinthService", Name: "tessera-gateway", UID: "uid-1"})
	result, err := target.Ensure(context.Background(), testManifest(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 9 {
		t.Fatalf("expected nine resources, got %d", len(result.Resources))
	}
	deployment, err := client.AppsV1().Deployments("plinth-test").Get(context.Background(), "tessera-gateway", metav1GetOptions())
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 3 {
		t.Fatalf("unexpected replicas: %v", deployment.Spec.Replicas)
	}
	if deployment.APIVersion != "apps/v1" || deployment.Kind != "Deployment" {
		t.Fatalf("expected server-side apply metadata, got apiVersion=%q kind=%q", deployment.APIVersion, deployment.Kind)
	}
	if deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("expected local/dev images to use IfNotPresent, got %q", deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
	}
	annotations := deployment.Spec.Template.Annotations
	if annotations["prometheus.io/scrape"] != "true" || annotations["prometheus.io/path"] != "/metrics" || annotations["prometheus.io/port"] != "8080" || annotations["plinth.dev/logs"] != "structured" {
		t.Fatalf("expected pod metrics/logging annotations, got %v", annotations)
	}
	if deployment.Spec.Template.Spec.SecurityContext.FSGroup == nil || *deployment.Spec.Template.Spec.SecurityContext.FSGroup != 65532 || len(deployment.Spec.Template.Spec.Volumes) != 1 || deployment.Spec.Template.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("expected writable ephemeral runtime volume with non-root group, got security=%+v volumes=%+v", deployment.Spec.Template.Spec.SecurityContext, deployment.Spec.Template.Spec.Volumes)
	}
	for _, labels := range []map[string]string{
		deployment.Labels,
		mustGetService(t, client).Labels,
		mustGetIngress(t, client).Labels,
		mustGetConfigMap(t, client).Labels,
		mustGetPDB(t, client).Labels,
		mustGetNetworkPolicy(t, client).Labels,
	} {
		if labels["plinth.dev/revision"] != "1" {
			t.Fatalf("expected all managed resources to carry revision 1, got %v", labels)
		}
	}
	if result.Ready {
		t.Fatal("new deployment should not be reported ready before available replicas are observed")
	}
	if len(deployment.OwnerReferences) != 1 || deployment.OwnerReferences[0].Name != "tessera-gateway" {
		t.Fatalf("expected owner reference, got %+v", deployment.OwnerReferences)
	}
	networkPolicy, err := client.NetworkingV1().NetworkPolicies("plinth-test").Get(context.Background(), "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(networkPolicy.Spec.Ingress) != 1 || len(networkPolicy.Spec.Egress) != 1 || len(networkPolicy.Spec.Egress[0].Ports) != 2 {
		t.Fatalf("expected default-deny policy with platform ingress and DNS egress, got %+v", networkPolicy.Spec)
	}
	if deployment.Spec.Template.Spec.Containers[0].SecurityContext == nil || deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem == nil || !*deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("expected read-only root filesystem")
	}
	deployment.Status.AvailableReplicas = 3
	deployment.Status.UpdatedReplicas = 3
	deployment.Status.ReadyReplicas = 3
	deployment.Status.ObservedGeneration = deployment.Generation
	if _, err := client.AppsV1().Deployments("plinth-test").Update(context.Background(), deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err = target.Ensure(context.Background(), testManifest(), 1)
	if err != nil || !result.Ready {
		t.Fatalf("expected observed deployment to become ready: result=%+v err=%v", result, err)
	}
}

func TestEnsureRejectsReadyCountsFromAnOlderDeploymentGeneration(t *testing.T) {
	client := fake.NewSimpleClientset()
	target := New(client, "plinth-test")
	ctx := context.Background()
	m := testManifest()
	if _, err := target.Ensure(ctx, m, 1); err != nil {
		t.Fatal(err)
	}
	deployment, err := client.AppsV1().Deployments("plinth-test").Get(ctx, m.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployment.Status.AvailableReplicas = 3
	deployment.Status.UpdatedReplicas = 3
	deployment.Status.ReadyReplicas = 3
	if deployment.Generation == 0 {
		deployment.Status.ObservedGeneration = -1
	} else {
		deployment.Status.ObservedGeneration = deployment.Generation - 1
	}
	if _, err := client.AppsV1().Deployments("plinth-test").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := target.Ensure(ctx, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready {
		t.Fatal("expected stale deployment status to remain unready")
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	target := New(fake.NewSimpleClientset(), "plinth-test")
	ctx := context.Background()
	if _, err := target.Ensure(ctx, testManifest(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Ensure(ctx, testManifest(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestErrorRateReadsPrometheusInstantQuery(t *testing.T) {
	var query string
	target := New(fake.NewSimpleClientset(), "plinth-test").WithPrometheusURL("http://prometheus.test")
	target.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/query" {
			t.Fatalf("unexpected Prometheus path %q", request.URL.Path)
		}
		query = request.URL.Query().Get("query")
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewBufferString(`{"status":"success","data":{"result":[{"value":["0","0.04"]}]}}`)), Header: make(http.Header), Request: request}, nil
	})}
	rate, err := target.ErrorRate(context.Background(), testManifest())
	if err != nil || rate != 0.04 {
		t.Fatalf("expected Prometheus error rate 0.04, got %v err=%v", rate, err)
	}
	if !strings.Contains(query, `namespace="plinth-test"`) || !strings.Contains(query, `service="tessera-gateway"`) {
		t.Fatalf("query did not identify service and namespace: %q", query)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestErrorRateFailsClosedWithoutPrometheusURL(t *testing.T) {
	_, err := New(fake.NewSimpleClientset(), "plinth-test").ErrorRate(context.Background(), testManifest())
	if err == nil {
		t.Fatal("expected progressive rollout metrics to require a Prometheus URL")
	}
}

func TestErrorRateAcceptsLiteralPrometheusQuery(t *testing.T) {
	target := New(fake.NewSimpleClientset(), "plinth-test").WithPrometheusURL("http://prometheus.test").WithPrometheusQuery("vector(0.5)")
	target.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("query") != "vector(0.5)" {
			t.Fatalf("unexpected literal query %q", request.URL.Query().Get("query"))
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewBufferString(`{"status":"success","data":{"result":[{"value":["0","0.5"]}]}}`)), Header: make(http.Header), Request: request}, nil
	})}
	rate, err := target.ErrorRate(context.Background(), testManifest())
	if err != nil || rate != 0.5 {
		t.Fatalf("expected literal Prometheus query rate 0.5, got %v err=%v", rate, err)
	}
}

func TestEnsureAndDeleteHonorsManifestNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	target := New(client, "default")
	m := testManifest()
	m.Namespace = "plinth-payments"
	if _, err := target.Ensure(context.Background(), m, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments("plinth-payments").Get(context.Background(), m.Name, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := target.Delete(context.Background(), m.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments("plinth-payments").Get(context.Background(), m.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected namespaced deployment to be deleted, got %v", err)
	}
}

func TestStandaloneBackendDoesNotClaimOperatorOwnedResources(t *testing.T) {
	client := fake.NewSimpleClientset()
	m := testManifest()
	m.Name = "operator-service"
	operatorTarget := New(client, "plinth-test").WithOwnerReference(metav1.OwnerReference{APIVersion: "plinth.dev/v1alpha1", Kind: "PlinthService", Name: m.Name, UID: "uid-operator"})
	if _, err := operatorTarget.Ensure(context.Background(), m, 1); err != nil {
		t.Fatal(err)
	}
	standaloneTarget := New(client, "plinth-test")
	if _, err := standaloneTarget.Resources(context.Background(), m.Name); err == nil {
		t.Fatal("standalone backend should not discover operator-owned resources")
	}
	if err := standaloneTarget.Delete(context.Background(), m.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().Deployments("plinth-test").Get(context.Background(), m.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("standalone cleanup removed or lost operator resource: %v", err)
	}
}

func TestEnsureNamespaceCreatesAndPreservesManagedNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	target := New(client, "default")
	if err := target.EnsureNamespace(context.Background(), "plinth-payments"); err != nil {
		t.Fatal(err)
	}
	namespace, err := client.CoreV1().Namespaces().Get(context.Background(), "plinth-payments", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if namespace.Labels["app.kubernetes.io/managed-by"] != "plinth" {
		t.Fatalf("expected managed namespace label, got %v", namespace.Labels)
	}
	namespace.Labels["owner"] = "payments"
	if _, err := client.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := target.EnsureNamespace(context.Background(), "plinth-payments"); err != nil {
		t.Fatal(err)
	}
	namespace, err = client.CoreV1().Namespaces().Get(context.Background(), "plinth-payments", metav1.GetOptions{})
	if err != nil || namespace.Labels["owner"] != "payments" {
		t.Fatalf("expected existing labels to survive: namespace=%v err=%v", namespace.Labels, err)
	}
}

func TestWatchEnqueuesManagedDeploymentChanges(t *testing.T) {
	client := fake.NewSimpleClientset()
	target := New(client, "plinth-test")
	ctx, cancel := context.WithCancel(context.Background())
	deployed := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- target.Watch(ctx, func(name string) { deployed <- name }) }()
	time.Sleep(25 * time.Millisecond)
	_, err := client.AppsV1().Deployments("plinth-test").Create(ctx, deploymentFor(testManifest(), 1, "plinth-test"), metav1.CreateOptions{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case name := <-deployed:
		if name != "tessera-gateway" {
			t.Fatalf("unexpected queued name %q", name)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for deployment watch event")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deployment watch did not stop after context cancellation")
	}
}

// Keep the test call concise without importing a second metav1 alias in the
// assertions above.
func metav1GetOptions() metav1.GetOptions { return metav1.GetOptions{} }

func mustGetService(t *testing.T, client *fake.Clientset) *corev1.Service {
	t.Helper()
	value, err := client.CoreV1().Services("plinth-test").Get(context.Background(), "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustGetIngress(t *testing.T, client *fake.Clientset) *networkingv1.Ingress {
	t.Helper()
	value, err := client.NetworkingV1().Ingresses("plinth-test").Get(context.Background(), "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustGetConfigMap(t *testing.T, client *fake.Clientset) *corev1.ConfigMap {
	t.Helper()
	value, err := client.CoreV1().ConfigMaps("plinth-test").Get(context.Background(), "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustGetPDB(t *testing.T, client *fake.Clientset) *policyv1.PodDisruptionBudget {
	t.Helper()
	value, err := client.PolicyV1().PodDisruptionBudgets("plinth-test").Get(context.Background(), "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustGetNetworkPolicy(t *testing.T, client *fake.Clientset) *networkingv1.NetworkPolicy {
	t.Helper()
	value, err := client.NetworkingV1().NetworkPolicies("plinth-test").Get(context.Background(), "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
