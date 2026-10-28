package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Containers Anywhere host API (host.containersanywhere.dev/v1alpha1, docs/HOST_API.md
// in that project). It is optional: every function degrades to "unavailable" when the
// CRDs or the appliance bridge are missing, and nothing here is CNCM-specific.
const hostGroup = "host.containersanywhere.dev"
const hostVersion = "v1alpha1"

var hostStatuses = gvr(hostGroup, hostVersion, "hoststatuses")
var hostRequests = gvr(hostGroup, hostVersion, "hostrequests")
var registryTrusts = gvr(hostGroup, hostVersion, "hostregistrytrusts")

// Sum of the persistent volume claims CNCM creates (registry, Silo, Redis, database, CNCM).
const carePVCBytes int64 = 42 << 30

const registryTrustName = "cncm-registry"

type HostView struct {
	State    string             `json:"state"` // available | unknown | unavailable
	Reason   string             `json:"reason,omitempty"`
	Host     map[string]any     `json:"host,omitempty"`
	Guest    map[string]any     `json:"guest,omitempty"`
	Warnings []string           `json:"warnings"`
	Requests []HostRequestView  `json:"requests"`
	Registry *RegistryTrustView `json:"registry,omitempty"`
}

type HostRequestView struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Reason  string         `json:"reason,omitempty"`
	Phase   string         `json:"phase"`
	Why     string         `json:"why,omitempty"`
	Message string         `json:"message,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	Created string         `json:"created"`
}

type RegistryTrustView struct {
	Endpoint string `json:"endpoint"`
	Phase    string `json:"phase"`
	Reason   string `json:"reason,omitempty"`
}

type hostRequestInput struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
	Action    string `json:"action"`
	Enabled   bool   `json:"enabled"`
	Target    string `json:"target"`
}

func (k *Cluster) hasHostAPI() bool {
	return k.hasKind("HostStatus", hostGroup, hostVersion)
}

// hostView reads current host truth. A fact is only shown while the appliance
// reports HostReported=True; a stale or missing report is never replayed.
func (k *Cluster) hostView(ctx context.Context, ns string) HostView {
	v := HostView{Warnings: []string{}, Requests: []HostRequestView{}}
	if !k.hasHostAPI() {
		v.State, v.Reason = "unavailable", "This appliance or app predates the Containers Anywhere host API (0.3.0)."
		return v
	}
	o, err := k.client.Resource(hostStatuses).Get(ctx, "host", metav1.GetOptions{})
	if err != nil {
		v.State, v.Reason = "unavailable", "The appliance has not published host status (appliance older than 0.3.0 or bridge not running)."
		return v
	}
	v.State, v.Reason = "unknown", "NoHostFacts"
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "HostReported" {
			if m["status"] == "True" {
				v.State, v.Reason = "available", ""
			} else if r, ok := m["reason"].(string); ok {
				v.Reason = r
			}
		}
	}
	if v.State == "available" {
		v.Host, _, _ = unstructured.NestedMap(o.Object, "status", "host")
		v.Guest, _, _ = unstructured.NestedMap(o.Object, "status", "guest")
		v.Warnings = hostWarnings(v.Host, v.Guest)
	}
	v.Requests = k.listHostRequests(ctx, ns)
	if t, err := k.client.Resource(registryTrusts).Get(ctx, registryTrustName, metav1.GetOptions{}); err == nil {
		ep, _, _ := unstructured.NestedString(t.Object, "spec", "endpoint")
		ph, _, _ := unstructured.NestedString(t.Object, "status", "phase")
		rs, _, _ := unstructured.NestedString(t.Object, "status", "reason")
		v.Registry = &RegistryTrustView{Endpoint: ep, Phase: ph, Reason: rs}
	}
	return v
}

// hostWarnings turns facts into advice. CARE with its build needs roughly 8 GiB.
func hostWarnings(host, guest map[string]any) []string {
	var w []string
	if mem, ok := host["memoryMiB"].(map[string]any); ok {
		if a, ok := mem["allocated"].(int64); ok && a < 8192 {
			if r, ok := mem["requested"].(int64); !ok || r < 8192 {
				w = append(w, fmt.Sprintf("The appliance has %d MiB of memory; CARE plus its frontend build needs about 8192 MiB. Request a resize and approve it in the launcher.", a))
			}
		}
	}
	if disk, ok := guest["disk"].(map[string]any); ok && disk["pressure"] == true {
		w = append(w, "The appliance data disk is nearly full. Free space before building or backing up.")
	}
	if disk, ok := guest["disk"].(map[string]any); ok {
		if size, ok := disk["sizeBytes"].(int64); ok && size < carePVCBytes {
			w = append(w, fmt.Sprintf("The appliance disk is %.0f GiB; CARE's volumes request about %d GiB. Request disk growth (it never shrinks) and approve it in the launcher.", float64(size)/(1<<30), carePVCBytes>>30))
		}
	}
	if up, ok := host["update"].(map[string]any); ok {
		if p, _ := up["phase"].(string); p != "" && p != "idle" && p != "committed" {
			w = append(w, "An appliance update is in progress ("+p+").")
		}
	}
	return w
}

func (k *Cluster) listHostRequests(ctx context.Context, ns string) []HostRequestView {
	out := []HostRequestView{}
	list, err := k.client.Resource(hostRequests).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=" + managedBy})
	if err != nil {
		return out
	}
	for _, o := range list.Items {
		r := HostRequestView{Name: o.GetName(), Created: o.GetCreationTimestamp().UTC().Format(time.RFC3339)}
		r.Type, _, _ = unstructured.NestedString(o.Object, "spec", "type")
		r.Reason, _, _ = unstructured.NestedString(o.Object, "spec", "reason")
		r.Phase, _, _ = unstructured.NestedString(o.Object, "status", "phase")
		r.Why, _, _ = unstructured.NestedString(o.Object, "status", "reason")
		r.Message, _, _ = unstructured.NestedString(o.Object, "status", "message")
		r.Result, _, _ = unstructured.NestedMap(o.Object, "status", "result")
		if r.Phase == "" {
			r.Phase = "Waiting"
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func buildHostRequest(ns string, in hostRequestInput) (*unstructured.Unstructured, error) {
	if len(in.Reason) > 256 {
		return nil, errors.New("reason must be at most 256 characters")
	}
	spec := map[string]any{"type": in.Type}
	switch in.Type {
	case "resources":
		if in.CPUs < 2 || in.CPUs > 64 || in.MemoryMiB < 2048 || in.MemoryMiB > 262144 {
			return nil, errors.New("cpus must be 2–64 and memoryMiB 2048–262144")
		}
		res := map[string]any{"cpus": int64(in.CPUs), "memoryMiB": int64(in.MemoryMiB)}
		if in.DiskGiB != 0 {
			if in.DiskGiB < 8 || in.DiskGiB > 256 {
				return nil, errors.New("diskGiB must be 8–256")
			}
			res["diskGiB"] = int64(in.DiskGiB)
		}
		spec["resources"] = res
	case "power":
		if in.Action != "shutdown" && in.Action != "restart-appliance" {
			return nil, errors.New("action must be shutdown or restart-appliance")
		}
		spec["power"] = map[string]any{"action": in.Action}
	case "autostart":
		spec["autostart"] = map[string]any{"enabled": in.Enabled}
	case "prune-images", "lan-check":
	default:
		return nil, errors.New("unsupported request type")
	}
	if in.Reason != "" {
		spec["reason"] = in.Reason
	}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": hostGroup + "/" + hostVersion, "kind": "HostRequest",
		"metadata": map[string]any{"name": "cncm-" + in.Type + "-" + randomToken()[:6], "namespace": ns,
			"labels": map[string]any{"app.kubernetes.io/managed-by": managedBy}},
		"spec": spec}}
	return o, nil
}

func hostNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("CNCM_NAMESPACE")); ns != "" {
		return ns
	}
	return "cncm-system"
}

func (a *App) hostState(w http.ResponseWriter, r *http.Request) {
	if a.kube == nil {
		reply(w, 200, HostView{State: "unavailable", Reason: "No cluster selected.", Warnings: []string{}, Requests: []HostRequestView{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	reply(w, 200, a.kube.hostView(ctx, hostNamespace()))
}

func (a *App) createHostRequest(w http.ResponseWriter, r *http.Request) {
	var in hostRequestInput
	if !decode(w, r, &in) {
		return
	}
	if a.kube == nil || in.Target != a.kube.target {
		fail(w, 400, "confirm the exact cluster target")
		return
	}
	if !a.kube.hasKind("HostRequest", hostGroup, hostVersion) {
		fail(w, 409, "this appliance has no host API")
		return
	}
	o, err := buildHostRequest(hostNamespace(), in)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if _, err = a.kube.client.Resource(hostRequests).Namespace(hostNamespace()).Create(r.Context(), o, metav1.CreateOptions{}); err != nil {
		fail(w, 502, "the host request was refused; check CNCM's permissions")
		return
	}
	a.audit(a.user(r), "created host request "+in.Type+" "+o.GetName())
	reply(w, 202, map[string]string{"name": o.GetName()})
}

var hostRequestName = regexp.MustCompile(`^cncm-[a-z-]+-[a-f0-9]{6}$`)

func (a *App) deleteHostRequest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if a.kube == nil || !hostRequestName.MatchString(name) {
		fail(w, 400, "unknown request")
		return
	}
	err := a.kube.client.Resource(hostRequests).Namespace(hostNamespace()).Delete(r.Context(), name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		fail(w, 502, "cannot delete the request")
		return
	}
	a.audit(a.user(r), "deleted host request "+name)
	reply(w, 200, map[string]string{"status": "deleted"})
}

// trustableEndpoint reports whether the host API would accept the registry
// address: a loopback/node IP or "localhost", with a port, no scheme or path.
func trustableEndpoint(endpoint string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false
	}
	return host == "localhost" || net.ParseIP(host) != nil
}

func registryTrust(endpoint string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": hostGroup + "/" + hostVersion, "kind": "HostRegistryTrust",
		"metadata": map[string]any{"name": registryTrustName},
		"spec":     map[string]any{"endpoint": endpoint, "plainHTTP": true, "description": "CNCM in-cluster image registry"}}}
}

// trustRegistry asks the appliance to let containerd pull from the in-cluster
// registry. Where the host API is absent it only reports that the node must be
// configured by hand; it never edits node configuration itself.
func (a *App) trustRegistry(ctx context.Context, id, endpoint string) error {
	if !trustableEndpoint(endpoint) {
		return a.stage(id, "Registry address "+endpoint+" cannot be trusted through Containers Anywhere (use localhost or an IP); configure the node runtime yourself")
	}
	if !a.kube.hasKind("HostRegistryTrust", hostGroup, hostVersion) {
		return a.stage(id, "Containers Anywhere host API not found; configure the node runtime to trust "+endpoint+" yourself")
	}
	if err := a.stage(id, "Asking the appliance to trust registry "+endpoint); err != nil {
		return err
	}
	if err := a.kube.apply(ctx, registryTrust(endpoint)); err != nil {
		return err
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		t, err := a.kube.client.Resource(registryTrusts).Get(ctx, registryTrustName, metav1.GetOptions{})
		if err == nil {
			phase, _, _ := unstructured.NestedString(t.Object, "status", "phase")
			reason, _, _ := unstructured.NestedString(t.Object, "status", "reason")
			switch phase {
			case "Trusted":
				return nil
			case "Rejected", "Revoked":
				return fmt.Errorf("the appliance did not trust the registry (%s %s)", phase, reason)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return a.stage(id, "The appliance did not report registry trust within 45 seconds (bridge may be off); image pulls may fail until the node trusts "+endpoint)
}
