package kubernetes

import (
	"context"
	"fmt"
	"sort"

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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const managedBy = "plinth"

type Backend struct {
	client    kubernetes.Interface
	namespace string
}

func New(client kubernetes.Interface, namespace string) *Backend {
	if namespace == "" {
		namespace = "default"
	}
	return &Backend{client: client, namespace: namespace}
}

func (b *Backend) Ensure(ctx context.Context, m manifest.Manifest, revision int) (backend.ApplyResult, error) {
	selector := labels.Set{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy}.AsSelector().String()
	resources := backend.ApplyResult{}
	deployment := deploymentFor(m, revision, b.namespace)
	if err := b.upsertDeployment(ctx, deployment); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "Deployment", Name: m.Name, Revision: revision, Ready: true})

	service := serviceFor(m, b.namespace)
	if err := b.upsertService(ctx, service); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "Service", Name: m.Name, Revision: revision, Ready: true})

	ingress := ingressFor(m, b.namespace)
	if err := b.upsertIngress(ctx, ingress); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "Ingress", Name: m.Name, Revision: revision, Ready: true})

	config := configMapFor(m, b.namespace)
	if err := b.upsertConfigMap(ctx, config); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "ConfigMap", Name: m.Name, Revision: revision, Ready: true})

	pdb := pdbFor(m, b.namespace)
	if err := b.upsertPDB(ctx, pdb); err != nil {
		return resources, err
	}
	resources.Resources = append(resources.Resources, backend.Resource{Kind: "PodDisruptionBudget", Name: m.Name, Revision: revision, Ready: true})

	networkPolicy := networkPolicyFor(m, b.namespace)
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
	return resources, nil
}

func (b *Backend) Delete(ctx context.Context, name string) error {
	for _, kind := range []string{"deployment", "service", "ingress", "configmap", "poddisruptionbudget", "networkpolicy"} {
		if err := b.delete(ctx, kind, name); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) DeleteResource(ctx context.Context, name, kind string) error {
	for _, candidate := range []string{"deployment", "service", "ingress", "configmap", "poddisruptionbudget", "networkpolicy"} {
		if candidate == normalizeKind(kind) {
			return b.delete(ctx, candidate, name)
		}
	}
	return fmt.Errorf("resource kind %q is not deletable by the Kubernetes backend", kind)
}

func (b *Backend) Resources(ctx context.Context, name string) ([]backend.Resource, error) {
	if _, err := b.client.AppsV1().Deployments(b.namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		return nil, err
	}
	resources := []backend.Resource{}
	for _, kind := range []string{"ConfigMap", "Deployment", "Ingress", "Logs", "Metrics", "NetworkPolicy", "PodDisruptionBudget", "Service", "TLS"} {
		resources = append(resources, backend.Resource{Kind: kind, Name: name, Ready: true})
	}
	return resources, nil
}

func (b *Backend) delete(ctx context.Context, kind, name string) error {
	options := metav1.DeleteOptions{}
	var err error
	switch kind {
	case "deployment":
		err = b.client.AppsV1().Deployments(b.namespace).Delete(ctx, name, options)
	case "service":
		err = b.client.CoreV1().Services(b.namespace).Delete(ctx, name, options)
	case "ingress":
		err = b.client.NetworkingV1().Ingresses(b.namespace).Delete(ctx, name, options)
	case "configmap":
		err = b.client.CoreV1().ConfigMaps(b.namespace).Delete(ctx, name, options)
	case "poddisruptionbudget":
		err = b.client.PolicyV1().PodDisruptionBudgets(b.namespace).Delete(ctx, name, options)
	case "networkpolicy":
		err = b.client.NetworkingV1().NetworkPolicies(b.namespace).Delete(ctx, name, options)
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
	desired.ResourceVersion = current.ResourceVersion
	_, err = items.Update(ctx, desired, metav1.UpdateOptions{})
	return err
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
	desired.ResourceVersion = current.ResourceVersion
	desired.Spec.ClusterIP = current.Spec.ClusterIP
	_, err = items.Update(ctx, desired, metav1.UpdateOptions{})
	return err
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
	desired.ResourceVersion = current.ResourceVersion
	_, err = items.Update(ctx, desired, metav1.UpdateOptions{})
	return err
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
	desired.ResourceVersion = current.ResourceVersion
	_, err = items.Update(ctx, desired, metav1.UpdateOptions{})
	return err
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
	desired.ResourceVersion = current.ResourceVersion
	_, err = items.Update(ctx, desired, metav1.UpdateOptions{})
	return err
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
	desired.ResourceVersion = current.ResourceVersion
	_, err = items.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func baseMeta(m manifest.Manifest, revision int, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: m.Name, Namespace: namespace, Labels: map[string]string{
		"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy, "plinth.dev/revision": fmt.Sprint(revision),
	}}
}

func deploymentFor(m manifest.Manifest, revision int, namespace string) *appsv1.Deployment {
	meta := baseMeta(m, revision, namespace)
	replicas := int32(m.Replicas)
	labels := map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy}
	cpu, _ := resource.ParseQuantity(m.Resources.CPU)
	memory, _ := resource.ParseQuantity(m.Resources.Memory)
	return &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)}, Containers: []corev1.Container{{Name: m.Name, Image: m.Image, Ports: []corev1.ContainerPort{{ContainerPort: int32(m.Port)}}, Env: envFor(m), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}, Limits: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true), RunAsNonRoot: ptr(true)}, LivenessProbe: probeFor(m), ReadinessProbe: probeFor(m)}}}}}}
}

func serviceFor(m manifest.Manifest, namespace string) *corev1.Service {
	meta := baseMeta(m, 0, namespace)
	labels := map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy}
	return &corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: int32(m.Port), TargetPort: intstr.FromInt(m.Port)}}}}
}

func ingressFor(m manifest.Manifest, namespace string) *networkingv1.Ingress {
	meta := baseMeta(m, 0, namespace)
	meta.Annotations = map[string]string{"cert-manager.io/cluster-issuer": "plinth-default", "plinth.dev/metrics": "enabled", "plinth.dev/logs": "structured"}
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{ObjectMeta: meta, Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{Hosts: []string{m.Name + ".plinth.local"}, SecretName: m.Name + "-tls"}}, Rules: []networkingv1.IngressRule{{Host: m.Name + ".plinth.local", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: m.Name, Port: networkingv1.ServiceBackendPort{Number: int32(m.Port)}}}}}}}}}}}
}

func configMapFor(m manifest.Manifest, namespace string) *corev1.ConfigMap {
	meta := baseMeta(m, 0, namespace)
	return &corev1.ConfigMap{ObjectMeta: meta, Data: m.Env}
}

func pdbFor(m manifest.Manifest, namespace string) *policyv1.PodDisruptionBudget {
	meta := baseMeta(m, 0, namespace)
	min := intstr.FromInt(1)
	return &policyv1.PodDisruptionBudget{ObjectMeta: meta, Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &min, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy}}}}
}

func networkPolicyFor(m manifest.Manifest, namespace string) *networkingv1.NetworkPolicy {
	meta := baseMeta(m, 0, namespace)
	return &networkingv1.NetworkPolicy{ObjectMeta: meta, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": m.Name, "app.kubernetes.io/managed-by": managedBy}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}}}
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
