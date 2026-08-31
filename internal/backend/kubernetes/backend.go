package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickemma/plinth/internal/backend"
	"github.com/nickemma/plinth/internal/manifest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const managedBy = "plinth"
const defaultController = "plinthd"

type Backend struct {
	client       kubernetes.Interface
	namespace    string
	owner        *metav1.OwnerReference
	controller   string
	metricsURL   string
	metricsQuery string
	httpClient   *http.Client
}

func New(client kubernetes.Interface, namespace string) *Backend {
	if namespace == "" {
		namespace = "default"
	}
	return &Backend{client: client, namespace: namespace, controller: defaultController, httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// WithPrometheusURL enables real error-rate measurements for progressive
// rollout. The URL may be a Prometheus base URL or its /api/v1/query URL.
func (b *Backend) WithPrometheusURL(value string) *Backend {
	copy := *b
	copy.metricsURL = strings.TrimRight(strings.TrimSpace(value), "/")
	return &copy
}

// WithPrometheusQuery overrides the default query. Its four %s placeholders
// receive namespace, service name, namespace, and service name respectively.
func (b *Backend) WithPrometheusQuery(value string) *Backend {
	copy := *b
	copy.metricsQuery = strings.TrimSpace(value)
	return &copy
}

// WithOwnerReference lets the operator give generated resources a real
// Kubernetes owner. Standalone API resources have no parent object, so they
// use managed-by labels for discovery and cleanup instead.
func (b *Backend) WithOwnerReference(owner metav1.OwnerReference) *Backend {
	copy := *b
	copy.owner = &owner
	copy.controller = "operator"
	return &copy
}

func (b *Backend) EnsureNamespace(ctx context.Context, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	namespaces := b.client.CoreV1().Namespaces()
	current, err := namespaces.Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		if current.Labels == nil {
			current.Labels = map[string]string{}
		}
		if current.Labels["app.kubernetes.io/managed-by"] != managedBy || current.Labels["plinth.dev/ingress"] != "allowed" {
			current.Labels["app.kubernetes.io/managed-by"] = managedBy
			current.Labels["plinth.dev/ingress"] = "allowed"
			_, err = namespaces.Update(ctx, current, metav1.UpdateOptions{})
		}
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = namespaces.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": managedBy, "plinth.dev/ingress": "allowed"}}}, metav1.CreateOptions{})
	return err
}

// ErrorRate queries Prometheus's instant-query API. A rollout deliberately
// fails closed when no metrics URL is configured, because silently treating a
// missing health signal as zero errors would defeat the rollout guard.
func (b *Backend) ErrorRate(ctx context.Context, m manifest.Manifest) (float64, error) {
	if b.metricsURL == "" {
		return 0, fmt.Errorf("prometheus URL is required for progressive rollout")
	}
	queryTemplate := b.metricsQuery
	if queryTemplate == "" {
		queryTemplate = `sum(rate(http_requests_total{namespace="%s",service="%s",status=~"5.."}[1m])) / clamp_min(sum(rate(http_requests_total{namespace="%s",service="%s"}[1m])), 1)`
	}
	namespace := m.Namespace
	if namespace == "" {
		namespace = b.namespace
	}
	query := queryTemplate
	if strings.Contains(queryTemplate, "%s") {
		query = fmt.Sprintf(queryTemplate, namespace, m.Name, namespace, m.Name)
	}
	endpoint := b.metricsURL
	if !strings.HasSuffix(endpoint, "/api/v1/query") {
		endpoint += "/api/v1/query"
	}
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return 0, fmt.Errorf("parse prometheus URL: %w", err)
	}
	values := requestURL.Query()
	values.Set("query", query)
	requestURL.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("create prometheus request: %w", err)
	}
	client := b.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("query prometheus: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return 0, fmt.Errorf("prometheus returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode prometheus response: %w", err)
	}
	if payload.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s: %s", payload.ErrorType, payload.Error)
	}
	if len(payload.Data.Result) == 0 || len(payload.Data.Result[0].Value) < 2 {
		return 0, nil
	}
	var encodedValue string
	if err := json.Unmarshal(payload.Data.Result[0].Value[1], &encodedValue); err != nil {
		return 0, fmt.Errorf("decode prometheus value: %w", err)
	}
	rate, err := strconv.ParseFloat(encodedValue, 64)
	if err != nil {
		return 0, fmt.Errorf("parse prometheus error rate %q: %w", encodedValue, err)
	}
	if rate < 0 || rate > 1 {
		return 0, fmt.Errorf("prometheus error rate %.4f is outside [0,1]", rate)
	}
	return rate, nil
}

func (b *Backend) own(object metav1.Object) {
	if b.owner != nil {
		object.SetOwnerReferences([]metav1.OwnerReference{*b.owner})
	}
}

func (b *Backend) Ensure(ctx context.Context, m manifest.Manifest, revision int) (backend.ApplyResult, error) {
	if m.Namespace != "" && m.Namespace != b.namespace {
		target := *b
		target.namespace = m.Namespace
		m.Namespace = ""
		return target.Ensure(ctx, m, revision)
	}
	selector := b.selector(m.Name)
	resources := backend.ApplyResult{}
	deployment := deploymentFor(m, revision, b.namespace, b.controller)
	b.own(deployment)
	if err := b.upsertDeployment(ctx, deployment); err != nil {
		return resources, err
	}
	observedDeployment, err := b.client.AppsV1().Deployments(b.namespace).Get(ctx, m.Name, metav1.GetOptions{})
	if err != nil {
		return resources, err
	}
	deploymentReady := deploymentIsReady(observedDeployment, m.Replicas)
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "Deployment", Name: m.Name, Revision: revision, Ready: deploymentReady})

	service := serviceFor(m, revision, b.namespace, b.controller)
	b.own(service)
	if err := b.upsertService(ctx, service); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "Service", Name: m.Name, Revision: revision, Ready: true})

	ingress := ingressFor(m, revision, b.namespace, b.controller)
	b.own(ingress)
	if err := b.upsertIngress(ctx, ingress); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "Ingress", Name: m.Name, Revision: revision, Ready: true})

	config := configMapFor(m, revision, b.namespace, b.controller)
	b.own(config)
	if err := b.upsertConfigMap(ctx, config); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "ConfigMap", Name: m.Name, Revision: revision, Ready: true})

	pdb := pdbFor(m, revision, b.namespace, b.controller)
	b.own(pdb)
	if err := b.upsertPDB(ctx, pdb); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "PodDisruptionBudget", Name: m.Name, Revision: revision, Ready: true})

	networkPolicy := networkPolicyFor(m, revision, b.namespace, b.controller)
	b.own(networkPolicy)
	if err := b.upsertNetworkPolicy(ctx, networkPolicy); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "NetworkPolicy", Name: m.Name, Revision: revision, Ready: true})

	// TLS, metrics, and structured logs are platform integrations represented
	// by annotations and ingress configuration. They are surfaced as virtual
	// resources so the API exposes the complete golden path consistently with
	// the fake backend.
	resources.Resources = append(resources.Resources,
		backend.Resource{Kind: "TLS", Name: m.Name, Revision: revision, Ready: true},
		backend.Resource{Kind: "Metrics", Name: m.Name, Revision: revision, Ready: true},
		backend.Resource{Kind: "Logs", Name: m.Name, Revision: revision, Ready: true},
	)
	sort.Slice(resources.Resources, func(i, j int) bool { return resources.Resources[i].Kind < resources.Resources[j].Kind })
	resources.Logs = []string{fmt.Sprintf("applied revision %d in namespace %s", revision, b.namespace), fmt.Sprintf("%d golden-path resources are managed", len(resources.Resources)), "selector: " + selector}
	resources.Ready = deploymentReady
	if !resources.Ready {
		resources.Logs = append(resources.Logs, "deployment is waiting for available replicas")
	}
	return resources, nil
}

func (b *Backend) Delete(ctx context.Context, name string) error {
	for _, kind := range []string{"deployment", "service", "ingress", "configmap", "poddisruptionbudget", "networkpolicy"} {
		if err := b.deleteEverywhere(ctx, kind, name); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) DeleteResource(ctx context.Context, name, kind string) error {
	for _, candidate := range []string{"deployment", "service", "ingress", "configmap", "poddisruptionbudget", "networkpolicy"} {
		if candidate == normalizeKind(kind) {
			return b.deleteEverywhere(ctx, candidate, name)
		}
	}
	return fmt.Errorf("resource kind %q is not deletable by the Kubernetes backend", kind)
}

func (b *Backend) Resources(ctx context.Context, name string) ([]backend.Resource, error) {
	deployments, err := b.client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: b.selector(name)})
	if err != nil {
		return nil, err
	}
	if len(deployments.Items) == 0 {
		return nil, fmt.Errorf("service %q has no backend resources", name)
	}
	ready := deploymentIsReady(&deployments.Items[0], replicasFromDeployment(&deployments.Items[0]))
	resources := []backend.Resource{}
	for _, kind := range []string{"ConfigMap", "Deployment", "Ingress", "Logs", "Metrics", "NetworkPolicy", "PodDisruptionBudget", "Service", "TLS"} {
		resources = append(resources, backend.Resource{Kind: kind, Name: name, Ready: ready})
	}
	return resources, nil
}

// Watch turns Deployment changes in any managed namespace into queue keys.
// The shared informer handles list/watch reconnects and the worker's periodic
// resync repairs events missed while the informer is unavailable.
func (b *Backend) Watch(ctx context.Context, enqueue func(string)) error {
	selector := labels.Set{"app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": b.controller}.AsSelector().String()
	factory := informers.NewSharedInformerFactoryWithOptions(b.client, 0, informers.WithTweakListOptions(func(options *metav1.ListOptions) {
		options.LabelSelector = selector
	}))
	informer := factory.Apps().V1().Deployments().Informer()
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(object any) { enqueueManagedDeployment(object, enqueue) },
		UpdateFunc: func(_, object any) { enqueueManagedDeployment(object, enqueue) },
		DeleteFunc: func(object any) { enqueueManagedDeployment(object, enqueue) },
	}
	if _, err := informer.AddEventHandler(handler); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return nil
	}
	<-ctx.Done()
	return nil
}

func enqueueManagedDeployment(object any, enqueue func(string)) {
	deployment, ok := object.(*appsv1.Deployment)
	if ok && deployment.Labels["app.kubernetes.io/managed-by"] == managedBy {
		enqueue(deployment.Name)
	}
}

func (b *Backend) deleteEverywhere(ctx context.Context, kind, name string) error {
	options := metav1.ListOptions{LabelSelector: b.selector(name)}
	switch kind {
	case "deployment":
		items, err := b.client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return err
		}
		for _, item := range items.Items {
			if err := b.delete(ctx, kind, item.Namespace, item.Name); err != nil {
				return err
			}
		}
	case "service":
		items, err := b.client.CoreV1().Services(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return err
		}
		for _, item := range items.Items {
			if err := b.delete(ctx, kind, item.Namespace, item.Name); err != nil {
				return err
			}
		}
	case "ingress":
		items, err := b.client.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return err
		}
		for _, item := range items.Items {
			if err := b.delete(ctx, kind, item.Namespace, item.Name); err != nil {
				return err
			}
		}
	case "configmap":
		items, err := b.client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return err
		}
		for _, item := range items.Items {
			if err := b.delete(ctx, kind, item.Namespace, item.Name); err != nil {
				return err
			}
		}
	case "poddisruptionbudget":
		items, err := b.client.PolicyV1().PodDisruptionBudgets(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return err
		}
		for _, item := range items.Items {
			if err := b.delete(ctx, kind, item.Namespace, item.Name); err != nil {
				return err
			}
		}
	case "networkpolicy":
		items, err := b.client.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, options)
		if err != nil {
			return err
		}
		for _, item := range items.Items {
			if err := b.delete(ctx, kind, item.Namespace, item.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) delete(ctx context.Context, kind, namespace, name string) error {
	options := metav1.DeleteOptions{}
	var err error
	switch kind {
	case "deployment":
		err = b.client.AppsV1().Deployments(namespace).Delete(ctx, name, options)
	case "service":
		err = b.client.CoreV1().Services(namespace).Delete(ctx, name, options)
	case "ingress":
		err = b.client.NetworkingV1().Ingresses(namespace).Delete(ctx, name, options)
	case "configmap":
		err = b.client.CoreV1().ConfigMaps(namespace).Delete(ctx, name, options)
	case "poddisruptionbudget":
		err = b.client.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, name, options)
	case "networkpolicy":
		err = b.client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, options)
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (b *Backend) upsertDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	items := b.client.AppsV1().Deployments(b.namespace)
	current, err := items.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := b.checkOwnership(current); err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Spec.Selector, desired.Spec.Selector) {
		if err := items.Delete(ctx, desired.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	return applyPatch(ctx, items.Patch, desired.Name, desired)
}

func (b *Backend) upsertService(ctx context.Context, desired *corev1.Service) error {
	items := b.client.CoreV1().Services(b.namespace)
	current, err := items.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := b.checkOwnership(current); err != nil {
		return err
	}
	desired.Spec.ClusterIP = current.Spec.ClusterIP
	return applyPatch(ctx, items.Patch, desired.Name, desired)
}

func (b *Backend) upsertIngress(ctx context.Context, desired *networkingv1.Ingress) error {
	items := b.client.NetworkingV1().Ingresses(b.namespace)
	current, err := items.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := b.checkOwnership(current); err != nil {
		return err
	}
	return applyPatch(ctx, items.Patch, desired.Name, desired)
}

func (b *Backend) upsertConfigMap(ctx context.Context, desired *corev1.ConfigMap) error {
	items := b.client.CoreV1().ConfigMaps(b.namespace)
	current, err := items.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := b.checkOwnership(current); err != nil {
		return err
	}
	return applyPatch(ctx, items.Patch, desired.Name, desired)
}

func (b *Backend) upsertPDB(ctx context.Context, desired *policyv1.PodDisruptionBudget) error {
	items := b.client.PolicyV1().PodDisruptionBudgets(b.namespace)
	current, err := items.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := b.checkOwnership(current); err != nil {
		return err
	}
	return applyPatch(ctx, items.Patch, desired.Name, desired)
}

func (b *Backend) upsertNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	items := b.client.NetworkingV1().NetworkPolicies(b.namespace)
	current, err := items.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := b.checkOwnership(current); err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Spec.PodSelector, desired.Spec.PodSelector) {
		if err := items.Delete(ctx, desired.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		_, err = items.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	return applyPatch(ctx, items.Patch, desired.Name, desired)
}

func (b *Backend) checkOwnership(object metav1.Object) error {
	controller := b.controller
	if controller == "" {
		controller = defaultController
	}
	objectLabels := object.GetLabels()
	if objectLabels["app.kubernetes.io/managed-by"] != managedBy {
		return fmt.Errorf("resource %s/%s is not managed by Plinth", object.GetNamespace(), object.GetName())
	}
	if current := objectLabels["plinth.dev/controller"]; current != "" && current != controller {
		return fmt.Errorf("resource %s/%s is owned by controller %q", object.GetNamespace(), object.GetName(), current)
	}
	return nil
}

type patchFunc[T any] func(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*T, error)

func applyPatch[T any](ctx context.Context, patch patchFunc[T], name string, desired any) error {
	payload, err := json.Marshal(desired)
	if err != nil {
		return err
	}
	force := true
	_, err = patch(ctx, name, types.ApplyPatchType, payload, metav1.PatchOptions{FieldManager: managedBy, Force: &force})
	return err
}

func baseMeta(m manifest.Manifest, revision int, namespace string) metav1.ObjectMeta {
	meta := metav1.ObjectMeta{Name: m.Name, Namespace: namespace, Labels: map[string]string{
		"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": defaultController, "plinth.dev/revision": fmt.Sprint(revision),
	}}
	return meta
}

func managedSelector(name string) string {
	return managedSelectorFor(name, defaultController)
}

func managedSelectorFor(name, controller string) string {
	return labels.Set{"app.kubernetes.io/name": name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": controller}.AsSelector().String()
}

func (b *Backend) selector(name string) string {
	controller := b.controller
	if controller == "" {
		controller = defaultController
	}
	return managedSelectorFor(name, controller)
}

func replicasFromDeployment(deployment *appsv1.Deployment) int {
	if deployment.Spec.Replicas == nil {
		return 0
	}
	return int(*deployment.Spec.Replicas)
}

func deploymentIsReady(deployment *appsv1.Deployment, desired int) bool {
	if desired == 0 {
		return true
	}
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return false
	}
	return deployment.Status.UpdatedReplicas >= int32(desired) && deployment.Status.ReadyReplicas >= int32(desired) && deployment.Status.AvailableReplicas >= int32(desired)
}

func deploymentFor(m manifest.Manifest, revision int, namespace string, controllers ...string) *appsv1.Deployment {
	controller := controllerLabel(controllers)
	meta := baseMetaWithController(m, revision, namespace, controller)
	replicas := int32(m.Replicas)
	labels := map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": controller}
	annotations := map[string]string{
		"plinth.dev/logs":      "structured",
		"prometheus.io/scrape": "true",
		"prometheus.io/path":   "/metrics",
		"prometheus.io/port":   strconv.Itoa(m.Port),
	}
	cpu, _ := resource.ParseQuantity(m.Resources.CPU)
	memory, _ := resource.ParseQuantity(m.Resources.Memory)
	return &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true), FSGroup: ptr(int64(65532))}, Containers: []corev1.Container{{Name: m.Name, Image: m.Image, ImagePullPolicy: corev1.PullIfNotPresent, Ports: []corev1.ContainerPort{{ContainerPort: int32(m.Port)}}, Env: envFor(m), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}, Limits: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true), RunAsNonRoot: ptr(true)}, LivenessProbe: probeFor(m), ReadinessProbe: probeFor(m), VolumeMounts: []corev1.VolumeMount{{Name: "plinth-runtime-data", MountPath: "/home/nonroot/.lattice-data"}}}}, Volumes: []corev1.Volume{{Name: "plinth-runtime-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}}}
}

func serviceFor(m manifest.Manifest, revision int, namespace string, controllers ...string) *corev1.Service {
	controller := controllerLabel(controllers)
	meta := baseMetaWithController(m, revision, namespace, controller)
	labels := map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": controller}
	return &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta, Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: int32(m.Port), TargetPort: intstr.FromInt(m.Port)}}}}
}

func ingressFor(m manifest.Manifest, revision int, namespace string, controllers ...string) *networkingv1.Ingress {
	meta := baseMetaWithController(m, revision, namespace, controllerLabel(controllers))
	meta.Annotations = map[string]string{"cert-manager.io/cluster-issuer": "plinth-default", "plinth.dev/metrics": "enabled", "plinth.dev/logs": "structured"}
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"}, ObjectMeta: meta, Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{Hosts: []string{m.Name + ".plinth.local"}, SecretName: m.Name + "-tls"}}, Rules: []networkingv1.IngressRule{{Host: m.Name + ".plinth.local", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: m.Name, Port: networkingv1.ServiceBackendPort{Number: int32(m.Port)}}}}}}}}}}}
}

func configMapFor(m manifest.Manifest, revision int, namespace string, controllers ...string) *corev1.ConfigMap {
	meta := baseMetaWithController(m, revision, namespace, controllerLabel(controllers))
	return &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta, Data: m.Env}
}

func pdbFor(m manifest.Manifest, revision int, namespace string, controllers ...string) *policyv1.PodDisruptionBudget {
	controller := controllerLabel(controllers)
	meta := baseMetaWithController(m, revision, namespace, controller)
	min := intstr.FromInt(1)
	return &policyv1.PodDisruptionBudget{TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"}, ObjectMeta: meta, Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &min, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": controller}}}}
}

func networkPolicyFor(m manifest.Manifest, revision int, namespace string, controllers ...string) *networkingv1.NetworkPolicy {
	controller := controllerLabel(controllers)
	meta := baseMetaWithController(m, revision, namespace, controller)
	return &networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: meta, Spec: networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/controller": controller}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"plinth.dev/ingress": "allowed"}}}}}},
		Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr(corev1.ProtocolUDP), Port: ptr(intstr.FromInt(53))}, {Protocol: ptr(corev1.ProtocolTCP), Port: ptr(intstr.FromInt(53))}}}},
	}}
}

func baseMetaWithController(m manifest.Manifest, revision int, namespace, controller string) metav1.ObjectMeta {
	meta := baseMeta(m, revision, namespace)
	meta.Labels["plinth.dev/controller"] = controller
	return meta
}

func controllerLabel(values []string) string {
	if len(values) > 0 && values[0] != "" {
		return values[0]
	}
	return defaultController
}

func envFor(m manifest.Manifest) []corev1.EnvVar {
	keys := make([]string, 0, len(m.Env))
	for key := range m.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]corev1.EnvVar, 0, len(keys)+len(m.Secrets))
	for _, key := range keys {
		result = append(result, corev1.EnvVar{Name: key, ValueFrom: nil, Value: m.Env[key]})
	}
	for _, key := range m.Secrets {
		result = append(result, corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: m.Name + "-secrets"}, Key: key}}})
	}
	return result
}

func probeFor(m manifest.Manifest) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(m.Port)}}, InitialDelaySeconds: 5, PeriodSeconds: 5}
}

func normalizeKind(kind string) string {
	switch kind {
	case "Deployment":
		return "deployment"
	case "Service":
		return "service"
	case "Ingress":
		return "ingress"
	case "ConfigMap":
		return "configmap"
	case "PodDisruptionBudget":
		return "poddisruptionbudget"
	case "NetworkPolicy":
		return "networkpolicy"
	default:
		return kind
	}
}

func ptr[T any](value T) *T { return &value }
