package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestKiteManifestIsGenerated(t *testing.T) {
	read := func(name string) string {
		b, err := manifestFiles.ReadFile("manifests/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := "# Generated from cncm.yaml and cncm-publication.yaml (CNCM_ORIGIN=auto); do not edit.\n# Paste this one file into Kite on Containers Anywhere 0.2.0+.\n" +
		strings.Replace(read("cncm.yaml"), "value: 'http://localhost:8080'", "value: auto", 1) + "---\n" + read("cncm-publication.yaml")
	if read("cncm-containers-anywhere.yaml") != want {
		t.Fatal("run `make kite-manifest`")
	}
	objects, err := parseManifests([]byte(read("cncm-containers-anywhere.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	for _, o := range objects {
		if o.GetKind() == "WorkloadPublication" {
			h, _, _ := unstructured.NestedString(o.Object, "spec", "hostname")
			if !strings.HasSuffix(h, ".cncm-system.local") && !strings.HasSuffix(h, ".cncm-system.localhost") {
				t.Fatalf("hostname %q is outside the namespace", h)
			}
			hosts[h] = true
		}
	}
	if len(hosts) != 2 {
		t.Fatalf("expected two publications, got %v", hosts)
	}
}

func TestCarePublication(t *testing.T) {
	objs := carePublication("https://care.care.local:8443")
	if len(objs) != 3 || objs[0].GetKind() != "Ingress" {
		t.Fatalf("unexpected objects %v", objs)
	}
	spec := func(i int, path ...string) any {
		v, _, _ := unstructured.NestedFieldNoCopy(objs[i].Object, append([]string{"spec"}, path...)...)
		return v
	}
	if spec(1, "hostname") != "care.care.local" || spec(1, "lan") != true || spec(1, "mdns") != true || spec(1, "tls", "mode") != "LocalCA" {
		t.Fatalf("bad .local publication %v", objs[1].Object["spec"])
	}
	if spec(2, "hostname") != "care.care.localhost" || spec(2, "lan") != false || spec(2, "mdns") != false {
		t.Fatalf("bad .localhost publication %v", objs[2].Object["spec"])
	}
	if o := carePublication("https://care.local"); len(o) != 3 || o[1].Object["spec"].(map[string]any)["hostname"] != "care.local" || o[2].Object["spec"].(map[string]any)["hostname"] != "care.localhost" {
		t.Fatal("the bare namespace name must be publishable")
	}
	if carePublication("http://care.care.local")[1].Object["spec"].(map[string]any)["tls"].(map[string]any)["mode"] != "HTTP" {
		t.Fatal("http address must request HTTP mode")
	}
	for _, u := range []string{"http://localhost:8090", "https://care.example.com", "https://other.local", "https://care.other.local", "https://care.care.local.evil.com"} {
		if carePublication(u) != nil {
			t.Fatalf("%s must not be published", u)
		}
	}
}

func TestPublicationSkippedWithoutAPI(t *testing.T) {
	k := fakeCluster()
	if k.hasKind("WorkloadPublication", "networking.containersanywhere.dev", "v1alpha1") {
		t.Fatal("API unexpectedly present")
	}
}

func TestBootstrapTokenIsCreatedOnceAndReused(t *testing.T) {
	k := fakeCluster()
	first, err := k.bootstrapToken(context.Background(), "cncm-system")
	if err != nil || len(first) < 24 {
		t.Fatalf("token %q, %v", first, err)
	}
	second, err := k.bootstrapToken(context.Background(), "cncm-system")
	if err != nil || second != first {
		t.Fatal("existing token must be reused, not regenerated")
	}
}

func TestBuildPlanFollowsAvailableCapacity(t *testing.T) {
	q := resource.MustParse
	if p := decideBuildPlan(q("8"), q("32Gi"), q("1"), q("6Gi")); !p.Parallel {
		t.Fatalf("roomy node should build in parallel: %s", p.Reason)
	}
	if p := decideBuildPlan(q("2"), q("8Gi"), q("500m"), q("3Gi")); p.Parallel {
		t.Fatalf("small node must build sequentially: %s", p.Reason)
	}
	if p := decideBuildPlan(q("16"), q("12Gi"), q("1"), q("4Gi")); p.Parallel {
		t.Fatal("memory short by headroom must stay sequential")
	}
	if p := decideBuildPlan(q("3"), q("64Gi"), q("0"), q("0")); p.Parallel {
		t.Fatal("CPU short must stay sequential")
	}
}

func planCluster(cpu, mem string, podRequests ...[2]string) *Cluster {
	k := fakeCluster()
	node := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "n1"},
		"status": map[string]any{"allocatable": map[string]any{"cpu": cpu, "memory": mem}}}}
	objs := []runtime.Object{node}
	for i, r := range podRequests {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": fmt.Sprintf("p%d", i), "namespace": "x"},
			"spec":     map[string]any{"containers": []any{map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": r[0], "memory": r[1]}}}}},
			"status":   map[string]any{"phase": "Running"}}})
	}
	k.client = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvr("", "v1", "nodes"): "NodeList", pods: "PodList"}, objs...)
	return k
}

func TestBuildPlanReadsNodeAndPodRequests(t *testing.T) {
	ctx := context.Background()
	if p := planCluster("8", "32Gi", [2]string{"1", "4Gi"}, [2]string{"500m", "2Gi"}).buildPlan(ctx); !p.Parallel {
		t.Fatalf("expected parallel: %s", p.Reason)
	}
	if p := planCluster("8", "16Gi", [2]string{"1", "4Gi"}, [2]string{"500m", "2Gi"}).buildPlan(ctx); p.Parallel {
		t.Fatalf("pods' requests must reduce what is free: %s", p.Reason)
	}
	if p := planCluster("not-a-quantity", "16Gi").buildPlan(ctx); p.Parallel {
		t.Fatal("unreadable capacity must fall back to sequential")
	}
}

func TestReconcileRefusesUnsafeStates(t *testing.T) {
	a := testApp(t)
	id, _ := a.begin("reconcile", "t")
	req := operationRequest{Kind: "reconcile"}
	a.kube = jobCluster()
	if err := a.reconcile(context.Background(), id, req, defaultSettings()); err == nil || !strings.Contains(err.Error(), "nothing to reconcile") {
		t.Fatalf("missing job: %v", err)
	}
	a.kube = jobCluster(initJob("img@sha256:aa", false))
	if err := a.reconcile(context.Background(), id, req, defaultSettings()); err == nil || !strings.Contains(err.Error(), "not completed") {
		t.Fatalf("running job: %v", err)
	}
	a.kube = jobCluster(initJob("img@sha256:aa", true))
	if err := a.reconcile(context.Background(), id, req, defaultSettings()); err == nil || !strings.Contains(err.Error(), "matches no pending release") {
		t.Fatalf("unmatched image: %v", err)
	}
	if _, err := a.db.Exec("INSERT INTO releases(id,backend_commit,frontend_commit,backend_image,frontend_image,status,created) VALUES('r1','b','f','img@sha256:aa','fe','built',?)", now()); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcile(context.Background(), id, operationRequest{Kind: "reconcile", ReleaseID: "other"}, defaultSettings()); err == nil || !strings.Contains(err.Error(), "belongs to release r1") {
		t.Fatalf("wrong release: %v", err)
	}
	var status string
	_ = a.db.QueryRow("SELECT status FROM releases WHERE id='r1'").Scan(&status)
	if status != "built" {
		t.Fatalf("a refused reconcile must not change the release, got %q", status)
	}
}

func jobCluster(objects ...runtime.Object) *Cluster {
	k := fakeCluster()
	k.client = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{jobs: "JobList", pods: "PodList"}, objects...)
	return k
}

func initJob(image string, complete bool) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "care-initialize", "namespace": namespace},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": image}}}}}}}
	if complete {
		o.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
	}
	return o
}
