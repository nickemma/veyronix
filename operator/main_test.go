package main

import (
	"context"
	"testing"

	kubeBackend "github.com/nickemma/plinth/internal/backend/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestOperatorAddsFinalizerReportsReadinessAndOwnsResources(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "plinth.dev/v1alpha1",
		"kind":       "PlinthService",
		"metadata": map[string]any{
			"name": "tessera-gateway", "namespace": "plinth-test", "uid": "service-uid",
		},
		"spec": map[string]any{
			"image": "ghcr.io/example/tessera:v1", "port": int64(8080), "replicas": int64(1),
			"resources": map[string]any{"cpu": "100m", "memory": "128Mi"},
		},
	}}
	object.SetGroupVersionKind(schemaForTest())
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme(), object)
	typedClient := k8sfake.NewSimpleClientset()
	operator := &operator{client: dynamicClient, backend: func(namespace string) *kubeBackend.Backend { return kubeBackend.New(typedClient, namespace) }}

	ctx := context.Background()
	if err := operator.reconcile(ctx, object); err != nil {
		t.Fatal(err)
	}
	deployment, err := typedClient.AppsV1().Deployments("plinth-test").Get(ctx, "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deployment.OwnerReferences) != 1 || deployment.OwnerReferences[0].UID != "service-uid" {
		t.Fatalf("expected CRD owner reference, got %+v", deployment.OwnerReferences)
	}
	current, err := dynamicClient.Resource(serviceResource).Namespace("plinth-test").Get(ctx, "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(current.GetFinalizers(), finalizer) {
		t.Fatalf("expected finalizer, got %v", current.GetFinalizers())
	}
	phase, _, err := unstructured.NestedString(current.Object, "status", "phase")
	if err != nil || phase != "Reconciling" {
		t.Fatalf("expected Reconciling status before available replicas, got %q err=%v", phase, err)
	}

	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.ReadyReplicas = 1
	deployment.Status.ObservedGeneration = deployment.Generation
	if _, err := typedClient.AppsV1().Deployments("plinth-test").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := operator.reconcile(ctx, object); err != nil {
		t.Fatal(err)
	}
	current, err = dynamicClient.Resource(serviceResource).Namespace("plinth-test").Get(ctx, "tessera-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, err = unstructured.NestedString(current.Object, "status", "phase")
	if err != nil || phase != "Ready" {
		t.Fatalf("expected Ready status after available replicas, got %q err=%v", phase, err)
	}
	conditions, found, err := unstructured.NestedSlice(current.Object, "status", "conditions")
	if err != nil || !found || len(conditions) != 1 {
		t.Fatalf("expected ready condition, got found=%v conditions=%v err=%v", found, conditions, err)
	}
}

func schemaForTest() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "plinth.dev", Version: "v1alpha1", Kind: "PlinthService"}
}
