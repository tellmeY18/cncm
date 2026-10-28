package main

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func snapCluster(objects ...runtime.Object) *Cluster {
	k := fakeCluster()
	m := k.mapper.(*meta.DefaultRESTMapper)
	m.Add(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"}, meta.RESTScopeNamespace)
	k.client = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		volumeSnapshots: "VolumeSnapshotList", volumeSnapshotClasses: "VolumeSnapshotClassList", cnpgBackups: "BackupList"}, objects...)
	return k
}

func snapClass(name string, def bool) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotClass", "metadata": map[string]any{"name": name}, "driver": "x"}}
	if def {
		o.SetAnnotations(map[string]string{"snapshot.storage.kubernetes.io/is-default-class": "true"})
	}
	return o
}

func TestSnapshotSupportSelection(t *testing.T) {
	ctx := context.Background()
	if s := fakeCluster().snapshotSupport(ctx); s.Available {
		t.Fatal("no API must be unavailable")
	}
	if s := snapCluster().snapshotSupport(ctx); s.Available {
		t.Fatal("no class must be unavailable")
	}
	if s := snapCluster(snapClass("only", false)).snapshotSupport(ctx); !s.Available || s.Class != "only" {
		t.Fatalf("single class: %+v", s)
	}
	if s := snapCluster(snapClass("a", false), snapClass("b", true)).snapshotSupport(ctx); s.Class != "b" {
		t.Fatalf("default class: %+v", s)
	}
	if s := snapCluster(snapClass("a", false), snapClass("b", false)).snapshotSupport(ctx); s.Available {
		t.Fatal("ambiguous classes must not be guessed")
	}
}

func TestSnapshotObjects(t *testing.T) {
	f := filesSnapshot("0123456789ab", "btrfs")
	src, _, _ := unstructured.NestedString(f.Object, "spec", "source", "persistentVolumeClaimName")
	if f.GetName() != "rp-0123456789ab-files" || src != filesPVC || f.GetNamespace() != namespace || f.GetLabels()[pointLabel] != "0123456789ab" {
		t.Fatalf("bad files snapshot %v", f.Object)
	}
	b := dbBackup("0123456789ab")
	method, _, _ := unstructured.NestedString(b.Object, "spec", "method")
	if method != "volumeSnapshot" || b.GetAPIVersion() != "postgresql.cnpg.io/v1" {
		t.Fatalf("bad backup %v", b.Object)
	}
}

func TestBackupCompletionAndNames(t *testing.T) {
	o := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": "failed", "error": "password=hunter2 boom"}}}
	if _, err := backupCompleted(o); err == nil || containsSecret(err.Error()) {
		t.Fatalf("failed backup must error without secrets: %v", err)
	}
	o = &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": "completed",
		"backupSnapshotStatus": map[string]any{"elements": []any{map[string]any{"name": "snap-1"}, map[string]any{"name": "snap-2"}}}}}}
	if ok, _ := backupCompleted(o); !ok {
		t.Fatal("completed must be ready")
	}
	if n := backupSnapshotNames(o); len(n) != 2 || n[1] != "snap-2" {
		t.Fatalf("names %v", n)
	}
}

func containsSecret(s string) bool { return len(s) > 0 && (indexOf(s, "hunter2") >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func snapObject(name string, ready bool) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
		"metadata": map[string]any{"name": name, "namespace": namespace}, "status": map[string]any{"readyToUse": ready}}}
	return o
}

func insertPoint(t *testing.T, a *App, id, status string, snaps []string) {
	t.Helper()
	raw, _ := json.Marshal(snaps)
	if _, err := a.db.Exec("INSERT INTO recovery_points(id,created,consistency,release_id,status,snapshots,db_backup) VALUES(?,?,?,?,?,?,?)", id, now(), "stopped", "r1", status, string(raw), "rp-"+id+"-db"); err != nil {
		t.Fatal(err)
	}
}

func TestPointHealthReportsMissingAndNotReady(t *testing.T) {
	a := testApp(t)
	a.kube = snapCluster(snapObject("s-ready", true), snapObject("s-wait", false))
	ctx := context.Background()
	if h := a.pointHealth(ctx, RecoveryPoint{Status: "ready", Snapshots: []string{"s-ready"}}); h != "ready" {
		t.Fatal(h)
	}
	if h := a.pointHealth(ctx, RecoveryPoint{Status: "ready", Snapshots: []string{"s-ready", "s-wait"}}); h != "notReady" {
		t.Fatal(h)
	}
	if h := a.pointHealth(ctx, RecoveryPoint{Status: "ready", Snapshots: []string{"s-ready", "gone"}}); h != "missing" {
		t.Fatal(h)
	}
	if h := a.pointHealth(ctx, RecoveryPoint{Status: "failed"}); h != "unknown" {
		t.Fatal(h)
	}
}

func TestDeletePointRemovesOnlyRecordedSnapshots(t *testing.T) {
	a := testApp(t)
	a.kube = snapCluster(snapObject("mine", true), snapObject("unrelated", true))
	insertPoint(t, a, "aaaaaaaaaaaa", "ready", []string{"mine"})
	insertPoint(t, a, "bbbbbbbbbbbb", "creating", nil)
	id, _ := a.begin("snapshot-delete", "t")
	ctx := context.Background()
	if err := a.deletePoint(ctx, id, "bbbbbbbbbbbb"); err == nil {
		t.Fatal("a point that is still being created must not be deletable")
	}
	if err := a.deletePoint(ctx, id, "aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.kube.client.Resource(volumeSnapshots).Namespace(namespace).Get(ctx, "mine", metav1.GetOptions{}); err == nil {
		t.Fatal("recorded snapshot should be deleted")
	}
	if _, err := a.kube.client.Resource(volumeSnapshots).Namespace(namespace).Get(ctx, "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatal("unrelated snapshot must survive")
	}
	pts, err := a.recoveryPoints(ctx)
	if err != nil || len(pts) != 1 || pts[0].ID != "bbbbbbbbbbbb" {
		t.Fatalf("deleted point must be hidden: %+v %v", pts, err)
	}
}

func TestSnapshotRequestValidation(t *testing.T) {
	if !validConsistency("stopped") || !validConsistency("live") || validConsistency("") || validConsistency("x") {
		t.Fatal("consistency validation")
	}
	if !hasPointID("0123456789ab") || hasPointID("../etc/passwd") || hasPointID("0123456789abc") {
		t.Fatal("point id validation")
	}
}

func TestDiskGrowthRequestAndWarning(t *testing.T) {
	if _, err := buildHostRequest("ns", hostRequestInput{Type: "resources", CPUs: 4, MemoryMiB: 8192, DiskGiB: 4}); err == nil {
		t.Fatal("disk below 8 GiB accepted")
	}
	o, err := buildHostRequest("ns", hostRequestInput{Type: "resources", CPUs: 4, MemoryMiB: 8192, DiskGiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	if d, _, _ := unstructured.NestedInt64(o.Object, "spec", "resources", "diskGiB"); d != 64 {
		t.Fatal("diskGiB not set")
	}
	w := hostWarnings(map[string]any{}, map[string]any{"disk": map[string]any{"sizeBytes": int64(8 << 30)}})
	if len(w) != 1 {
		t.Fatalf("expected a disk-size warning, got %v", w)
	}
}
