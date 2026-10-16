package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func fakeCluster(objects ...runtime.Object) *Cluster {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: "apps", Version: "v1"}, {Group: "batch", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	return &Cluster{client: fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...), mapper: mapper, target: "fake-only"}
}

func TestSecretsPreservedAndUnmanagedResourcesRefused(t *testing.T) {
	ctx := context.Background()
	existing := object("Secret", "credentials", map[string]any{"data": map[string]any{"password": "cHJlc2VydmU="}})
	existing.SetLabels(map[string]string{"app.kubernetes.io/managed-by": managedBy})
	k := fakeCluster(existing)
	if err := k.createSecret(ctx, "credentials", map[string]string{"password": "replacement"}); err != nil {
		t.Fatal(err)
	}
	got, err := k.client.Resource(secrets).Namespace(namespace).Get(ctx, "credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := unstructured.NestedString(got.Object, "data", "password")
	if v != "cHJlc2VydmU=" {
		t.Fatal("existing secret rotated")
	}
	unmanaged := object("ConfigMap", "existing", map[string]any{"data": map[string]any{"key": "user-work"}})
	k = fakeCluster(unmanaged)
	if err := k.apply(ctx, unmanaged.DeepCopy()); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("adopted unmanaged object: %v", err)
	}
}

func TestApplyDoesNotForceConflicts(t *testing.T) {
	k := fakeCluster()
	called := false
	k.client.(*fake.FakeDynamicClient).PrependReactor("patch", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		called = true
		p := action.(ktesting.PatchActionImpl)
		if p.GetPatchOptions().Force != nil && *p.GetPatchOptions().Force {
			t.Fatal("forced ownership conflict")
		}
		if p.GetPatchOptions().FieldManager != managedBy {
			t.Fatal("missing field manager")
		}
		var body map[string]any
		if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
			t.Fatal(err)
		}
		o := &unstructured.Unstructured{Object: body}
		if o.GetLabels()["app.kubernetes.io/managed-by"] != managedBy {
			t.Fatal("missing ownership label")
		}
		return true, o, nil
	})
	if err := k.apply(context.Background(), object("ConfigMap", "config", map[string]any{"data": map[string]any{"key": "value"}})); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("no patch")
	}
}

func TestManifestContracts(t *testing.T) {
	files, err := manifestFiles.ReadDir("manifests")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		t.Run(file.Name(), func(t *testing.T) {
			data, err := manifestFiles.ReadFile("manifests/" + file.Name())
			if err != nil {
				t.Fatal(err)
			}
			objects, err := parseManifests(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range objects {
				if strings.Contains(o.GetAPIVersion(), "gke") || strings.Contains(o.GetAPIVersion(), "helm") {
					t.Fatal("cloud-specific deployment dependency")
				}
				if o.GetKind() == "Deployment" {
					replicas, _, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
					if replicas != 1 {
						t.Fatalf("%s replicas=%d", o.GetName(), replicas)
					}
				}
				if o.GetKind() == "Job" {
					retries, _, _ := unstructured.NestedInt64(o.Object, "spec", "backoffLimit")
					if retries != 0 {
						t.Fatal("Jobs must not blindly retry")
					}
				}
				if o.GetName() == "care-beat" {
					strategy, _, _ := unstructured.NestedString(o.Object, "spec", "strategy", "type")
					if strategy != "Recreate" {
						t.Fatal("beat may overlap during rollout")
					}
					b, _ := json.Marshal(o.Object)
					if strings.Contains(string(b), "celery_beat.sh") || strings.Contains(string(b), "manage.py migrate") {
						t.Fatal("beat must not run migrations")
					}
				}
			}
		})
	}
}

func TestBuildPinsSourceAndCapturesDigest(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, component := range []string{"backend", "frontend"} {
		o, err := buildManifest("test123", component, sha)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(o.Object)
		s := string(b)
		for _, required := range []string{sha, "rev-parse HEAD", "registry.care.svc.cluster.local:5000", "/dev/termination-log", "containerimage.digest"} {
			if !strings.Contains(s, required) {
				t.Errorf("%s build missing %q", component, required)
			}
		}
		mount, _, _ := unstructured.NestedBool(o.Object, "spec", "template", "spec", "automountServiceAccountToken")
		if mount {
			t.Fatal("build received management credentials")
		}
	}
	if _, err := buildManifest("id", "backend", "develop;bad"); err == nil {
		t.Fatal("unresolved source accepted")
	}
}

func TestDigestUsesBuildContainerTerminationMessage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	p := object("Pod", "build-pod", map[string]any{"status": map[string]any{"containerStatuses": []any{map[string]any{"name": "build", "state": map[string]any{"terminated": map[string]any{"message": digest}}}}}})
	p.SetLabels(map[string]string{"job-name": "build-test"})
	a := testApp(t)
	a.kube = fakeCluster(p)
	got, err := a.buildDigest(context.Background(), "build-test")
	if err != nil || got != digest {
		t.Fatalf("digest=%s err=%v", got, err)
	}
}

func TestReadinessRequiresObservedGenerationAndUpdatedReplicas(t *testing.T) {
	o := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"generation": int64(2)}, "spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"observedGeneration": int64(1), "availableReplicas": int64(1), "updatedReplicas": int64(1)}}}
	if ready, _ := deploymentReady(o); ready {
		t.Fatal("stale generation reported Ready")
	}
	_ = unstructured.SetNestedField(o.Object, int64(2), "status", "observedGeneration")
	if ready, _ := deploymentReady(o); !ready {
		t.Fatal("ready deployment rejected")
	}
}

func TestFailedJobIsNotRetried(t *testing.T) {
	o := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "migration"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True"}}}}}
	if ready, err := jobComplete(o); ready || err == nil {
		t.Fatal("failed migration treated as success/pending")
	}
}

func TestSecretRedaction(t *testing.T) {
	for _, input := range []string{"postgres://user:password@example/db", "TOKEN=abc-secret", "PASSWORD=top-secret"} {
		out := redact(input)
		if strings.Contains(out, "password@") || strings.Contains(out, "abc-secret") || strings.Contains(out, "top-secret") {
			t.Errorf("credential leaked: %s", out)
		}
	}
	s := object("Secret", "test", map[string]any{"data": map[string]any{"password": "c3VwZXItc2VjcmV0"}})
	k := fakeCluster(s)
	got := k.redactSecrets(context.Background(), "arbitrary text super-secret c3VwZXItc2VjcmV0")
	if strings.Contains(got, "super-secret") || strings.Contains(got, "c3VwZXItc2VjcmV0") {
		t.Fatal("known secret not redacted")
	}
}

func TestLocalClusterSelectionIgnoresAmbientContext(t *testing.T) {
	t.Setenv("CNCM_KUBE_CONTEXT", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", "/does/not/exist")
	k, err := newCluster()
	if err != nil || k != nil {
		t.Fatalf("ambient kubeconfig loaded: %v %v", k, err)
	}
}

func TestInitializationGuardPreventsMigrationReplay(t *testing.T) {
	a := testApp(t)
	// A fake one-node cluster and default storage satisfy preflight; existing Job
	// must stop installation before any credential writes or deployment mutations.
	node := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "test"}, "status": map[string]any{"nodeInfo": map[string]any{"architecture": "arm64"}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}
	sc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": "local-path", "annotations": map[string]any{"storageclass.kubernetes.io/is-default-class": "true"}}}}
	job := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": "care-initialize", "namespace": namespace}}}
	a.kube = fakeCluster(node, sc, job)
	id, err := a.begin("install", "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.db.Exec("INSERT INTO releases VALUES('release',?,?,'backend@digest','frontend@digest','built',?)", strings.Repeat("a", 40), strings.Repeat("b", 40), now())
	if err != nil {
		t.Fatal(err)
	}
	err = a.install(context.Background(), id, operationRequest{ReleaseID: "release"}, defaultSettings())
	if err == nil || !strings.Contains(err.Error(), "automatic migration retry is blocked") {
		t.Fatalf("migration replay not blocked: %v", err)
	}
	for _, action := range a.kube.client.(*fake.FakeDynamicClient).Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatal(fmt.Sprintf("guard performed mutation: %s", action.GetVerb()))
		}
	}
}
