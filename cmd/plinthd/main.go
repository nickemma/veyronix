package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
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
	databaseDSN := flag.String("database-dsn", os.Getenv("PLINTH_DATABASE_DSN"), "PostgreSQL DSN; empty uses the local file store")
	backendName := flag.String("backend", "fake", "backend: fake or kubernetes")
	kubeconfig := flag.String("kubeconfig", "", "Kubernetes kubeconfig path; empty uses in-cluster config or ~/.kube/config")
	namespace := flag.String("namespace", "plinth-default", "Kubernetes namespace")
	prometheusURL := flag.String("prometheus-url", os.Getenv("PLINTH_PROMETHEUS_URL"), "Prometheus URL for progressive rollout error-rate checks")
	prometheusQuery := flag.String("prometheus-query", os.Getenv("PLINTH_PROMETHEUS_QUERY"), "Prometheus query template; %s placeholders are namespace, service, namespace, service")
	flag.Parse()
	var err error
	var store state.Repository
	var closeStore func()
	if *databaseDSN != "" {
		postgres, err := state.OpenPostgres(context.Background(), *databaseDSN)
		if err != nil {
			log.Fatal(err)
		}
		store = postgres
		closeStore = postgres.Close
	} else {
		fileStore, err := state.Open(*statePath)
		if err != nil {
			log.Fatal(err)
		}
		store = fileStore
	}
	if closeStore != nil {
		defer closeStore()
	}
	target, err := loadBackend(*backendName, *kubeconfig, *namespace, *prometheusURL, *prometheusQuery)
	if err != nil {
		log.Fatal(err)
	}
	controller := reconcile.NewController(store, target)
	if *backendName == "kubernetes" {
		if err := controller.EnsureNamespace(context.Background(), *namespace); err != nil {
			log.Fatal(err)
		}
	}
	worker := reconcile.NewWorker(controller, 10*time.Second)
	worker.Start(context.Background())
	server := api.NewServerWithWorkerAndNamespace(controller, store, worker, *namespace)
	fmt.Printf("plinthd listening on http://localhost%s\n", *addr)
	fmt.Printf("Swagger UI: http://localhost%s/docs\n", *addr)
	fmt.Printf("Playground: http://localhost%s/playground\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, server))
}

func loadBackend(name, kubeconfig, namespace, prometheusURL, prometheusQuery string) (backend.Backend, error) {
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
		return kubeBackend.New(client, namespace).WithPrometheusURL(prometheusURL).WithPrometheusQuery(prometheusQuery), nil
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
