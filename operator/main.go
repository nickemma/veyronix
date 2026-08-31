package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nickemma/plinth/internal/backend"
	kubeBackend "github.com/nickemma/plinth/internal/backend/kubernetes"
	"github.com/nickemma/plinth/internal/manifest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var serviceResource = schema.GroupVersionResource{Group: "plinth.dev", Version: "v1alpha1", Resource: "plinthservices"}

const finalizer = "plinth.dev/finalizer"

func main() {
	kubeconfig := flag.String("kubeconfig", "", "Kubernetes kubeconfig path")
	flag.Parse()
	config, err := loadConfig(*kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	typed, err := k8sclient.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	backendFactory := func(namespace string) *kubeBackend.Backend {
		return kubeBackend.New(typed, namespace).WithPrometheusURL(os.Getenv("PLINTH_PROMETHEUS_URL")).WithPrometheusQuery(os.Getenv("PLINTH_PROMETHEUS_QUERY"))
	}
	operator := &operator{client: client, backend: backendFactory}
	log.Println("plinth operator started")
	operator.run(context.Background())
}

type operator struct {
	client  dynamic.Interface
	backend func(string) *kubeBackend.Backend
	mu      sync.Mutex
}

func (o *operator) run(ctx context.Context) {
	if err := o.reconcileAll(ctx); err != nil {
		log.Printf("operator reconcile: %v", err)
	}
	go o.watch(ctx)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := o.reconcileAll(ctx); err != nil {
			log.Printf("operator reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *operator) watch(ctx context.Context) {
	for {
		stream, err := o.client.Resource(serviceResource).Namespace(metav1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("operator watch: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for event := range stream.ResultChan() {
			if event.Type == watch.Error {
				break
			}
			if event.Type == watch.Deleted {
				continue
			}
			object, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if err := o.reconcile(ctx, object); err != nil {
				log.Printf("%s/%s: %v", object.GetNamespace(), object.GetName(), err)
			}
		}
		stream.Stop()
		if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

func (o *operator) reconcileAll(ctx context.Context) error {
	list, err := o.client.Resource(serviceResource).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := o.reconcile(ctx, &list.Items[i]); err != nil {
			log.Printf("%s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
		}
	}
	return nil
}

func (o *operator) reconcile(ctx context.Context, object *unstructured.Unstructured) error {
	// The watch and periodic resync run concurrently. Serialize a process's
	// updates so a status/finalizer write never races another local reconcile;
	// resource versions still protect against a second operator replica.
	o.mu.Lock()
	defer o.mu.Unlock()
	current := object.DeepCopy()
	for attempt := 0; attempt < 3; attempt++ {
		err := o.reconcileOnce(ctx, current)
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
		latest, getErr := o.client.Resource(serviceResource).Namespace(current.GetNamespace()).Get(ctx, current.GetName(), metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		current = latest
	}
	return fmt.Errorf("reconcile %s/%s conflicted after retries", current.GetNamespace(), current.GetName())
}

func (o *operator) reconcileOnce(ctx context.Context, object *unstructured.Unstructured) error {
	resourceClient := o.client.Resource(serviceResource).Namespace(object.GetNamespace())
	deletion := object.GetDeletionTimestamp()
	if deletion != nil && !deletion.IsZero() {
		if err := o.backend(object.GetNamespace()).Delete(ctx, object.GetName()); err != nil {
			return err
		}
		finalizers := removeFinalizer(object.GetFinalizers(), finalizer)
		object.SetFinalizers(finalizers)
		_, err := resourceClient.Update(ctx, object, metav1.UpdateOptions{})
		return err
	}
	if !contains(object.GetFinalizers(), finalizer) {
		object.SetFinalizers(append(object.GetFinalizers(), finalizer))
		updated, err := resourceClient.Update(ctx, object, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		*object = *updated
	}
	spec, found, err := unstructured.NestedFieldCopy(object.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("spec is required")
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	var service manifest.Manifest
	if err := json.Unmarshal(encoded, &service); err != nil {
		return err
	}
	service.Name = object.GetName()
	if err := service.Validate(); err != nil {
		return status(ctx, resourceClient, object, "Failed", err.Error(), nil, 0)
	}
	owner := metav1.OwnerReference{APIVersion: "plinth.dev/v1alpha1", Kind: "PlinthService", Name: object.GetName(), UID: object.GetUID(), Controller: pointer(true), BlockOwnerDeletion: pointer(true)}
	completedStep := intRolloutStep(object)
	observedGeneration, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	if observedGeneration != object.GetGeneration() {
		completedStep = 0
	}
	result, err := backend.EnsureWithRolloutFrom(ctx, o.backend(object.GetNamespace()).WithOwnerReference(owner), service, intRevision(object), completedStep)
	if err != nil {
		return status(ctx, resourceClient, object, "Failed", err.Error(), nil, result.RolloutStep)
	}
	if !result.Ready {
		return status(ctx, resourceClient, object, "Reconciling", "waiting for workload readiness", nil, result.RolloutStep)
	}
	observed := make([]any, 0, len(result.Resources))
	for _, resource := range result.Resources {
		observed = append(observed, map[string]any{"kind": resource.Kind, "name": resource.Name, "ready": resource.Ready})
	}
	return status(ctx, resourceClient, object, "Ready", fmt.Sprintf("revision %d is converged", intRevision(object)), observed, result.RolloutStep)
}

func status(ctx context.Context, resources dynamic.ResourceInterface, object *unstructured.Unstructured, phase, message string, observed []any, rolloutStep int) error {
	if observed == nil {
		observed = []any{}
	}
	conditionStatus := "False"
	conditionReason := "NotReady"
	if phase == "Ready" {
		conditionStatus = "True"
		conditionReason = "Converged"
	}
	statusObject := map[string]any{
		"phase": phase, "message": message, "observed": observed, "rolloutStep": int64(rolloutStep), "observedGeneration": object.GetGeneration(),
		"updatedAt":  time.Now().UTC().Format(time.RFC3339),
		"conditions": []any{map[string]any{"type": "Ready", "status": conditionStatus, "reason": conditionReason, "message": message, "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}},
	}
	copy := object.DeepCopy()
	if err := unstructured.SetNestedField(copy.Object, statusObject, "status"); err != nil {
		return err
	}
	_, err := resources.UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func intRevision(object *unstructured.Unstructured) int {
	value, found, _ := unstructured.NestedInt64(object.Object, "spec", "revision")
	if !found || value < 1 {
		return 1
	}
	return int(value)
}

func intRolloutStep(object *unstructured.Unstructured) int {
	value, found, _ := unstructured.NestedInt64(object.Object, "status", "rolloutStep")
	if !found || value < 0 {
		return 0
	}
	return int(value)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func removeFinalizer(values []string, unwanted string) []string {
	result := []string{}
	for _, value := range values {
		if value != unwanted {
			result = append(result, value)
		}
	}
	return result
}

func pointer[T any](value T) *T { return &value }

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if config, err := rest.InClusterConfig(); err == nil {
		return config, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}
