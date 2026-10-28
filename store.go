package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Settings struct {
	ClinicName string `json:"clinicName"`
	CareURL    string `json:"careURL"`
	Registry   string `json:"registry"`
}

func defaultSettings() Settings {
	return Settings{ClinicName: "My clinic", CareURL: "http://localhost:8090", Registry: "localhost:30500"}
}

type Operation struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Created string `json:"created"`
	Updated string `json:"updated"`
	Actor   string `json:"actor"`
}

type Release struct {
	ID             string `json:"id"`
	BackendCommit  string `json:"backendCommit"`
	FrontendCommit string `json:"frontendCommit"`
	BackendImage   string `json:"backendImage"`
	FrontendImage  string `json:"frontendImage"`
	Status         string `json:"status"`
	Created        string `json:"created"`
}

func openStore(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS users (username TEXT PRIMARY KEY, password BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (hash TEXT PRIMARY KEY, username TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, kind TEXT NOT NULL, status TEXT NOT NULL, stage TEXT NOT NULL, message TEXT NOT NULL DEFAULT '', created TEXT NOT NULL, updated TEXT NOT NULL, actor TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS one_mutation ON operations((1)) WHERE status IN ('running','uncertain');
CREATE TABLE IF NOT EXISTS releases (id TEXT PRIMARY KEY, backend_commit TEXT NOT NULL, frontend_commit TEXT NOT NULL, backend_image TEXT NOT NULL DEFAULT '', frontend_image TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS recovery_points (id TEXT PRIMARY KEY, created TEXT NOT NULL, consistency TEXT NOT NULL, release_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, class TEXT NOT NULL DEFAULT '', db_backup TEXT NOT NULL DEFAULT '', snapshots TEXT NOT NULL DEFAULT '[]', note TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, action TEXT NOT NULL, created TEXT NOT NULL);
UPDATE operations SET status='uncertain', message='CNCM restarted during this operation. Inspect Jobs and resources before acknowledging; no automatic retry.', updated=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE status='running';`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func randomToken() string { return hex.EncodeToString(randomBytes(32)) }
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func now() string               { return time.Now().UTC().Format(time.RFC3339) }

// settingsFor returns saved settings. Until settings are saved, the CARE address
// follows how the browser reached CNCM: cncm.cncm-system.local[:port] suggests
// https://care.care.local[:port], the layout Containers Anywhere publishes with
// its local CA.
func (a *App) settingsFor(r *http.Request) (Settings, error) {
	s, err := a.settings()
	if err != nil {
		return s, err
	}
	var saved int
	if a.db.QueryRow("SELECT count(*) FROM settings WHERE id=1").Scan(&saved) == nil && saved == 0 {
		host, port := r.Host, ""
		if h, p, e := net.SplitHostPort(r.Host); e == nil {
			host, port = h, ":"+p
		}
		for _, tld := range []string{"local", "localhost"} {
			if host == "cncm.cncm-system."+tld {
				s.CareURL = "https://care.care." + tld + port
			}
		}
	}
	return s, nil
}

func (a *App) settings() (Settings, error) {
	s := defaultSettings()
	var raw string
	err := a.db.QueryRow("SELECT value FROM settings WHERE id=1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal([]byte(raw), &s)
}

func (a *App) operations() ([]Operation, error) {
	rows, err := a.db.Query("SELECT id,kind,status,stage,message,created,updated,actor FROM operations ORDER BY created DESC, rowid DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Operation{}
	for rows.Next() {
		var o Operation
		if err := rows.Scan(&o.ID, &o.Kind, &o.Status, &o.Stage, &o.Message, &o.Created, &o.Updated, &o.Actor); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (a *App) releases() ([]Release, error) {
	rows, err := a.db.Query("SELECT id,backend_commit,frontend_commit,backend_image,frontend_image,status,created FROM releases ORDER BY created DESC, rowid DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.BackendCommit, &r.FrontendCommit, &r.BackendImage, &r.FrontendImage, &r.Status, &r.Created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (a *App) begin(kind, actor string) (string, error) {
	id := randomToken()[:12]
	_, err := a.db.Exec("INSERT INTO operations(id,kind,status,stage,created,updated,actor) VALUES(?,?,'running','queued',?,?,?)", id, kind, now(), now(), actor)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", errors.New("another operation is running or needs recovery acknowledgement")
		}
		return "", err
	}
	return id, nil
}

func (a *App) stage(id, stage string) error {
	r, err := a.db.Exec("UPDATE operations SET stage=?,updated=? WHERE id=? AND status='running'", stage, now(), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("operation %s is no longer running", id)
	}
	return nil
}
