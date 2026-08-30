package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nickemma/plinth/internal/manifest"
)

type Resource struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Revision int    `json:"revision"`
	Ready    bool   `json:"ready"`
}

type ApplyResult struct {
	Resources []Resource
	Logs      []string
}

type Backend interface {
	Ensure(context.Context, manifest.Manifest, int) (ApplyResult, error)
	Delete(context.Context, string) error
	DeleteResource(context.Context, string, string) error
	Resources(context.Context, string) ([]Resource, error)
}

// Fake is the first permanent backend. It models the resources the golden
// path will eventually create in Kubernetes, without requiring a cluster.
type Fake struct {
	mu       sync.Mutex
	services map[string]map[string]Resource
}

func NewFake() *Fake { return &Fake{services: map[string]map[string]Resource{}} }

func (f *Fake) Ensure(ctx context.Context, m manifest.Manifest, revision int) (ApplyResult, error) {
	select {
	case <-ctx.Done():
		return ApplyResult{}, ctx.Err()
	default:
	}
	if strings.Contains(strings.ToLower(m.Image), "bad") || strings.Contains(strings.ToLower(m.Image), "fail") {
		return ApplyResult{Logs: []string{fmt.Sprintf("image %s failed readiness", m.Image)}}, fmt.Errorf("readiness check failed for image %q", m.Image)
	}
	if m.Replicas == 0 {
		return ApplyResult{Logs: []string{"replicas is zero; service is intentionally paused"}}, nil
	}

	kinds := []string{"Deployment", "Service", "Ingress", "TLS", "Metrics", "Logs", "ConfigMap", "PodDisruptionBudget", "NetworkPolicy"}
	resources := make(map[string]Resource, len(kinds))
	result := ApplyResult{Logs: []string{fmt.Sprintf("applying revision %d for %s", revision, m.Name)}}
	for _, kind := range kinds {
		resource := Resource{Kind: kind, Name: m.Name, Revision: revision, Ready: true}
		resources[kind] = resource
		result.Resources = append(result.Resources, resource)
	}
	sort.Slice(result.Resources, func(i, j int) bool { return result.Resources[i].Kind < result.Resources[j].Kind })
	f.mu.Lock()
	f.services[m.Name] = resources
	f.mu.Unlock()
	result.Logs = append(result.Logs, fmt.Sprintf("%d golden-path resources are ready", len(result.Resources)))
	return result, nil
}

func (f *Fake) Delete(ctx context.Context, name string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	f.mu.Lock()
	delete(f.services, name)
	f.mu.Unlock()
	return nil
}

func (f *Fake) DeleteResource(ctx context.Context, name, kind string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resources, ok := f.services[name]
	if !ok {
		return fmt.Errorf("service %q has no backend resources", name)
	}
	if _, ok := resources[kind]; !ok {
		return fmt.Errorf("resource %s/%s does not exist", kind, name)
	}
	delete(resources, kind)
	return nil
}

func (f *Fake) Resources(ctx context.Context, name string) ([]Resource, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resources, ok := f.services[name]
	if !ok {
		return nil, fmt.Errorf("service %q has no backend resources", name)
	}
	out := make([]Resource, 0, len(resources))
	for _, resource := range resources {
		out = append(out, resource)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}
