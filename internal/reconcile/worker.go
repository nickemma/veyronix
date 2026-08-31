package reconcile

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nickemma/plinth/internal/backend"
)

// Worker turns reconciliation into a level-triggered process. Requests and
// backend events enqueue a name; the periodic resync repairs missed events.
type Worker struct {
	controller *Controller
	interval   time.Duration
	queue      chan string
	mu         sync.Mutex
	pending    map[string]struct{}
}

func NewWorker(controller *Controller, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Worker{
		controller: controller,
		interval:   interval,
		queue:      make(chan string, 256),
		pending:    map[string]struct{}{},
	}
}

func (w *Worker) Enqueue(name string) {
	if name == "" {
		return
	}
	w.mu.Lock()
	if _, exists := w.pending[name]; exists {
		w.mu.Unlock()
		return
	}
	w.pending[name] = struct{}{}
	w.mu.Unlock()
	select {
	case w.queue <- name:
	default:
		w.mu.Lock()
		delete(w.pending, name)
		w.mu.Unlock()
		log.Printf("reconcile queue is full; dropped %s", name)
	}
}

func (w *Worker) Start(ctx context.Context) {
	go func() {
		if watcher, ok := w.controller.backend.(backend.Watcher); ok {
			go func() {
				if err := watcher.Watch(ctx, w.Enqueue); err != nil && ctx.Err() == nil {
					log.Printf("backend watch: %v", err)
				}
			}()
		}
		for _, service := range w.controller.store.List() {
			w.Enqueue(service.Name)
		}
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case name := <-w.queue:
				w.clearPending(name)
				if _, err := w.controller.Reconcile(ctx, name); err != nil {
					log.Printf("reconcile %s: %v", name, err)
				}
			case <-ticker.C:
				for _, service := range w.controller.store.List() {
					w.Enqueue(service.Name)
				}
			}
		}
	}()
}

func (w *Worker) clearPending(name string) {
	w.mu.Lock()
	delete(w.pending, name)
	w.mu.Unlock()
}
