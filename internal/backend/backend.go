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
	Resources   []Resource
	Logs        []string
	ErrorRate   float64
	Ready       bool
	RolloutStep int
}

type Backend interface {
	Ensure(context.Context, manifest.Manifest, int) (ApplyResult, error)
	Delete(context.Context, string) error
	DeleteResource(context.Context, string, string) error
	Resources(context.Context, string) ([]Resource, error)
}

// Watcher is implemented by providers that can turn backend changes into
// level-triggered reconciliation requests. Providers without native watches
// continue to use the worker's periodic resync.
type Watcher interface {
	Watch(context.Context, func(string)) error
}

type NamespaceManager interface {
	EnsureNamespace(context.Context, string) error
}

// ErrorRateReader supplies the health signal used by progressive rollout.
// Backends without an external metric source can continue to return their own
// deterministic signal through ApplyResult.ErrorRate.
type ErrorRateReader interface {
	ErrorRate(context.Context, manifest.Manifest) (float64, error)
}

// EnsureWithRollout applies a manifest through its configured replica steps.
// It lives beside the backend contract so the standalone controller and the
// Kubernetes operator make the same rollout decision.
func EnsureWithRollout(ctx context.Context, target Backend, m manifest.Manifest, revision int) (ApplyResult, error) {
	return EnsureWithRolloutFrom(ctx, target, m, revision, 0)
}

// EnsureWithRolloutFrom resumes a rollout at the first step greater than the
// persisted completed step. Backend watch events can arrive after a stage
// succeeds, so a fresh reconcile must not scale a finished rollout back down.
func EnsureWithRolloutFrom(ctx context.Context, target Backend, m manifest.Manifest, revision, completedStep int) (ApplyResult, error) {
	if !m.Rollout.Enabled {
		return target.Ensure(ctx, m, revision)
	}
	steps := append([]int(nil), m.Rollout.Steps...)
	if len(steps) == 0 {
		steps = []int{10, 50, 100}
	}
	if completedStep >= steps[len(steps)-1] {
		staged := m
		staged.Rollout.Enabled = false
		result, err := target.Ensure(ctx, staged, revision)
		result.RolloutStep = completedStep
		return result, err
	}
	var result ApplyResult
	for _, step := range steps {
		if step <= completedStep {
			continue
		}
		staged := m
		staged.Rollout.Enabled = false
		staged.Replicas = rolloutReplicas(m.Replicas, step)
		stageResult, err := target.Ensure(ctx, staged, revision)
		result.Resources = stageResult.Resources
		result.Ready = stageResult.Ready
		result.RolloutStep = step
		result.Logs = append(result.Logs, stageResult.Logs...)
		result.Logs = append(result.Logs, fmt.Sprintf("rollout step %d%% applied with %d replica(s)", step, staged.Replicas))
		if err != nil || !stageResult.Ready {
			return result, err
		}
		if reader, ok := target.(ErrorRateReader); ok {
			rate, readErr := reader.ErrorRate(ctx, m)
			if readErr != nil {
				result.ErrorRate = 0
				result.Logs = append(result.Logs, "error-rate measurement failed: "+readErr.Error())
				return result, readErr
			}
			stageResult.ErrorRate = rate
		}
		result.ErrorRate = stageResult.ErrorRate
		if stageResult.ErrorRate > m.Rollout.MaxErrorRate {
			return result, fmt.Errorf("rollout aborted at %d%%: error rate %.2f exceeded threshold %.2f", step, stageResult.ErrorRate, m.Rollout.MaxErrorRate)
		}
	}
	return result, nil
}

func rolloutReplicas(replicas, percentage int) int {
	if replicas == 0 {
		return 0
	}
	count := (replicas*percentage + 99) / 100
	if count < 1 {
		return 1
	}
	return count
}

// Fake is the permanent dependency-free backend. It models the resources the
// Kubernetes golden path creates without requiring a cluster.
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
	rolloutErrorRate := 0.0
	if strings.Contains(strings.ToLower(m.Image), "error") {
		rolloutErrorRate = 0.5
	}

	kinds := []string{"Deployment", "Service", "Ingress", "TLS", "Metrics", "Logs", "ConfigMap", "PodDisruptionBudget", "NetworkPolicy"}
	resources := make(map[string]Resource, len(kinds))
	result := ApplyResult{Logs: []string{fmt.Sprintf("applying revision %d for %s", revision, m.Name)}, Ready: true}
	if m.Replicas == 0 {
		result.Logs = append(result.Logs, "replicas is zero; service is intentionally paused")
	}
	result.ErrorRate = rolloutErrorRate
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

func (f *Fake) EnsureNamespace(context.Context, string) error { return nil }

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
