package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

//go:embed web/dist
var uiFiles embed.FS

type App struct {
	db           *sql.DB
	kube         *Cluster
	origin       string
	bootstrap    string
	mu           sync.Mutex
	authMu       sync.Mutex
	failedLogins int
	loginWindow  time.Time
	ctx          context.Context
}

func main() {
	data := env("CNCM_DATA_DIR", "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		log.Fatal(err)
	}
	// A second manager must never race operation recovery. Deployment uses Recreate
	// and replicas=1; the process-level lock also protects local development.
	lock, err := os.OpenFile(filepath.Join(data, "manager.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatal("another CNCM process owns this data directory")
	}
	db, err := openStore(filepath.Join(data, "cncm.sqlite"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	origin := env("CNCM_ORIGIN", "http://localhost:8080")
	if origin == "auto" {
		origin = "" // accept the origin the browser used, as long as it matches the Host header
	} else if err = validateOrigin(origin); err != nil {
		log.Fatal("CNCM_ORIGIN: ", err)
	}
	a := &App{db: db, origin: origin, bootstrap: os.Getenv("CNCM_BOOTSTRAP_TOKEN"), ctx: ctx}
	a.kube, err = newCluster()
	if err != nil {
		log.Fatal(err)
	}
	if a.kube == nil {
		log.Print("cluster access disabled: set CNCM_KUBE_CONTEXT explicitly for a disposable local cluster")
	}
	var users int
	if err = db.QueryRow("SELECT count(*) FROM users").Scan(&users); err != nil {
		log.Fatal(err)
	}
	if ns := os.Getenv("CNCM_NAMESPACE"); users == 0 && len(a.bootstrap) < 24 && a.kube != nil && ns != "" {
		if a.bootstrap, err = a.kube.bootstrapToken(ctx, ns); err != nil {
			log.Fatal(err)
		}
		log.Printf("setup token is stored in Secret %s/cncm-bootstrap", ns)
	}
	if users == 0 && len(a.bootstrap) < 24 {
		log.Fatal("set CNCM_BOOTSTRAP_TOKEN to a random value of at least 24 characters; it is never printed")
	}
	server := &http.Server{Addr: env("CNCM_ADDR", "127.0.0.1:8080"), Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("CNCM listening on %s (browser origin %q)", server.Addr, origin)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (a *App) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	m.HandleFunc("GET /api/session", a.sessionInfo)
	m.HandleFunc("POST /api/setup", a.setup)
	m.HandleFunc("POST /api/login", a.login)
	m.HandleFunc("POST /api/logout", a.authorized(a.logout))
	m.HandleFunc("GET /api/state", a.authorized(a.state))
	m.HandleFunc("PUT /api/settings", a.authorized(a.saveSettings))
	m.HandleFunc("GET /api/cluster", a.authorized(a.clusterStatus))
	m.HandleFunc("GET /api/logs", a.authorized(a.logs))
	m.HandleFunc("GET /api/host", a.authorized(a.hostState))
	m.HandleFunc("POST /api/host/requests", a.authorized(a.createHostRequest))
	m.HandleFunc("DELETE /api/host/requests/{name}", a.authorized(a.deleteHostRequest))
	m.HandleFunc("POST /api/operations", a.authorized(a.startOperation))
	m.HandleFunc("POST /api/operations/{id}/acknowledge", a.authorized(a.acknowledge))
	m.HandleFunc("GET /api/audit", a.authorized(a.auditHistory))
	m.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	assets, _ := fs.Sub(uiFiles, "web/dist")
	m.Handle("GET /", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" {
			if !a.allowedOrigin(r) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "request must originate from the configured CNCM origin")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}

// allowedOrigin checks the Origin header against the configured origin, or, in
// auto mode, against the Host the browser addressed.
func (a *App) allowedOrigin(r *http.Request) bool {
	got := r.Header.Get("Origin")
	if a.origin != "" {
		return got == a.origin
	}
	return got == "http://"+r.Host || got == "https://"+r.Host
}
