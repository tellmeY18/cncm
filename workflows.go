package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const operatorURL = "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml"
const operatorSHA = "37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb"

type operationRequest struct {
	Kind          string `json:"kind"`
	ReleaseID     string `json:"releaseId,omitempty"`
	AdminUsername string `json:"adminUsername,omitempty"`
	AdminPassword string `json:"adminPassword,omitempty"`
	AdminEmail    string `json:"adminEmail,omitempty"`
	Target        string `json:"target"`
}

func (a *App) startOperation(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kube == nil {
		fail(w, 409, "no cluster selected; set CNCM_KUBE_CONTEXT explicitly")
		return
	}
	var req operationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Target != a.kube.target {
		fail(w, 400, "confirm the exact cluster target")
		return
	}
	switch req.Kind {
	case "preflight", "prepare", "build", "install", "restart", "stop", "start":
	default:
		fail(w, 400, "unsupported operation; upgrades and restore are not enabled in this POC")
		return
	}
	if req.Kind == "install" {
		if !usernamePattern.MatchString(req.AdminUsername) || len(req.AdminPassword) < 12 || len(req.AdminPassword) > 128 || !strings.Contains(req.AdminEmail, "@") || len(req.AdminEmail) > 254 {
			fail(w, 400, "provide CARE administrator username, email and a password of at least 12 characters")
			return
		}
		var n int
		if err := a.db.QueryRow("SELECT count(*) FROM releases WHERE status IN ('installed','installing','install-uncertain')").Scan(&n); err != nil {
			fail(w, 500, "cannot inspect releases")
			return
		}
		if n > 0 {
			fail(w, 409, "CARE has an installed or uncertain installation; migration-safe upgrades/retries are not yet enabled")
			return
		}
		var status string
		if err := a.db.QueryRow("SELECT status FROM releases WHERE id=?", req.ReleaseID).Scan(&status); err != nil || status != "built" {
			fail(w, 400, "select a successfully built release")
			return
		}
	}
	if req.Kind == "restart" || req.Kind == "stop" || req.Kind == "start" {
		var n int
		if err := a.db.QueryRow("SELECT count(*) FROM releases WHERE status='installed'").Scan(&n); err != nil || n == 0 {
			fail(w, 409, "CARE must be installed first")
			return
		}
	}
	s, err := a.settings()
	if err != nil {
		fail(w, 500, "cannot read settings")
		return
	}
	id, err := a.begin(req.Kind, a.user(r))
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	a.audit(a.user(r), "requested "+req.Kind+" operation "+id)
	go a.runOperation(id, req, s)
	reply(w, 202, map[string]string{"id": id})
}

func (a *App) runOperation(id string, req operationRequest, s Settings) {
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Hour)
	defer cancel()
	var err error
	switch req.Kind {
	case "preflight":
		err = a.preflight(ctx, id)
	case "prepare":
		err = a.prepare(ctx, id)
	case "build":
		err = a.build(ctx, id, s)
	case "install":
		err = a.install(ctx, id, req, s)
	case "restart", "stop", "start":
		err = a.control(ctx, id, req.Kind)
	}
	status, message := "succeeded", "Completed"
	if err != nil {
		status = "uncertain"
		message = redact(err.Error())
		if req.Kind == "preflight" {
			status = "failed"
		}
		if len(message) > 1500 {
			message = message[:1500]
		}
	}
	_, _ = a.db.Exec("UPDATE operations SET status=?,message=?,updated=? WHERE id=?", status, message, now(), id)
}

func (a *App) preflight(ctx context.Context, id string) error {
	if err := a.stage(id, "Checking explicit cluster target, node readiness and storage"); err != nil {
		return err
	}
	list, err := a.kube.client.Resource(gvr("", "v1", "nodes")).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("cannot list nodes; check cluster access and permissions")
	}
	if len(list.Items) != 1 {
		return errors.New("this POC requires exactly one node")
	}
	n := list.Items[0]
	arch, _, _ := unstructured.NestedString(n.Object, "status", "nodeInfo", "architecture")
	if arch != "arm64" && arch != "amd64" {
		return fmt.Errorf("unsupported node architecture %s", arch)
	}
	ready, _ := conditionReady(&n)
	if !ready {
		return errors.New("node is not Ready")
	}
	scs, err := a.kube.client.Resource(gvr("storage.k8s.io", "v1", "storageclasses")).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("cannot list storage classes")
	}
	for _, sc := range scs.Items {
		if sc.GetAnnotations()["storageclass.kubernetes.io/is-default-class"] == "true" {
			return nil
		}
	}
	return errors.New("a default StorageClass is required")
}

func download(ctx context.Context, address string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CNCM/0.1")
	client := &http.Client{Timeout: 90 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("download returned HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("download exceeded limit")
	}
	return b, nil
}

func (a *App) prepare(ctx context.Context, id string) error {
	if err := a.preflight(ctx, id); err != nil {
		return err
	}
	if err := a.stage(id, "Installing pinned CloudNativePG operator"); err != nil {
		return err
	}
	data, err := download(ctx, operatorURL, 8<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != operatorSHA {
		return errors.New("CNPG manifest checksum mismatch")
	}
	objects, err := parseManifests(data)
	if err != nil {
		return err
	}
	for _, o := range objects {
		if err = a.kube.apply(ctx, o); err != nil {
			return err
		}
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err = a.kube.wait(readyCtx, deployments, "cnpg-system", "cnpg-controller-manager", deploymentReady); err != nil {
		return err
	}
	if reset, ok := a.kube.mapper.(interface{ Reset() }); ok {
		reset.Reset()
	}
	if err = a.stage(id, "Creating persistent infrastructure and credentials"); err != nil {
		return err
	}
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace}}}
	if err = a.kube.apply(ctx, ns); err != nil {
		return err
	}
	if err = a.kube.createSecret(ctx, "care-db-auth", map[string]string{"username": "care", "password": randomToken()}); err != nil {
		return err
	}
	if err = a.kube.createSecret(ctx, "silo-credentials", map[string]string{"MINIO_ROOT_USER": "care" + randomToken()[:12], "MINIO_ROOT_PASSWORD": randomToken()}); err != nil {
		return err
	}
	if err = a.kube.applyFile(ctx, "infrastructure", nil); err != nil {
		return err
	}
	if err = a.kube.applyFile(ctx, "postgres", nil); err != nil {
		return err
	}
	for _, name := range []string{"registry", "redis", "silo"} {
		if err = a.stage(id, "Waiting for "+name); err != nil {
			return err
		}
		wait, done := context.WithTimeout(ctx, 10*time.Minute)
		err = a.kube.wait(wait, deployments, namespace, name, deploymentReady)
		done()
		if err != nil {
			return err
		}
	}
	if err = a.stage(id, "Waiting for PostgreSQL"); err != nil {
		return err
	}
	wait, done := context.WithTimeout(ctx, 15*time.Minute)
	defer done()
	return a.kube.wait(wait, clusters, namespace, "care-db", conditionReady)
}

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func resolve(ctx context.Context, repo string) (string, error) {
	if repo != "care" && repo != "care_fe" {
		return "", errors.New("repository not supported")
	}
	b, err := download(ctx, "https://api.github.com/repos/ohcnetwork/"+repo+"/commits/develop", 2<<20)
	if err != nil {
		return "", err
	}
	var result struct {
		SHA string `json:"sha"`
	}
	if err = json.Unmarshal(b, &result); err != nil {
		return "", err
	}
	if !commitPattern.MatchString(result.SHA) {
		return "", errors.New("invalid resolved commit")
	}
	return result.SHA, nil
}

func envList(values map[string]string) []any {
	out := []any{}
	for key, value := range values {
		out = append(out, map[string]any{"name": key, "value": value})
	}
	return out
}

func buildManifest(id, component, commit string) (*unstructured.Unstructured, error) {
	if !commitPattern.MatchString(commit) {
		return nil, errors.New("invalid commit")
	}
	if component != "backend" && component != "frontend" {
		return nil, errors.New("invalid component")
	}
	objs, err := manifest("build")
	if err != nil {
		return nil, err
	}
	o := objs[0]
	o.SetName("build-" + id + "-" + component)
	repo, dockerfile := "care", "docker/prod.Dockerfile"
	if component == "frontend" {
		repo, dockerfile = "care_fe", "Dockerfile"
	}
	values := map[string]string{"SOURCE_REPOSITORY": "https://github.com/ohcnetwork/" + repo + ".git", "SOURCE_COMMIT": commit, "COMPONENT": component}
	init, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "initContainers")
	init[0].(map[string]any)["env"] = envList(values)
	if err = unstructured.SetNestedSlice(o.Object, init, "spec", "template", "spec", "initContainers"); err != nil {
		return nil, err
	}
	err = setContainer(o, 0, func(c map[string]any) {
		c["env"] = envList(map[string]string{"DOCKERFILE": dockerfile, "SOURCE_COMMIT": commit, "PUSH_IMAGE": "registry.care.svc.cluster.local:5000/care-" + component + ":" + id})
	})
	return o, err
}

func (a *App) build(ctx context.Context, id string, s Settings) error {
	if err := a.preflight(ctx, id); err != nil {
		return err
	}
	if err := a.stage(id, "Resolving approved develop branches to immutable commits"); err != nil {
		return err
	}
	be, err := resolve(ctx, "care")
	if err != nil {
		return err
	}
	fe, err := resolve(ctx, "care_fe")
	if err != nil {
		return err
	}
	if _, err = a.db.Exec("INSERT INTO releases(id,backend_commit,frontend_commit,status,created) VALUES(?,?,?,'building',?)", id, be, fe, now()); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_, _ = a.db.Exec("UPDATE releases SET status='build-incomplete' WHERE id=?", id)
		}
	}()
	for _, part := range []struct{ name, commit string }{{"backend", be}, {"frontend", fe}} {
		if err = a.stage(id, "Building "+part.name+" at "+part.commit[:12]); err != nil {
			return err
		}
		job, err := buildManifest(id, part.name, part.commit)
		if err != nil {
			return err
		}
		if err = a.kube.apply(ctx, job); err != nil {
			return err
		}
		if err = a.kube.wait(ctx, jobs, namespace, job.GetName(), jobComplete); err != nil {
			return err
		}
		digest, err := a.buildDigest(ctx, job.GetName())
		if err != nil {
			return err
		}
		column := "backend_image"
		if part.name == "frontend" {
			column = "frontend_image"
		}
		if _, err = a.db.Exec("UPDATE releases SET "+column+"=? WHERE id=?", s.Registry+"/care-"+part.name+"@"+digest, id); err != nil {
			return err
		}
	}
	_, err = a.db.Exec("UPDATE releases SET status='built' WHERE id=?", id)
	success = err == nil
	return err
}

func (a *App) buildDigest(ctx context.Context, name string) (string, error) {
	list, err := a.kube.client.Resource(pods).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return "", err
	}
	for _, p := range list.Items {
		statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
		for _, raw := range statuses {
			c := raw.(map[string]any)
			if c["name"] != "build" {
				continue
			}
			v, _, _ := unstructured.NestedString(c, "state", "terminated", "message")
			v = strings.TrimSpace(v)
			if digestPattern.MatchString(v) {
				return v, nil
			}
		}
	}
	return "", errors.New("build completed without a valid image digest; inspect build pod")
}

func decodeBase64(v string) ([]byte, error) { return base64.StdEncoding.DecodeString(v) }
func (a *App) secretValues(ctx context.Context, name string) (map[string]string, error) {
	o, err := a.kube.client.Resource(secrets).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("required Secret/%s unavailable; prepare infrastructure first", name)
	}
	data, _, err := unstructured.NestedStringMap(o.Object, "data")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range data {
		b, err := decodeBase64(v)
		if err != nil {
			return nil, errors.New("invalid Secret encoding")
		}
		out[k] = string(b)
	}
	return out, nil
}

func jwks() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	key.Precompute()
	enc := func(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }
	jwk := map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": randomToken()[:12], "n": enc(key.N), "e": enc(big.NewInt(int64(key.E))), "d": enc(key.D), "p": enc(key.Primes[0]), "q": enc(key.Primes[1]), "dp": enc(key.Precomputed.Dp), "dq": enc(key.Precomputed.Dq), "qi": enc(key.Precomputed.Qinv)}
	b, err := json.Marshal(map[string]any{"keys": []any{jwk}})
	return base64.StdEncoding.EncodeToString(b), err
}

func (a *App) install(ctx context.Context, id string, req operationRequest, s Settings) (err error) {
	if err = a.preflight(ctx, id); err != nil {
		return err
	}
	var be, fe string
	if err = a.db.QueryRow("SELECT backend_image,frontend_image FROM releases WHERE id=? AND status='built'", req.ReleaseID).Scan(&be, &fe); err != nil {
		return errors.New("built release not found")
	}
	if err = a.stage(id, "Checking fresh-install boundary"); err != nil {
		return err
	}
	// An existing initialization Job means migrations may have run. Never replay it.
	for _, resource := range []struct {
		kind string
		name string
	}{{"job", "care-initialize"}, {"deployment", "care-api"}} {
		g := jobs
		if resource.kind == "deployment" {
			g = deployments
		}
		_, e := a.kube.client.Resource(g).Namespace(namespace).Get(ctx, resource.name, metav1.GetOptions{})
		if e == nil {
			return errors.New("existing CARE initialization/workloads found; automatic migration retry is blocked")
		}
		if !apierrors.IsNotFound(e) {
			return e
		}
	}
	if _, err = a.db.Exec("UPDATE releases SET status='installing' WHERE id=?", req.ReleaseID); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = a.db.Exec("UPDATE releases SET status='install-uncertain' WHERE id=?", req.ReleaseID)
		}
	}()
	if err = a.stage(id, "Preparing CARE configuration and secrets"); err != nil {
		return err
	}
	db, err := a.secretValues(ctx, "care-db-auth")
	if err != nil {
		return err
	}
	silo, err := a.secretValues(ctx, "silo-credentials")
	if err != nil {
		return err
	}
	if db["password"] == "" || silo["MINIO_ROOT_USER"] == "" || silo["MINIO_ROOT_PASSWORD"] == "" {
		return errors.New("required infrastructure credentials are missing")
	}
	keys, err := jwks()
	if err != nil {
		return err
	}
	if err = a.kube.createSecret(ctx, "care-secrets", map[string]string{"DATABASE_URL": "postgres://care:" + url.QueryEscape(db["password"]) + "@care-db-rw:5432/care", "POSTGRES_HOST": "care-db-rw", "POSTGRES_PORT": "5432", "POSTGRES_DB": "care", "POSTGRES_USER": "care", "POSTGRES_PASSWORD": db["password"], "DJANGO_SECRET_KEY": randomToken(), "JWKS_BASE64": keys, "BUCKET_KEY": silo["MINIO_ROOT_USER"], "BUCKET_SECRET": silo["MINIO_ROOT_PASSWORD"]}); err != nil {
		return err
	}
	if err = a.kube.createSecret(ctx, "care-initial-admin", map[string]string{"DJANGO_SUPERUSER_USERNAME": req.AdminUsername, "DJANGO_SUPERUSER_PASSWORD": req.AdminPassword, "DJANGO_SUPERUSER_EMAIL": req.AdminEmail}); err != nil {
		return err
	}
	origins, _ := json.Marshal([]string{s.CareURL})
	u, _ := url.Parse(s.CareURL)
	config := map[string]string{"DJANGO_SETTINGS_MODULE": "config.settings.production", "DJANGO_DEBUG": "False", "DJANGO_ALLOWED_HOSTS": "[\"*\"]", "DJANGO_SECURE_SSL_REDIRECT": "False", "DJANGO_SECURE_HSTS_PRELOAD": "False", "DJANGO_SECURE_HSTS_INCLUDE_SUBDOMAINS": "False", "CSRF_TRUSTED_ORIGINS": string(origins), "CORS_ALLOWED_ORIGINS": string(origins), "CURRENT_DOMAIN": u.Host, "BACKEND_DOMAIN": u.Host, "REDIS_URL": "redis://redis:6379/0", "CELERY_BROKER_URL": "redis://redis:6379/0", "BUCKET_ENDPOINT": "http://silo:9000", "BUCKET_EXTERNAL_ENDPOINT": s.CareURL, "BUCKET_REGION": "ap-south-1", "FILE_UPLOAD_BUCKET": "patient-bucket", "FACILITY_S3_BUCKET": "facility-bucket", "USE_SMS": "False", "SENTRY_DSN": "", "GUNICORN_WORKERS": "1", "CELERY_WORKER_CONCURRENCY": "1", "DJANGO_EMAIL_BACKEND": "django.core.mail.backends.dummy.EmailBackend"}
	if err = a.kube.apply(ctx, object("ConfigMap", "care-config", map[string]any{"data": stringMap(config)})); err != nil {
		return err
	}
	if err = a.stage(id, "Running one-time migration and administrator initialization"); err != nil {
		return err
	}
	if err = a.kube.applyFile(ctx, "migration", func(o *unstructured.Unstructured) error {
		return setContainer(o, 0, func(c map[string]any) { c["image"] = be })
	}); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 35*time.Minute)
	defer cancel()
	if err = a.kube.wait(wait, jobs, namespace, "care-initialize", jobComplete); err != nil {
		return err
	}
	if err = a.kube.client.Resource(secrets).Namespace(namespace).Delete(ctx, "care-initial-admin", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return errors.New("initialization succeeded but failed to remove temporary administrator secret")
	}
	if err = a.stage(id, "Starting CARE workloads"); err != nil {
		return err
	}
	if err = a.kube.applyFile(ctx, "care", func(o *unstructured.Unstructured) error {
		if o.GetKind() != "Deployment" || o.GetName() == "care-proxy" {
			return nil
		}
		image := be
		if o.GetName() == "care-frontend" {
			image = fe
		}
		return setContainer(o, 0, func(c map[string]any) { c["image"] = image })
	}); err != nil {
		return err
	}
	for _, name := range careDeployments {
		if err = a.stage(id, "Waiting for "+name); err != nil {
			return err
		}
		wait, done := context.WithTimeout(ctx, 10*time.Minute)
		err = a.kube.wait(wait, deployments, namespace, name, deploymentReady)
		done()
		if err != nil {
			return err
		}
	}
	_, err = a.db.Exec("UPDATE releases SET status='installed' WHERE id=?", req.ReleaseID)
	return err
}

var careDeployments = []string{"care-api", "care-worker", "care-beat", "care-frontend", "care-proxy"}

func (a *App) control(ctx context.Context, id, kind string) error {
	if err := a.stage(id, "Applying CARE "+kind+" (persistent services remain running)"); err != nil {
		return err
	}
	// Validate the complete target set before changing any replica count.
	for _, name := range careDeployments {
		o, err := a.kube.client.Resource(deployments).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if o.GetLabels()["app.kubernetes.io/managed-by"] != managedBy {
			return errors.New("refusing to modify unmanaged deployment")
		}
		if kind == "restart" {
			replicas, _, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
			if replicas == 0 {
				return errors.New("CARE is stopped; use Start instead")
			}
		}
	}
	for _, name := range careDeployments {
		patch := map[string]any{"spec": map[string]any{"replicas": 1}}
		if kind == "stop" {
			patch["spec"] = map[string]any{"replicas": 0}
		}
		if kind == "restart" {
			patch["spec"] = map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"cncm.ohc.network/restarted-at": now()}}}}
		}
		b, _ := json.Marshal(patch)
		if _, err := a.kube.client.Resource(deployments).Namespace(namespace).Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{FieldManager: managedBy}); err != nil {
			return err
		}
	}
	for _, name := range careDeployments {
		ready := deploymentReady
		if kind == "stop" {
			ready = func(o *unstructured.Unstructured) (bool, error) {
				observed, _, _ := unstructured.NestedInt64(o.Object, "status", "observedGeneration")
				replicas, _, _ := unstructured.NestedInt64(o.Object, "status", "replicas")
				return observed >= o.GetGeneration() && replicas == 0, nil
			}
		}
		wait, done := context.WithTimeout(ctx, 10*time.Minute)
		err := a.kube.wait(wait, deployments, namespace, name, ready)
		done()
		if err != nil {
			return err
		}
	}
	return nil
}
