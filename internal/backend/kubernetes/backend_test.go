package kubernetes

import (
	"context"
	"testing"

	"github.com/nickemma/plinth/internal/manifest"
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
	target := New(client, "plinth-test")
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
	if deployment.Spec.Template.Spec.Containers[0].SecurityContext == nil || deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem == nil || !*deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("expected read-only root filesystem")
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

// Keep the test call concise without importing a second metav1 alias in the
// assertions above.
func metav1GetOptions() metav1.GetOptions { return metav1.GetOptions{} }
