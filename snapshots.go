package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Local recovery points built on standard CSI VolumeSnapshots (Containers Anywhere
// 0.4.2+ provides them on its btrfs data disk, but any CSI driver with a
// VolumeSnapshotClass works). A point is: a CloudNativePG volume-snapshot backup of
// the database plus a VolumeSnapshot of the Silo file volume, taken together.
//
// What this is not: an off-device backup (it lives on the same disk), point-in-time
// recovery, or a copy of Kubernetes Secrets (those live in the cluster datastore).
// Restore is deliberately not offered until the recovery path is tested.
var volumeSnapshots = gvr("snapshot.storage.k8s.io", "v1", "volumesnapshots")
var volumeSnapshotClasses = gvr("snapshot.storage.k8s.io", "v1", "volumesnapshotclasses")
var cnpgBackups = gvr("postgresql.cnpg.io", "v1", "backups")

const (
	filesPVC          = "silo-data"
	dbCluster         = "care-db"
	consistencyStop   = "stopped"
	consistencyLive   = "live"
	pointLabel        = "cncm.ohc.network/recovery-point"
	snapshotWaitLimit = 20 * time.Minute
)

var pointID = regexp.MustCompile(`^[a-f0-9]{12}$`)

type SnapshotSupport struct {
	Available bool   `json:"available"`
	Class     string `json:"class,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type RecoveryPoint struct {
	ID          string   `json:"id"`
	Created     string   `json:"created"`
	Consistency string   `json:"consistency"`
	ReleaseID   string   `json:"releaseId"`
	Status      string   `json:"status"` // creating | ready | failed | deleted
	Health      string   `json:"health"` // ready | notReady | missing | unknown
	Note        string   `json:"note,omitempty"`
	Snapshots   []string `json:"snapshots"`
	DBBackup    string   `json:"dbBackup,omitempty"`
	Class       string   `json:"class,omitempty"`
}

// snapshotSupport finds the VolumeSnapshotClass to use: the default class, or the
// only one. It never assumes a driver or class name.
func (k *Cluster) snapshotSupport(ctx context.Context) SnapshotSupport {
	if !k.hasKind("VolumeSnapshot", "snapshot.storage.k8s.io", "v1") || !k.hasKind("Backup", "postgresql.cnpg.io", "v1") {
		return SnapshotSupport{Reason: "This cluster has no CSI VolumeSnapshot API (or CloudNativePG is not installed yet)."}
	}
	list, err := k.client.Resource(volumeSnapshotClasses).List(ctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		return SnapshotSupport{Reason: "No VolumeSnapshotClass exists, so the storage driver cannot take snapshots."}
	}
	if len(list.Items) == 1 {
		return SnapshotSupport{Available: true, Class: list.Items[0].GetName()}
	}
	for _, c := range list.Items {
		if c.GetAnnotations()["snapshot.storage.kubernetes.io/is-default-class"] == "true" {
			return SnapshotSupport{Available: true, Class: c.GetName()}
		}
	}
	return SnapshotSupport{Reason: "Several VolumeSnapshotClasses exist and none is the default."}
}

func filesSnapshot(id, class string) *unstructured.Unstructured {
	o := object("VolumeSnapshot", "rp-"+id+"-files", map[string]any{"spec": map[string]any{
		"volumeSnapshotClassName": class,
		"source":                  map[string]any{"persistentVolumeClaimName": filesPVC},
	}})
	o.Object["apiVersion"] = "snapshot.storage.k8s.io/v1"
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": managedBy, pointLabel: id})
	return o
}

func dbBackup(id string) *unstructured.Unstructured {
	o := object("Backup", "rp-"+id+"-db", map[string]any{"spec": map[string]any{
		"cluster": map[string]any{"name": dbCluster}, "method": "volumeSnapshot", "online": true,
	}})
	o.Object["apiVersion"] = "postgresql.cnpg.io/v1"
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": managedBy, pointLabel: id})
	return o
}

func snapshotReady(o *unstructured.Unstructured) (bool, error) {
	if msg, _, _ := unstructured.NestedString(o.Object, "status", "error", "message"); msg != "" && o.GetDeletionTimestamp() == nil {
		// The snapshotter retries transient errors; surface them only through the timeout.
		return false, nil
	}
	ready, _, _ := unstructured.NestedBool(o.Object, "status", "readyToUse")
	return ready, nil
}

func backupCompleted(o *unstructured.Unstructured) (bool, error) {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	switch phase {
	case "completed":
		return true, nil
	case "failed":
		msg, _, _ := unstructured.NestedString(o.Object, "status", "error")
		return false, fmt.Errorf("database snapshot failed: %s", redact(msg))
	}
	return false, nil
}

// backupSnapshotNames returns the VolumeSnapshots CloudNativePG created for a Backup.
func backupSnapshotNames(o *unstructured.Unstructured) []string {
	elements, _, _ := unstructured.NestedSlice(o.Object, "status", "backupSnapshotStatus", "elements")
	var names []string
	for _, e := range elements {
		if m, ok := e.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

func (a *App) setPoint(id, status, note string, snapshots []string, dbBackupName string) {
	data, _ := json.Marshal(snapshots)
	_, _ = a.db.Exec("UPDATE recovery_points SET status=?,note=?,snapshots=?,db_backup=? WHERE id=?", status, note, string(data), dbBackupName, id)
}

// snapshotPoint creates a recovery point. With consistency "stopped" CARE's
// application pods are scaled to zero first, so the database and files are
// captured with no writers; "live" leaves CARE running (each volume is
// crash-consistent, the two are captured moments apart).
func (a *App) snapshotPoint(ctx context.Context, id, consistency string) (err error) {
	support := a.kube.snapshotSupport(ctx)
	if !support.Available {
		return errors.New(support.Reason)
	}
	var releaseID string
	if err = a.db.QueryRow("SELECT id FROM releases WHERE status='installed' LIMIT 1").Scan(&releaseID); err != nil {
		return errors.New("CARE must be installed before taking a recovery point")
	}
	if hv := a.kube.hostView(ctx, hostNamespace()); hv.State == "available" {
		if disk, ok := hv.Guest["disk"].(map[string]any); ok && disk["pressure"] == true {
			return errors.New("the appliance data disk is nearly full; free space or grow the disk before taking a snapshot")
		}
	}
	if _, err = a.db.Exec("INSERT INTO recovery_points(id,created,consistency,release_id,status,class) VALUES(?,?,?,?, 'creating', ?)", id, now(), consistency, releaseID, support.Class); err != nil {
		return err
	}
	var names []string
	var backupName string
	defer func() {
		if err != nil {
			a.setPoint(id, "failed", redact(err.Error()), names, backupName)
		}
	}()
	if consistency == consistencyStop {
		if err = a.control(ctx, id, "stop"); err != nil {
			return err
		}
		defer func() {
			restart, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			if e := a.control(restart, id, "start"); e != nil {
				err = errors.Join(err, errors.New("CARE could not be restarted automatically; use Start in Services"))
			}
		}()
	}
	if err = a.stage(id, "Telling the database to snapshot its volume"); err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"backup": map[string]any{"volumeSnapshot": map[string]any{"className": support.Class}}}})
	if _, err = a.kube.client.Resource(clusters).Namespace(namespace).Patch(ctx, dbCluster, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: managedBy}); err != nil {
		return errors.New("cannot enable volume snapshots on the database cluster")
	}
	b := dbBackup(id)
	backupName = b.GetName()
	if err = a.kube.apply(ctx, b); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, snapshotWaitLimit)
	defer cancel()
	if err = a.kube.wait(wait, cnpgBackups, namespace, backupName, backupCompleted); err != nil {
		return err
	}
	done, e := a.kube.client.Resource(cnpgBackups).Namespace(namespace).Get(ctx, backupName, metav1.GetOptions{})
	if e == nil {
		names = append(names, backupSnapshotNames(done)...)
	}
	if err = a.stage(id, "Snapshotting uploaded files"); err != nil {
		return err
	}
	f := filesSnapshot(id, support.Class)
	if err = a.kube.apply(ctx, f); err != nil {
		return err
	}
	names = append(names, f.GetName())
	if err = a.kube.wait(wait, volumeSnapshots, namespace, f.GetName(), snapshotReady); err != nil {
		return err
	}
	note := "Same disk as the clinic: protects against mistakes and bad updates, not against losing the disk. Kubernetes Secrets are not included."
	if consistency == consistencyLive {
		note = "Taken while CARE was running: each volume is crash-consistent, but the database and files were captured moments apart. " + note
	}
	a.setPoint(id, "ready", note, names, backupName)
	return nil
}

// deletePoint removes the snapshots CNCM recorded for a point. Only recorded
// names are touched, and only in the CARE namespace.
func (a *App) deletePoint(ctx context.Context, id, pointRef string) error {
	var status, raw, backup string
	if err := a.db.QueryRow("SELECT status,snapshots,db_backup FROM recovery_points WHERE id=?", pointRef).Scan(&status, &raw, &backup); err != nil {
		return errors.New("unknown recovery point")
	}
	if status == "creating" || status == "deleted" {
		return errors.New("this recovery point cannot be deleted in its current state")
	}
	var names []string
	_ = json.Unmarshal([]byte(raw), &names)
	if err := a.stage(id, "Deleting snapshots of recovery point "+pointRef); err != nil {
		return err
	}
	for _, n := range names {
		if err := a.kube.client.Resource(volumeSnapshots).Namespace(namespace).Delete(ctx, n, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("cannot delete snapshot %s", n)
		}
	}
	if backup != "" {
		if err := a.kube.client.Resource(cnpgBackups).Namespace(namespace).Delete(ctx, backup, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return errors.New("cannot delete the database backup record")
		}
	}
	_, err := a.db.Exec("UPDATE recovery_points SET status='deleted' WHERE id=?", pointRef)
	return err
}

func (a *App) recoveryPoints(ctx context.Context) ([]RecoveryPoint, error) {
	rows, err := a.db.Query("SELECT id,created,consistency,release_id,status,class,db_backup,snapshots,note FROM recovery_points WHERE status!='deleted' ORDER BY created DESC, rowid DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecoveryPoint{}
	for rows.Next() {
		var p RecoveryPoint
		var raw string
		if err = rows.Scan(&p.ID, &p.Created, &p.Consistency, &p.ReleaseID, &p.Status, &p.Class, &p.DBBackup, &raw, &p.Note); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &p.Snapshots)
		p.Health = a.pointHealth(ctx, p)
		out = append(out, p)
	}
	return out, rows.Err()
}

// pointHealth checks that every recorded snapshot still exists and is ready.
// It proves the snapshot objects are there; it is not a restore test.
func (a *App) pointHealth(ctx context.Context, p RecoveryPoint) string {
	if p.Status != "ready" || a.kube == nil {
		return "unknown"
	}
	health := "ready"
	for _, n := range p.Snapshots {
		o, err := a.kube.client.Resource(volumeSnapshots).Namespace(namespace).Get(ctx, n, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return "missing"
		case err != nil:
			return "unknown"
		}
		if ok, _ := snapshotReady(o); !ok {
			health = "notReady"
		}
	}
	return health
}

func (a *App) listRecoveryPoints(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	support := SnapshotSupport{Reason: "No cluster selected."}
	if a.kube != nil {
		support = a.kube.snapshotSupport(ctx)
	}
	points, err := a.recoveryPoints(ctx)
	if err != nil {
		fail(w, 500, "cannot read recovery points")
		return
	}
	reply(w, 200, map[string]any{"support": support, "points": points})
}

func validConsistency(m string) bool { return m == consistencyStop || m == consistencyLive }

func hasPointID(s string) bool { return pointID.MatchString(s) }
