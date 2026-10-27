package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed manifests/*.yaml
var manifestFiles embed.FS

const namespace = "care"
const managedBy = "cncm"

type Cluster struct {
	client dynamic.Interface
	mapper meta.RESTMapper
	http   *http.Client
	host   string
	target string
}

func newCluster() (*Cluster, error) {
	var cfg *rest.Config
	var err error
	target := os.Getenv("CNCM_KUBE_CONTEXT")
	if target != "" {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: target}).ClientConfig()
	} else if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		cfg, err = rest.InClusterConfig()
		target = "in-cluster"
	} else {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("explicit cluster configuration: %w", err)
	}
	cfg.Timeout = 30 * time.Second
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dis, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	hc.Timeout = 30 * time.Second
	return &Cluster{client: dc, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dis)), http: hc, host: cfg.Host, target: target}, nil
}

func parseManifests(data []byte) ([]*unstructured.Unstructured, error) {
	d := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	out := []*unstructured.Unstructured{}
	for {
		var raw map[string]any
		if err := d.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		// Kubernetes unstructured values require int64, not encoding/json's
		// float64 defaults, for replicas, deadlines and resource versions.
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(encoded); err != nil {
			return nil, err
		}
		if obj.GetName() == "" || obj.GetKind() == "" {
			return nil, errors.New("manifest missing name or kind")
		}
		out = append(out, obj)
	}
	return out, nil
}

func manifest(name string) ([]*unstructured.Unstructured, error) {
	b, err := manifestFiles.ReadFile("manifests/" + name + ".yaml")
	if err != nil {
		return nil, err
	}
	return parseManifests(b)
}

func (k *Cluster) resource(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	m, err := k.mapper.RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		return k.client.Resource(m.Resource).Namespace(obj.GetNamespace()), nil
	}
	return k.client.Resource(m.Resource), nil
}

func (k *Cluster) hasKind(kind, group, version string) bool {
	gk := schema.GroupKind{Group: group, Kind: kind}
	if _, err := k.mapper.RESTMapping(gk, version); err == nil {
		return true
	}
	// CRDs may have been installed after the discovery cache was filled.
	if reset, ok := k.mapper.(interface{ Reset() }); ok {
		reset.Reset()
		_, err := k.mapper.RESTMapping(gk, version)
		return err == nil
	}
	return false
}

// bootstrapToken returns the setup token stored in Secret ns/cncm-bootstrap,
// creating it with a random value when absent. Operators read it in Kite.
func (k *Cluster) bootstrapToken(ctx context.Context, ns string) (string, error) {
	r := k.client.Resource(secrets).Namespace(ns)
	existing, err := r.Get(ctx, "cncm-bootstrap", metav1.GetOptions{})
	if err == nil {
		if v, _, _ := unstructured.NestedString(existing.Object, "data", "token"); v != "" {
			if b, e := decodeBase64(v); e == nil {
				return string(b), nil
			}
		}
		if v, _, _ := unstructured.NestedString(existing.Object, "stringData", "token"); v != "" {
			return v, nil
		}
		return "", errors.New("Secret cncm-bootstrap exists without a token key")
	}
	if !apierrors.IsNotFound(err) {
		return "", errors.New("cannot inspect Secret cncm-bootstrap")
	}
	token := randomToken()
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": "cncm-bootstrap", "namespace": ns, "labels": map[string]any{"app.kubernetes.io/managed-by": managedBy}},
		"stringData": map[string]any{"token": token}}}
	if _, err = r.Create(ctx, o, metav1.CreateOptions{}); err != nil {
		return "", errors.New("cannot create Secret cncm-bootstrap")
	}
	return token, nil
}

func (k *Cluster) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app.kubernetes.io/managed-by"] = managedBy
	obj.SetLabels(labels)
	r, err := k.resource(obj)
	if err != nil {
		return err
	}
	existing, err := r.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("inspect %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	if err == nil && existing.GetLabels()["app.kubernetes.io/managed-by"] != managedBy {
		return fmt.Errorf("refusing to adopt existing %s/%s", obj.GetKind(), obj.GetName())
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	_, err = r.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: managedBy})
	// Never include the server response for Secret objects: validation errors may echo values.
	if err != nil && obj.GetKind() == "Secret" {
		return fmt.Errorf("failed to apply Secret/%s", obj.GetName())
	}
	return err
}

func (k *Cluster) applyFile(ctx context.Context, name string, edit func(*unstructured.Unstructured) error) error {
	objects, err := manifest(name)
	if err != nil {
		return err
	}
	for _, o := range objects {
		if edit != nil {
			if err = edit(o); err != nil {
				return err
			}
		}
		if err = k.apply(ctx, o); err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

var deployments = gvr("apps", "v1", "deployments")
var jobs = gvr("batch", "v1", "jobs")
var pods = gvr("", "v1", "pods")
var secrets = gvr("", "v1", "secrets")
var clusters = gvr("postgresql.cnpg.io", "v1", "clusters")

func (k *Cluster) wait(ctx context.Context, resource schema.GroupVersionResource, ns, name string, ready func(*unstructured.Unstructured) (bool, error)) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		o, err := k.client.Resource(resource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			ok, err := ready(o)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s/%s: %w", resource.Resource, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func deploymentReady(o *unstructured.Unstructured) (bool, error) {
	generation, _, _ := unstructured.NestedInt64(o.Object, "status", "observedGeneration")
	desired, found, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
	if !found {
		desired = 1
	}
	available, _, _ := unstructured.NestedInt64(o.Object, "status", "availableReplicas")
	updated, _, _ := unstructured.NestedInt64(o.Object, "status", "updatedReplicas")
	return generation >= o.GetGeneration() && available >= desired && updated >= desired, nil
}
func jobComplete(o *unstructured.Unstructured) (bool, error) {
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if c["status"] == "True" {
			if c["type"] == "Failed" {
				return false, fmt.Errorf("Job %s failed; inspect pod logs", o.GetName())
			}
			if c["type"] == "Complete" {
				return true, nil
			}
		}
	}
	return false, nil
}
func conditionReady(o *unstructured.Unstructured) (bool, error) {
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if c["type"] == "Ready" && c["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) clusterStatus(w http.ResponseWriter, r *http.Request) {
	if a.kube == nil {
		reply(w, 200, map[string]any{"pods": []any{}, "events": []any{}, "error": "No cluster selected. The portal works without one."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	list, err := a.kube.client.Resource(pods).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fail(w, 502, "cannot list CARE pods; check cluster access and namespace")
		return
	}
	out := []map[string]any{}
	for _, p := range list.Items {
		phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
		statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
		ready, total, restarts := 0, 0, int64(0)
		reason := ""
		for _, raw := range statuses {
			c, _ := raw.(map[string]any)
			total++
			if c["ready"] == true {
				ready++
			}
			if n, ok := c["restartCount"].(int64); ok {
				restarts += n
			}
			if state, ok := c["state"].(map[string]any); ok {
				if waiting, ok := state["waiting"].(map[string]any); ok {
					reason, _ = waiting["reason"].(string)
				}
			}
		}
		cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
		names := []string{}
		for _, c := range cs {
			names = append(names, c.(map[string]any)["name"].(string))
		}
		init, _, _ := unstructured.NestedSlice(p.Object, "spec", "initContainers")
		for _, c := range init {
			names = append(names, c.(map[string]any)["name"].(string))
		}
		out = append(out, map[string]any{"name": p.GetName(), "phase": phase, "ready": ready, "total": total, "restarts": restarts, "reason": reason, "containers": names})
	}
	events := []map[string]string{}
	ev, err := a.kube.client.Resource(gvr("", "v1", "events")).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 50})
	if err == nil {
		for _, e := range ev.Items {
			reason, _, _ := unstructured.NestedString(e.Object, "reason")
			message, _, _ := unstructured.NestedString(e.Object, "message")
			events = append(events, map[string]string{"reason": reason, "message": redact(message)})
		}
	}
	reply(w, 200, map[string]any{"pods": out, "events": events})
}

var podName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)
var credentialURL = regexp.MustCompile(`(https?|postgres(?:ql)?|redis)://[^\s/@]+:[^\s/@]+@`)
var secretAssignment = regexp.MustCompile(`(?i)(password|secret|token|authorization|access_key|bucket_key|jwks|vapid)([a-z_]*\s*[=:]\s*)[^\s,;]+`)

func redact(s string) string {
	s = credentialURL.ReplaceAllString(s, "$1://[redacted]@")
	return secretAssignment.ReplaceAllString(s, "$1$2[redacted]")
}
func (k *Cluster) podLogs(ctx context.Context, pod, container string) (string, error) {
	q := url.Values{"tailLines": {"150"}, "limitBytes": {"32768"}, "container": {container}}
	req, err := http.NewRequestWithContext(ctx, "GET", k.host+"/api/v1/namespaces/"+namespace+"/pods/"+pod+"/log?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	res, err := k.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", errors.New("logs unavailable; container may not have started")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 32768))
	return redact(string(b)), err
}
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	if a.kube == nil {
		fail(w, 409, "no cluster selected")
		return
	}
	p, c := r.URL.Query().Get("pod"), r.URL.Query().Get("container")
	if !podName.MatchString(p) || !podName.MatchString(c) {
		fail(w, 400, "invalid pod/container")
		return
	}
	text, err := a.kube.podLogs(r.Context(), p, c)
	if err != nil {
		fail(w, 502, "logs unavailable; container may not have started")
		return
	}
	reply(w, 200, map[string]string{"text": a.kube.redactSecrets(r.Context(), text)})
}

func setContainer(o *unstructured.Unstructured, index int, edit func(map[string]any)) error {
	cs, found, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || index >= len(cs) {
		return errors.New("manifest container missing")
	}
	c, ok := cs[index].(map[string]any)
	if !ok {
		return errors.New("invalid container")
	}
	edit(c)
	return unstructured.SetNestedSlice(o.Object, cs, "spec", "template", "spec", "containers")
}

func stringMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func object(kind, name string, fields map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}}}
	for k, v := range fields {
		o.Object[k] = v
	}
	return o
}

func (k *Cluster) createSecret(ctx context.Context, name string, data map[string]string) error {
	o := object("Secret", name, map[string]any{"type": "Opaque", "stringData": stringMap(data)})
	if name == "care-db-auth" {
		o.Object["type"] = "kubernetes.io/basic-auth"
	}
	existing, err := k.client.Resource(secrets).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if existing.GetLabels()["app.kubernetes.io/managed-by"] != managedBy {
			return fmt.Errorf("Secret/%s already exists and is not CNCM-owned", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("cannot inspect Secret/%s", name)
	}
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": managedBy})
	_, err = k.client.Resource(secrets).Namespace(namespace).Create(ctx, o, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("cannot create Secret/%s", name)
	}
	return nil
}

func (k *Cluster) redactSecrets(ctx context.Context, text string) string {
	// Logs are limited to care namespace; known secret values are removed as well
	// as common credential patterns. Never return Secret objects to the browser.
	list, err := k.client.Resource(secrets).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "Logs withheld: could not load redaction values."
	}
	for _, s := range list.Items {
		data, _, _ := unstructured.NestedStringMap(s.Object, "data")
		for _, v := range data {
			if decoded, err := decodeBase64(v); err == nil && len(decoded) >= 4 {
				text = strings.ReplaceAll(text, string(decoded), "[redacted]")
				text = strings.ReplaceAll(text, v, "[redacted]")
			}
		}
	}
	return redact(text)
}
