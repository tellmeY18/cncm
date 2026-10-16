package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid request body")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, 400, "expected one JSON object")
		return false
	}
	return true
}
func validateOrigin(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("expected an http(s) origin without a path, credentials, or trailing slash")
	}
	return nil
}

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,64}$`)
var registryPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*:[0-9]{1,5}$`)

func validateSettings(s Settings) error {
	if len(strings.TrimSpace(s.ClinicName)) < 1 || len(s.ClinicName) > 100 {
		return errors.New("clinic name must be 1–100 characters")
	}
	if err := validateOrigin(s.CareURL); err != nil {
		return err
	}
	if !registryPattern.MatchString(s.Registry) {
		return errors.New("registry must be hostname:port (no scheme or path)")
	}
	port, err := strconv.Atoi(s.Registry[strings.LastIndex(s.Registry, ":")+1:])
	if err != nil || port < 1 || port > 65535 {
		return errors.New("registry port must be between 1 and 65535")
	}
	return nil
}

func (a *App) user(r *http.Request) string {
	c, err := r.Cookie("cncm_session")
	if err != nil {
		return ""
	}
	var user string
	if err = a.db.QueryRow("SELECT username FROM sessions WHERE hash=? AND expires>?", tokenHash(c.Value), time.Now().Unix()).Scan(&user); err != nil {
		return ""
	}
	return user
}
func (a *App) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.user(r) == "" {
			fail(w, 401, "sign in to continue")
			return
		}
		next(w, r)
	}
}
func (a *App) sessionInfo(w http.ResponseWriter, r *http.Request) {
	var count int
	if err := a.db.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil {
		fail(w, 500, "cannot read accounts")
		return
	}
	reply(w, 200, map[string]any{"username": a.user(r), "needsSetup": count == 0})
}

type credentials struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	BootstrapToken string `json:"bootstrapToken,omitempty"`
}

func (a *App) session(w http.ResponseWriter, user string) error {
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	_, err := a.db.Exec("INSERT INTO sessions VALUES(?,?,?)", tokenHash(token), user, expiry.Unix())
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "cncm_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.origin, "https://"), SameSite: http.SameSiteStrictMode, Expires: expiry})
	return nil
}
func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var c credentials
	if !decode(w, r, &c) {
		return
	}
	var n int
	if err := a.db.QueryRow("SELECT count(*) FROM users").Scan(&n); err != nil {
		fail(w, 500, "cannot read accounts")
		return
	}
	if n != 0 {
		fail(w, 409, "administrator already exists")
		return
	}
	if len(a.bootstrap) < 24 || subtle.ConstantTimeCompare([]byte(c.BootstrapToken), []byte(a.bootstrap)) != 1 {
		fail(w, 403, "invalid bootstrap token")
		return
	}
	if !usernamePattern.MatchString(c.Username) || len(c.Password) < 12 || len(c.Password) > 72 {
		fail(w, 400, "use a 3–64 character username and a 12–72 byte password")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "could not hash password")
		return
	}
	if _, err = a.db.Exec("INSERT INTO users VALUES(?,?)", c.Username, hash); err != nil {
		fail(w, 500, "could not create administrator")
		return
	}
	a.audit(c.Username, "created CNCM administrator")
	if err = a.session(w, c.Username); err != nil {
		fail(w, 500, "account created; sign in again")
		return
	}
	reply(w, 201, map[string]bool{"ok": true})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if time.Since(a.loginWindow) > time.Minute {
		a.failedLogins = 0
		a.loginWindow = time.Now()
	}
	if a.failedLogins >= 10 {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "too many attempts; wait one minute")
		return
	}
	var c credentials
	if !decode(w, r, &c) {
		return
	}
	a.failedLogins++
	var hash []byte
	err := a.db.QueryRow("SELECT password FROM users WHERE username=?", c.Username).Scan(&hash)
	if err != nil || bcrypt.CompareHashAndPassword(hash, []byte(c.Password)) != nil {
		fail(w, 401, "invalid username or password")
		return
	}
	if err = a.session(w, c.Username); err != nil {
		fail(w, 500, "cannot create session")
		return
	}
	a.failedLogins = 0
	a.audit(c.Username, "signed in")
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("cncm_session")
	if _, err := a.db.Exec("DELETE FROM sessions WHERE hash=?", tokenHash(c.Value)); err != nil {
		fail(w, 500, "cannot end session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "cncm_session", Value: "", Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.origin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *App) audit(user, action string) {
	_, _ = a.db.Exec("INSERT INTO audit(actor,action,created) VALUES(?,?,?)", user, action, now())
}
func (a *App) auditHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query("SELECT actor,action,created FROM audit ORDER BY id DESC LIMIT 100")
	if err != nil {
		fail(w, 500, "cannot read audit history")
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var actor, action, created string
		if err = rows.Scan(&actor, &action, &created); err != nil {
			fail(w, 500, "cannot read audit history")
			return
		}
		out = append(out, map[string]string{"actor": actor, "action": action, "created": created})
	}
	reply(w, 200, out)
}
func (a *App) state(w http.ResponseWriter, r *http.Request) {
	s, err := a.settings()
	if err != nil {
		fail(w, 500, "cannot read settings")
		return
	}
	ops, err := a.operations()
	if err != nil {
		fail(w, 500, "cannot read operations")
		return
	}
	rels, err := a.releases()
	if err != nil {
		fail(w, 500, "cannot read releases")
		return
	}
	target := "Disconnected"
	if a.kube != nil {
		target = a.kube.target
	}
	reply(w, 200, map[string]any{"settings": s, "operations": ops, "releases": rels, "cluster": target, "connected": a.kube != nil, "version": "0.1.0-poc"})
}
func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var s Settings
	if !decode(w, r, &s) {
		return
	}
	if err := validateSettings(s); err != nil {
		fail(w, 400, err.Error())
		return
	}
	var n int
	if err := a.db.QueryRow("SELECT count(*) FROM operations WHERE status IN ('running','uncertain')").Scan(&n); err != nil {
		fail(w, 500, "cannot check operations")
		return
	}
	if n > 0 {
		fail(w, 409, "finish or acknowledge the active operation first")
		return
	}
	if err := a.db.QueryRow("SELECT count(*) FROM releases WHERE status IN ('installed','installing','install-uncertain')").Scan(&n); err != nil {
		fail(w, 500, "cannot check installed release")
		return
	}
	if n > 0 {
		old, err := a.settings()
		if err != nil {
			fail(w, 500, "cannot read settings")
			return
		}
		if old.Registry != s.Registry || old.CareURL != s.CareURL {
			fail(w, 409, "changing deployment addresses after installation is not supported in this POC")
			return
		}
	}
	data, _ := json.Marshal(s)
	if _, err := a.db.Exec("INSERT INTO settings VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value", string(data)); err != nil {
		fail(w, 500, "cannot save settings")
		return
	}
	a.audit(a.user(r), "saved installation settings")
	reply(w, 200, s)
}

func (a *App) acknowledge(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var body struct {
		Confirmation string `json:"confirmation"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Confirmation != "I inspected the cluster" {
		fail(w, 400, "confirm that you inspected outstanding Jobs and resources")
		return
	}
	res, err := a.db.Exec("UPDATE operations SET status='acknowledged',updated=? WHERE id=? AND status='uncertain'", now(), r.PathValue("id"))
	if err != nil {
		fail(w, 500, "cannot acknowledge")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, 409, "operation is not uncertain")
		return
	}
	a.audit(a.user(r), "acknowledged interrupted operation "+r.PathValue("id"))
	reply(w, 200, map[string]bool{"ok": true})
}
