package reconcile

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/nickemma/plinth/internal/backend"
	"github.com/nickemma/plinth/internal/manifest"
	"github.com/nickemma/plinth/internal/state"
)

type Controller struct {
	store   state.Repository
	backend backend.Backend
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewController(store state.Repository, target backend.Backend) *Controller {
	return &Controller{store: store, backend: target, locks: map[string]*sync.Mutex{}}
}

func (c *Controller) EnsureNamespace(ctx context.Context, namespace string) error {
	if manager, ok := c.backend.(backend.NamespaceManager); ok {
		return manager.EnsureNamespace(ctx, namespace)
	}
	return nil
}

// ReconcileAll rebuilds backend state after a process restart and is also the
// startup pass used by the Kubernetes-backed control plane.
func (c *Controller) ReconcileAll(ctx context.Context) error {
	var firstErr error
	for _, service := range c.store.List() {
		if service.Paused || service.Destroyed {
			continue
		}
		if _, err := c.Reconcile(ctx, service.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Controller) Apply(ctx context.Context, m manifest.Manifest) (state.Service, error) {
	unlock := c.lockService(m.Name)
	defer unlock()
	service, err := c.store.Apply(m, "apply")
	if err != nil {
		return state.Service{}, err
	}
	return c.reconcileLocked(ctx, service.Name)
}

// ApplyDesired records a revision without waiting for the backend. The
// worker owns reconciliation, which keeps API writes quick and makes retries
// and periodic resync part of the normal execution model.
func (c *Controller) ApplyDesired(m manifest.Manifest) (state.Service, error) {
	return c.store.Apply(m, "apply")
}

func (c *Controller) Reconcile(ctx context.Context, name string) (state.Service, error) {
	unlock := c.lockService(name)
	defer unlock()
	return c.reconcileLocked(ctx, name)
}

func (c *Controller) reconcileLocked(ctx context.Context, name string) (state.Service, error) {
	service, err := c.store.Get(name)
	if err != nil {
		return state.Service{}, err
	}
	if service.Destroyed {
		return service, nil
	}
	if service.Paused {
		return service, nil
	}
	revision, err := c.store.Revision(name, service.DesiredRevision)
	if err != nil {
		return state.Service{}, err
	}
	wasRolledBack := service.Phase == state.PhaseRolledBack
	_, err = c.store.Update(name, func(current *state.Service) error {
		current.Phase = state.PhaseReconciling
		current.Message = fmt.Sprintf("reconciling revision %d", revision.Number)
		state.AddEvent(current, "reconcile_started", current.Message)
		return nil
	})
	if err != nil {
		return state.Service{}, err
	}
	result, applyErr := backend.EnsureWithRolloutFrom(ctx, c.backend, revision.Manifest, revision.Number, service.RolloutStep)
	if applyErr == nil && !result.Ready {
		waiting, updateErr := c.store.Update(name, func(current *state.Service) error {
			current.Phase = state.PhaseReconciling
			current.Message = fmt.Sprintf("revision %d is waiting for backend readiness", revision.Number)
			current.RolloutStep = result.RolloutStep
			for _, line := range result.Logs {
				state.AddLog(current, "stdout", line)
			}
			state.AddEvent(current, "reconcile_waiting", current.Message)
			return nil
		})
		if updateErr != nil {
			return state.Service{}, updateErr
		}
		return waiting, nil
	}
	if applyErr == nil {
		return c.store.Update(name, func(current *state.Service) error {
			current.ActiveRevision = revision.Number
			current.LastKnownGood = revision.Number
			current.Phase = state.PhaseReady
			current.Message = fmt.Sprintf("revision %d is converged", revision.Number)
			current.RolloutStep = result.RolloutStep
			if wasRolledBack {
				current.Phase = state.PhaseRolledBack
				current.Message = fmt.Sprintf("revision %d remains restored", revision.Number)
			}
			current.Observed = resourceNames(result.Resources)
			for _, line := range result.Logs {
				state.AddLog(current, "stdout", line)
			}
			state.AddEvent(current, "reconcile_succeeded", current.Message)
			return nil
		})
	}

	failed, updateErr := c.store.Update(name, func(current *state.Service) error {
		current.Phase = state.PhaseFailed
		current.RolloutStep = result.RolloutStep
		current.Message = fmt.Sprintf("revision %d failed: %v", revision.Number, applyErr)
		for _, line := range result.Logs {
			state.AddLog(current, "stderr", line)
		}
		state.AddLog(current, "stderr", applyErr.Error())
		state.AddEvent(current, "reconcile_failed", current.Message)
		return nil
	})
	if updateErr != nil {
		return state.Service{}, updateErr
	}
	if failed.LastKnownGood == 0 || failed.LastKnownGood == revision.Number {
		return failed, applyErr
	}

	lastGood, err := c.store.Revision(name, failed.LastKnownGood)
	if err != nil {
		return failed, applyErr
	}
	rollback, rollbackErr := c.backend.Ensure(ctx, lastGood.Manifest, lastGood.Number)
	if rollbackErr != nil {
		final, _ := c.store.Update(name, func(current *state.Service) error {
			current.Message = fmt.Sprintf("revision %d failed and rollback to revision %d failed: %v", revision.Number, lastGood.Number, rollbackErr)
			state.AddEvent(current, "rollback_failed", current.Message)
			return nil
		})
		return final, applyErr
	}
	final, statusErr := c.store.Update(name, func(current *state.Service) error {
		// The failed revision remains in history, but it must stop being the
		// desired target or every periodic resync would retry it forever.
		current.DesiredRevision = lastGood.Number
		current.RolloutStep = 0
		current.ActiveRevision = lastGood.Number
		current.Phase = state.PhaseRolledBack
		current.Message = fmt.Sprintf("revision %d failed; revision %d restored", revision.Number, lastGood.Number)
		current.Observed = resourceNames(rollback.Resources)
		for _, line := range rollback.Logs {
			state.AddLog(current, "stdout", line)
		}
		state.AddEvent(current, "rolled_back", current.Message)
		return nil
	})
	if statusErr != nil {
		return state.Service{}, statusErr
	}
	return final, applyErr
}

func (c *Controller) Rollback(ctx context.Context, name string, target int) (state.Service, error) {
	unlock := c.lockService(name)
	defer unlock()
	service, err := c.store.Rollback(name, target)
	if err != nil {
		return state.Service{}, err
	}
	return c.reconcileLocked(ctx, service.Name)
}

func (c *Controller) Pause(name string) (state.Service, error) {
	unlock := c.lockService(name)
	defer unlock()
	return c.store.Update(name, func(service *state.Service) error {
		service.Paused = true
		service.Phase = state.PhasePaused
		service.Message = "reconciliation paused; current resources remain running"
		state.AddEvent(service, "paused", service.Message)
		return nil
	})
}

func (c *Controller) Resume(ctx context.Context, name string) (state.Service, error) {
	unlock := c.lockService(name)
	defer unlock()
	service, err := c.store.Update(name, func(service *state.Service) error {
		service.Paused = false
		service.Phase = state.PhasePending
		service.Message = "reconciliation resumed"
		state.AddEvent(service, "resumed", service.Message)
		return nil
	})
	if err != nil {
		return state.Service{}, err
	}
	return c.reconcileLocked(ctx, service.Name)
}

func (c *Controller) Destroy(ctx context.Context, name string) (state.Service, error) {
	unlock := c.lockService(name)
	defer unlock()
	if err := c.backend.Delete(ctx, name); err != nil {
		return state.Service{}, err
	}
	return c.store.Update(name, func(service *state.Service) error {
		service.Destroyed = true
		service.Phase = state.PhaseDestroyed
		service.Message = "service resources destroyed; revision history retained"
		service.Observed = nil
		state.AddEvent(service, "destroyed", service.Message)
		return nil
	})
}

func (c *Controller) Drift(ctx context.Context, name, kind string) (state.Service, error) {
	unlock := c.lockService(name)
	defer unlock()
	if err := c.backend.DeleteResource(ctx, name, kind); err != nil {
		return state.Service{}, err
	}
	service, err := c.store.Update(name, func(service *state.Service) error {
		service.Message = fmt.Sprintf("simulated drift: deleted %s/%s", kind, name)
		state.AddEvent(service, "drift_detected", service.Message)
		return nil
	})
	if err != nil {
		return state.Service{}, err
	}
	return c.reconcileLocked(ctx, service.Name)
}

func (c *Controller) lockService(name string) func() {
	c.locksMu.Lock()
	lock := c.locks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[name] = lock
	}
	c.locksMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func resourceNames(resources []backend.Resource) []string {
	result := make([]string, 0, len(resources))
	for _, resource := range resources {
		result = append(result, resource.Kind+"/"+resource.Name)
	}
	return result
}

func IsNotFound(err error) bool { return os.IsNotExist(err) }
