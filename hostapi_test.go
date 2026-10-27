package main

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func hostCluster(objects ...runtime.Object) *Cluster {
	k := fakeCluster()
	m := k.mapper.(*meta.DefaultRESTMapper)
	for kind, scope := range map[string]meta.RESTScope{"HostStatus": meta.RESTScopeRoot, "HostRegistryTrust": meta.RESTScopeRoot, "HostRequest": meta.RESTScopeNamespace} {
		m.Add(schema.GroupVersionKind{Group: hostGroup, Version: hostVersion, Kind: kind}, scope)
	}
	k.client = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		hostRequests: "HostRequestList", hostStatuses: "HostStatusList", registryTrusts: "HostRegistryTrustList"}, objects...)
	return k
}

func hostStatusObject(reported string, memory int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": hostGroup + "/" + hostVersion, "kind": "HostStatus",
		"metadata": map[string]any{"name": "host"},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "HostReported", "status": reported, "reason": "Stale"}},
			"host":       map[string]any{"memoryMiB": map[string]any{"allocated": memory}},
			"guest":      map[string]any{"disk": map[string]any{"pressure": true}},
		}}}
}

func TestHostViewUnavailableWithoutAPI(t *testing.T) {
	if v := fakeCluster().hostView(context.Background(), "cncm-system"); v.State != "unavailable" {
		t.Fatalf("state %q", v.State)
	}
	if v := hostCluster().hostView(context.Background(), "cncm-system"); v.State != "unavailable" {
		t.Fatalf("CRD without a published HostStatus must be unavailable, got %q", v.State)
	}
}

func TestHostViewNeverShowsStaleFacts(t *testing.T) {
	v := hostCluster(hostStatusObject("Unknown", 4096)).hostView(context.Background(), "cncm-system")
	if v.State != "unknown" || v.Reason != "Stale" || v.Host != nil || len(v.Warnings) != 0 {
		t.Fatalf("stale facts leaked: %+v", v)
	}
}

func TestHostViewWarnsAboutMemoryAndDisk(t *testing.T) {
	v := hostCluster(hostStatusObject("True", 4096)).hostView(context.Background(), "cncm-system")
	if v.State != "available" || len(v.Warnings) != 2 {
		t.Fatalf("unexpected %+v", v)
	}
	v = hostCluster(hostStatusObject("True", 16384)).hostView(context.Background(), "cncm-system")
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "disk") {
		t.Fatalf("unexpected warnings %v", v.Warnings)
	}
}

func TestBuildHostRequestValidation(t *testing.T) {
	bad := []hostRequestInput{
		{Type: "resources", CPUs: 1, MemoryMiB: 8192}, {Type: "resources", CPUs: 4, MemoryMiB: 1024},
		{Type: "power", Action: "format"}, {Type: "other"}, {Type: "lan-check", Reason: strings.Repeat("x", 257)},
	}
	for _, in := range bad {
		if _, err := buildHostRequest("ns", in); err == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
	o, err := buildHostRequest("ns", hostRequestInput{Type: "resources", CPUs: 4, MemoryMiB: 8192, Reason: "build"})
	if err != nil {
		t.Fatal(err)
	}
	cpus, _, _ := unstructured.NestedInt64(o.Object, "spec", "resources", "cpus")
	if cpus != 4 || o.GetLabels()["app.kubernetes.io/managed-by"] != managedBy || !hostRequestName.MatchString(o.GetName()) {
		t.Fatalf("bad object %v", o.Object)
	}
}

func TestRequestsListedWithPhaseAndOwnLabelOnly(t *testing.T) {
	mine, _ := buildHostRequest("cncm-system", hostRequestInput{Type: "prune-images"})
	other, _ := buildHostRequest("cncm-system", hostRequestInput{Type: "lan-check"})
	other.SetLabels(nil)
	other.SetName("someone-else")
	_ = unstructured.SetNestedField(mine.Object, "Pending", "status", "phase")
	list := hostCluster(mine, other).listHostRequests(context.Background(), "cncm-system")
	if len(list) != 1 || list[0].Phase != "Pending" || list[0].Type != "prune-images" {
		t.Fatalf("unexpected %+v", list)
	}
}

func TestTrustableEndpoint(t *testing.T) {
	for ep, want := range map[string]bool{"localhost:30500": true, "127.0.0.1:30500": true, "192.168.1.5:5000": true,
		"registry.example.com:5000": false, "http://localhost:30500": false, "localhost": false, "localhost:0": false, "*:5000": false} {
		if trustableEndpoint(ep) != want {
			t.Fatalf("%s: want %v", ep, want)
		}
	}
}

func TestRegistryTrustObject(t *testing.T) {
	o := registryTrust("localhost:30500")
	plain, _, _ := unstructured.NestedBool(o.Object, "spec", "plainHTTP")
	if o.GetKind() != "HostRegistryTrust" || o.GetNamespace() != "" || !plain {
		t.Fatalf("bad trust %v", o.Object)
	}
}
