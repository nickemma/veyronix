package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/nickemma/plinth/internal/api"
	"github.com/nickemma/plinth/internal/backend"
	kubeBackend "github.com/nickemma/plinth/internal/backend/kubernetes"
	"github.com/nickemma/plinth/internal/reconcile"
	"github.com/nickemma/plinth/internal/state"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	statePath := flag.String("state", ".plinth-state.json", "desired-state file")
	backendName := flag.String("backend", "fake", "backend: fake or kubernetes")
	kubeconfig := flag.String("kubeconfig", "", "Kubernetes kubeconfig path; empty uses in-cluster config or ~/.kube/config")
	namespace := flag.String("namespace", "default", "Kubernetes namespace")
	flag.Parse()
	store, err := state.Open(*statePath)
	if err != nil {
		log.Fatal(err)
	}
	target, err := loadBackend(*backendName, *kubeconfig, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	controller := reconcile.NewController(store, target)
	worker := reconcile.NewWorker(controller, 10*time.Second)
	worker.Start(context.Background())
	server := api.NewServerWithWorker(controller, store, worker)
	fmt.Printf("plinthd listening on http://localhost%s\n", *addr)
	fmt.Printf("Swagger UI: http://localhost%s/docs\n", *addr)
	fmt.Printf("Playground: http://localhost%s/playground\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, server))
}

func loadBackend(name, kubeconfig, namespace string) (backend.Backend, error) {
	switch name {
	case "fake":
		return backend.NewFake(), nil
	case "kubernetes":
		config, err := kubeConfig(kubeconfig)
		if err != nil {
			return nil, err
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, fmt.Errorf("create Kubernetes client: %w", err)
		}
		return kubeBackend.New(client, namespace), nil
	default:
		return nil, fmt.Errorf("unsupported backend %q; choose fake or kubernetes", name)
	}
}

func kubeConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if config, err := rest.InClusterConfig(); err == nil {
		return config, nil
	}
	loading := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile},
		&clientcmd.ConfigOverrides{},
	)
	return loading.ClientConfig()
}
