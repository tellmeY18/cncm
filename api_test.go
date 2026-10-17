package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	db, err := openStore(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &App{db: db, origin: "http://localhost:8080", bootstrap: strings.Repeat("b", 32), ctx: context.Background()}
}

func request(a *App, method, path string, body any, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func setupAdmin(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	w := request(a, "POST", "/api/setup", credentials{Username: "admin", Password: "long-test-password", BootstrapToken: a.bootstrap}, nil, a.origin)
	if w.Code != 201 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "cncm_session" {
			return c
		}
	}
	t.Fatal("missing session cookie")
	return nil
}

func TestAuthenticationAndOriginBoundary(t *testing.T) {
	a := testApp(t)
	if got := request(a, "GET", "/api/state", nil, nil, "").Code; got != 401 {
		t.Fatalf("unauthenticated state = %d", got)
	}
	c := credentials{Username: "admin", Password: "long-test-password", BootstrapToken: a.bootstrap}
	if got := request(a, "POST", "/api/setup", c, nil, "http://attacker.invalid").Code; got != 403 {
		t.Fatalf("cross-origin setup = %d", got)
	}
	c.BootstrapToken = "wrong"
	if got := request(a, "POST", "/api/setup", c, nil, a.origin).Code; got != 403 {
		t.Fatalf("wrong bootstrap = %d", got)
	}
	cookie := setupAdmin(t, a)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie must be HttpOnly and Strict")
	}
	if got := request(a, "GET", "/api/state", nil, cookie, "").Code; got != 200 {
		t.Fatalf("authenticated state = %d", got)
	}
	if got := request(a, "POST", "/api/setup", c, nil, a.origin).Code; got != 409 {
		t.Fatalf("second setup = %d", got)
	}
	for _, origin := range []string{"", "http://attacker.invalid"} {
		if got := request(a, "PUT", "/api/settings", defaultSettings(), cookie, origin).Code; got != 403 {
			t.Fatalf("settings with origin %q = %d", origin, got)
		}
	}
	var stored string
	if err := a.db.QueryRow("SELECT hash FROM sessions").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == cookie.Value || stored != tokenHash(cookie.Value) {
		t.Fatal("raw session token persisted")
	}
	if got := request(a, "POST", "/api/logout", map[string]any{}, cookie, a.origin).Code; got != 200 {
		t.Fatalf("logout = %d", got)
	}
	if got := request(a, "GET", "/api/state", nil, cookie, "").Code; got != 401 {
		t.Fatalf("session usable after logout = %d", got)
	}
}

func TestExpiredSessionsAndLoginThrottle(t *testing.T) {
	a := testApp(t)
	cookie := setupAdmin(t, a)
	if _, err := a.db.Exec("UPDATE sessions SET expires=?", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if got := request(a, "GET", "/api/state", nil, cookie, "").Code; got != 401 {
		t.Fatalf("expired session accepted = %d", got)
	}
	for i := 0; i < 10; i++ {
		w := request(a, "POST", "/api/login", credentials{Username: "admin", Password: "wrong"}, nil, a.origin)
		if w.Code != 401 {
			t.Fatalf("attempt %d = %d", i, w.Code)
		}
	}
	if got := request(a, "POST", "/api/login", credentials{Username: "admin", Password: "long-test-password"}, nil, a.origin).Code; got != 429 {
		t.Fatalf("login not throttled = %d", got)
	}
}

func TestOperationsSerializeAndBlockSettings(t *testing.T) {
	a := testApp(t)
	cookie := setupAdmin(t, a)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.begin("build", "admin"); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("concurrent winners = %d", winners.Load())
	}
	if got := request(a, "PUT", "/api/settings", defaultSettings(), cookie, a.origin).Code; got != 409 {
		t.Fatalf("settings changed during operation = %d", got)
	}
}

func TestRestartDoesNotReplayUncertainOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{db: db}
	id, err := a.begin("install", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.stage(id, "migrations"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a.db = db
	ops, err := a.operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Status != "uncertain" || ops[0].Stage != "migrations" {
		t.Fatalf("lost interrupted operation: %+v", ops)
	}
	if _, err = a.begin("build", "admin"); err == nil {
		t.Fatal("uncertainty must block new mutations")
	}
}

func TestSettingsValidation(t *testing.T) {
	for _, origin := range []string{"file:///tmp/data", "https://user:password@example.com", "https://example.com/", "https://example.com?q=1", "//example.com", "javascript:alert(1)"} {
		s := defaultSettings()
		s.CareURL = origin
		if validateSettings(s) == nil {
			t.Errorf("accepted origin %q", origin)
		}
	}
	for _, registry := range []string{"http://registry:5000", "registry:5000/path", "registry:5000;id", "registry:5000\n"} {
		s := defaultSettings()
		s.Registry = registry
		if validateSettings(s) == nil {
			t.Errorf("accepted registry %q", registry)
		}
	}
	if err := validateSettings(defaultSettings()); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectedModeAndTargetConfirmation(t *testing.T) {
	a := testApp(t)
	cookie := setupAdmin(t, a)
	if got := request(a, "POST", "/api/operations", operationRequest{Kind: "prepare"}, cookie, a.origin).Code; got != 409 {
		t.Fatalf("disconnected mutation = %d", got)
	}
	a.kube = &Cluster{target: "disposable"}
	if got := request(a, "POST", "/api/operations", operationRequest{Kind: "prepare", Target: "other"}, cookie, a.origin).Code; got != 400 {
		t.Fatalf("wrong target = %d", got)
	}
	if got := request(a, "POST", "/api/operations", operationRequest{Kind: "restore", Target: "disposable"}, cookie, a.origin).Code; got != 400 {
		t.Fatalf("unimplemented restore = %d", got)
	}
}

func TestEmbeddedUI(t *testing.T) {
	a := testApp(t)
	w := request(a, "GET", "/", nil, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<div id=\"root\"") {
		t.Fatal("build frontend before tests; embedded entry point missing")
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing CSP")
	}
}

func TestAutoOriginAndDerivedCareURL(t *testing.T) {
	a := testApp(t)
	a.origin = ""
	r := httptest.NewRequest("POST", "http://cncm.cncm-system.local:8080/api/login", nil)
	r.Header.Set("Origin", "http://cncm.cncm-system.local:8080")
	if !a.allowedOrigin(r) {
		t.Fatal("same-origin request rejected in auto mode")
	}
	r.Header.Set("Origin", "http://evil.example")
	if a.allowedOrigin(r) {
		t.Fatal("foreign origin accepted in auto mode")
	}
	s, err := a.settingsFor(httptest.NewRequest("GET", "http://cncm.cncm-system.local:8080/api/state", nil))
	if err != nil || s.CareURL != "http://care.care.local:8080" {
		t.Fatalf("derived CARE URL %q, %v", s.CareURL, err)
	}
	s, _ = a.settingsFor(httptest.NewRequest("GET", "http://localhost:8080/api/state", nil))
	if s.CareURL != defaultSettings().CareURL {
		t.Fatalf("unexpected derivation for %q", s.CareURL)
	}
}
