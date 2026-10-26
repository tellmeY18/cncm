import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import "@fontsource-variable/figtree";
import "@fontsource-variable/geist-mono";
import "./careui.css";
import "./style.css";

type Session = { username: string; needsSetup: boolean };
// The launcher passes its bootstrap token in the fragment, never in server logs.
const prefilledToken = new URLSearchParams(location.hash.slice(1)).get("token") ?? "";
if (prefilledToken) history.replaceState(null, "", location.pathname + location.search);
type Settings = { clinicName: string; careURL: string; registry: string };
type Operation = { id: string; kind: string; status: string; stage: string; message: string; created: string; actor: string };
type Release = { id: string; backendCommit: string; frontendCommit: string; backendImage: string; frontendImage: string; status: string; created: string };
type State = { settings: Settings; operations: Operation[]; releases: Release[]; cluster: string; connected: boolean; version: string };
type Pod = { name: string; phase: string; ready: number; total: number; restarts: number; reason: string; containers: string[] };
type Cluster = { pods: Pod[]; events: { reason: string; message: string }[]; health?: string; error?: string; eventsError?: string; healthError?: string };
type Audit = { actor: string; action: string; created: string };
type Page = "Overview" | "Setup" | "Builds & releases" | "Services" | "Backups" | "Activity" | "Settings";
type Confirmation = { title: string; description: string; label: string; target?: string; inspection?: boolean; accept: () => void };

async function api<T>(path: string, method = "GET", body?: unknown): Promise<T> {
  const res = await fetch(`/api/${path}`, { method, signal: AbortSignal.timeout(20000), credentials: "same-origin", headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data as T;
}

function Icon({ name, size = 20 }: { name: string; size?: number }) {
  const paths: Record<string, ReactNode> = {
    Overview: <><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></>,
    Setup: <><path d="m12 3 9 5-9 5-9-5 9-5Z"/><path d="m3 12 9 5 9-5M3 16l9 5 9-5"/></>,
    "Builds & releases": <><path d="m8 4-6 8 6 8m8-16 6 8-6 8m-3-17-2 18"/></>,
    Services: <><rect x="3" y="3" width="18" height="7" rx="2"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 6.5h.01M7 17.5h.01M11 6.5h6M11 17.5h6"/></>,
    Backups: <><path d="M5 8a7 7 0 0 1 13-2M3 3v6h6m10 7A7 7 0 0 1 6 18m15 3v-6h-6M12 7v5l3 2"/></>,
    Activity: <path d="M3 12h4l3-8 4 16 3-8h4"/>,
    Settings: <><circle cx="12" cy="12" r="3"/><path d="m9 3-1 3-3 1-2 4 2 3 1 4 4 2 3-1 4-1 2-4-1-3-1-4-4-2-4 1Z"/></>,
    arrow: <path d="M7 17 17 7M7 7h10v10"/>,
    check: <path d="m5 12 4 4L19 6"/>,
    logout: <path d="M9 4H4v16h5m6-12 4 4-4 4m-6-4h10"/>,
  };
  return <svg aria-hidden="true" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round">{paths[name] || paths.Activity}</svg>;
}

function Brand() { return <div className="brand"><span className="brand-mark">c<span>+</span></span><div>CNCM<small>Cloud Native CARE Manager</small></div></div>; }
function Badge({ value }: { value: string }) {
  return <span className={`badge ${["succeeded", "installed", "built", "Workloads ready"].includes(value) ? "green" : ["uncertain", "failed", "install-uncertain", "build-incomplete", "Needs attention", "Not ready"].includes(value) ? "amber" : "neutral"}`}><span className="dot"/>{value.replaceAll("-", " ")}</span>;
}
function Empty({ title, children }: { title: string; children: ReactNode }) { return <div className="empty"><h3>{title}</h3><p>{children}</p></div>; }
function Field({ label, help, children }: { label: string; help?: string; children: ReactNode }) { return <label className="field"><span>{label}</span>{children}{help && <small>{help}</small>}</label>; }
function When({ value }: { value: string }) { return <time dateTime={value}>{new Date(value).toLocaleString()}</time>; }

function ConfirmDialog({ confirmation, close }: { confirmation: Confirmation; close: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [inspected, setInspected] = useState(false);
  useEffect(() => {
    const previous = document.activeElement;
    ref.current?.showModal();
    return () => { if (previous instanceof HTMLElement && previous.isConnected) previous.focus(); };
  }, []);
  return <dialog ref={ref} className="confirm-dialog" aria-labelledby="confirmation-title" aria-describedby="confirmation-description" onCancel={close}>
    <h2 id="confirmation-title">{confirmation.title}</h2>
    <p id="confirmation-description">{confirmation.description}</p>
    {confirmation.target && <div className="target-summary"><span>Selected cluster</span><code>{confirmation.target}</code></div>}
    {confirmation.inspection && <label className="checkbox-field"><input type="checkbox" checked={inspected} onChange={e => setInspected(e.target.checked)}/>I inspected the outstanding Jobs and resources in Kite.</label>}
    <div className="dialog-actions"><button autoFocus onClick={close}>Cancel</button><button className="primary" disabled={confirmation.inspection && !inspected} onClick={() => { close(); confirmation.accept(); }}>{confirmation.label}</button></div>
  </dialog>;
}

function Login({ session, onLogin }: { session: Session; onLogin: () => void }) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    setBusy(true); setError("");
    try {
      await api(session.needsSetup ? "setup" : "login", "POST", { username: f.get("username"), password: f.get("password"), ...(session.needsSetup ? { bootstrapToken: f.get("token") } : {}) });
      onLogin();
    } catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  return <div className="auth-page">
    <div className="auth-intro"><Brand/><div><span className="eyebrow">YOUR CLINIC. YOUR INFRASTRUCTURE.</span><h1>CARE, with<br/>less complexity.</h1><p>One place to build, deploy, and look after your CARE installation.</p></div><small>First-install proof of concept · Disposable test data only</small></div>
    <div className="auth-main"><form onSubmit={submit} className="auth-form" aria-busy={busy}>
      <span className="eyebrow">WELCOME TO CNCM</span><h2>{session.needsSetup ? "Create your administrator" : "Welcome back"}</h2>
      <p>{session.needsSetup ? "This account manages CNCM. You will create a separate CARE account during installation." : "Sign in to manage your clinic."}</p>
      {error && <div role="alert" className="alert error">{error}</div>}
      <Field label="Username"><input name="username" required autoComplete="username" minLength={3}/></Field>
      <Field label="Password" help={session.needsSetup ? "12–72 bytes. Keep this password separate from your CARE account." : undefined}><input name="password" type="password" required autoComplete={session.needsSetup ? "new-password" : "current-password"} minLength={session.needsSetup ? 12 : 1} maxLength={72}/></Field>
      {session.needsSetup && <Field label="Bootstrap token" help="Provided by Containers Anywhere or the person who started CNCM."><input name="token" type="password" required autoComplete="off" defaultValue={prefilledToken}/></Field>}
      <button className="primary full" disabled={busy}>{busy ? "Please wait…" : session.needsSetup ? "Create administrator" : "Sign in"}</button>
    </form></div>
  </div>;
}

function SettingsForm({ settings, disabled, onDirty, save }: { settings: Settings; disabled: boolean; onDirty: (value: boolean) => void; save: (settings: Settings) => Promise<boolean> }) {
  const [draft, setDraft] = useState(settings);
  const dirty = JSON.stringify(draft) !== JSON.stringify(settings);
  useEffect(() => { onDirty(dirty); }, [dirty, onDirty]);
  useEffect(() => {
    if (!dirty) return;
    const guard = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", guard);
    return () => window.removeEventListener("beforeunload", guard);
  }, [dirty]);
  function change(key: keyof Settings, value: string) { setDraft(current => ({ ...current, [key]: value })); }
  return <form className="settings-form" onSubmit={async e => { e.preventDefault(); if (await save(draft)) onDirty(false); }}>
    <fieldset disabled={disabled}>
      <Field label="Clinic name"><input value={draft.clinicName} onChange={e => change("clinicName", e.target.value)} required maxLength={100}/></Field>
      <Field label="CARE browser address" help="An exact http(s) address, without a path or trailing slash. Saving does not change an existing CARE installation."><input type="url" value={draft.careURL} onChange={e => change("careURL", e.target.value)} required pattern="https?://[^\/?#]+"/></Field>
      <details><summary>Advanced connection settings</summary>
        <Field label="Node-reachable image registry" help="Use hostname:port. The node runtime must already trust and reach this registry; CNCM does not configure it."><input value={draft.registry} onChange={e => change("registry", e.target.value)} required pattern="[a-zA-Z0-9][a-zA-Z0-9.\-]*:[0-9]{1,5}"/></Field>
      </details>
      <div className="form-footer"><small>{dirty ? "Unsaved changes" : "Saved configuration. Changes affect future operations, not existing workloads."}</small><div className="actions"><button type="button" disabled={!dirty} onClick={() => setDraft(settings)}>Discard</button><button className="primary" disabled={!dirty}>Save settings</button></div></div>
    </fieldset>
  </form>;
}

const actionDetails: Record<string, { title: string; description: string; label: string }> = {
  preflight: { title: "Check this cluster?", description: "Checks node readiness, architecture, and default storage. No workloads will be changed. Capacity and registry usability are not yet verified by this check.", label: "Run checks" },
  prepare: { title: "Prepare supporting services?", description: "Installs CloudNativePG, PostgreSQL, file storage, Redis, and an image registry. This creates persistent storage and requires internet access.", label: "Prepare services" },
  build: { title: "Build CARE images?", description: "Builds the approved develop branches at pinned commits. Requires internet, privileged build Jobs, and sufficient CPU, memory, and disk. Running CARE is not updated.", label: "Build images" },
  install: { title: "Install this CARE release?", description: "Initializes a fresh database, runs migrations, creates the CARE administrator, and starts the clinic. This is not an upgrade or a safe retry of an interrupted installation.", label: "Install CARE" },
  start: { title: "Start CARE?", description: "Starts the installed application. Supporting services and stored clinic data are preserved.", label: "Start CARE" },
  stop: { title: "Stop CARE?", description: "Staff will be unable to use CARE until you start it again. PostgreSQL, files, and supporting services stay running. This is not a backup.", label: "Stop CARE" },
  restart: { title: "Restart CARE?", description: "Temporarily interrupts staff access while the application restarts. Stored clinic data is preserved.", label: "Restart CARE" },
};

function App() {
  const [session, setSession] = useState<Session>();
  const [state, setState] = useState<State>();
  const [cluster, setCluster] = useState<Cluster>({ pods: [], events: [] });
  const [page, setPage] = useState<Page>("Overview");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState(false);
  const requestLock = useRef(false);
  const [dirty, setDirty] = useState(false);
  const [confirmation, setConfirmation] = useState<Confirmation>();
  const [selected, setSelected] = useState("");
  const [container, setContainer] = useState("");
  const [logs, setLogs] = useState("");
  const [logBusy, setLogBusy] = useState(false);
  const [audit, setAudit] = useState<Audit[]>([]);
  const [auditError, setAuditError] = useState("");
  const [stateError, setStateError] = useState("");
  const heading = useRef<HTMLHeadingElement>(null);
  const refreshSession = () => api<Session>("session").then(setSession).catch(e => setError(e.message));
  const refresh = async () => { setState(await api<State>("state")); setStateError(""); };
  useEffect(() => { void refreshSession(); }, []);
  useEffect(() => {
    if (!session?.username) return;
    let alive = true;
    const load = async () => {
      try { const s = await api<State>("state"); if (alive) { setState(s); setStateError(""); } }
      catch (e) { if (alive) setStateError((e as Error).message); }
    };
    void load();
    const timer = setInterval(() => void load(), 4000);
    return () => { alive = false; clearInterval(timer); };
  }, [session?.username]);
  useEffect(() => {
    if (!session?.username) return;
    let alive = true;
    const load = async () => {
      try { const c = await api<Cluster>("cluster"); if (alive) setCluster(c); }
      catch (e) { if (alive) setCluster({ pods: [], events: [], error: (e as Error).message }); }
    };
    void load();
    const timer = setInterval(() => void load(), 10000);
    return () => { alive = false; clearInterval(timer); };
  }, [session?.username]);
  useEffect(() => {
    if (page !== "Activity" || !session?.username) return;
    let alive = true;
    api<Audit[]>("audit").then(data => { if (alive) { setAudit(data); setAuditError(""); } }).catch(e => { if (alive) setAuditError(e.message); });
    return () => { alive = false; };
  }, [page, session?.username, state?.operations[0]?.status]);
  useEffect(() => { heading.current?.focus(); }, [page]);

  if (!session) return <div className="loading"><Brand/>{error ? <><p role="alert">{error}</p><button onClick={refreshSession}>Retry</button></> : <p role="status">Connecting to CNCM…</p>}</div>;
  if (!session.username) return <Login session={session} onLogin={refreshSession}/>;
  if (!state) return <div className="loading"><Brand/><p role={stateError ? "alert" : "status"}>{stateError || "Loading your clinic…"}</p><button onClick={() => refresh().catch(e => setStateError(e.message))}>Retry</button></div>;

  const active = state.operations.find(o => ["running", "uncertain"].includes(o.status));
  const installed = state.releases.find(r => r.status === "installed");
  const blockedReason = stateError ? "Current management status is unavailable. Wait for reconnection." : pending ? "A request is being sent." : active?.status === "uncertain" ? "Inspect the unresolved operation in Activity before continuing." : active ? "Wait for the current operation to finish." : !state.connected ? "No cluster is configured. Open CNCM through Containers Anywhere or select an explicit test cluster." : "";
  const blocked = !!blockedReason;
  const latestControl = state.operations.find(o => ["start", "stop", "restart"].includes(o.kind));
  const health = active?.status === "uncertain" ? "Needs attention" : active && ["install", "start", "restart", "stop"].includes(active.kind) ? `${active.kind === "stop" ? "Stopping" : "Starting"} CARE` : cluster.error ? "Status unavailable" : installed && cluster.health === "Not installed" ? "Needs attention" : cluster.health || "Checking status";
  const stopped = health === "Stopped";

  function navigate(next: Page) {
    if (next === page) return;
    const go = () => { setDirty(false); setPage(next); setError(""); setNotice(""); setSelected(""); };
    if (dirty) setConfirmation({ title: "Discard unsaved settings?", description: "Your edits have not been saved. Leaving this page will discard them.", label: "Discard and leave", accept: go });
    else go();
  }
  async function mutate(path: string, body: unknown, success: string, method = "POST") {
    if (requestLock.current) return false;
    requestLock.current = true; setPending(true); setError(""); setNotice("");
    try {
      await api(path, method, body);
      setNotice(success);
      try { await refresh(); } catch (e) { setStateError((e as Error).message); }
      return true;
    } catch (e) { setError((e as Error).message); return false; }
    finally { requestLock.current = false; setPending(false); }
  }
  function action(kind: string, extra: Record<string, unknown> = {}) {
    if (!state || blocked) return;
    setConfirmation({ ...actionDetails[kind], target: state.cluster, accept: () => { void mutate("operations", { kind, target: state.cluster, ...extra }, "Request accepted. Follow the actual progress in Activity."); } });
  }
  function acknowledge(op: Operation) {
    setConfirmation({
      title: "Clear the operation lock after inspection?",
      description: "This only clears the mutation lock. It does not recover the clinic, undo changes, or make migrations safe to rerun. If the outcome is unclear, cancel and ask for support.",
      label: "Acknowledge inspection", target: state?.cluster, inspection: true,
      accept: () => { void mutate(`operations/${op.id}/acknowledge`, { confirmation: "I inspected the cluster" }, "Inspection acknowledged. No recovery or retry was performed."); },
    });
  }
  const settingsForm = <SettingsForm key={page} settings={state.settings} disabled={pending || !!active || !!stateError} onDirty={setDirty} save={settings => mutate("settings", settings, "Settings saved for future operations. Existing CARE workloads were not changed.", "PUT")}/>;
  const controls = <div className="actions"><button disabled={blocked || !installed || (health === "Workloads ready")} onClick={() => action("start")}>Start CARE</button><button disabled={blocked || !installed || stopped || latestControl?.status === "running"} onClick={() => action("restart")}>Restart CARE</button><button disabled={blocked || !installed || stopped} onClick={() => action("stop")}>Stop CARE</button></div>;
  const operationList = <div className="operation-list">{state.operations.length ? state.operations.map(o => <article className="operation" key={o.id}><div className="op-body"><div className="row"><strong className="capitalize">{o.kind}</strong><Badge value={o.status}/></div><p>{o.stage}</p>{o.message && <small>{o.message}</small>}<div className="meta"><code>{o.id}</code><span>{o.actor}</span><When value={o.created}/></div>{o.status === "uncertain" && <button disabled={pending || !!stateError} onClick={() => acknowledge(o)}>Acknowledge after inspection</button>}</div></article>) : <Empty title="No activity yet">Installation, builds, and service actions will appear here.</Empty>}</div>;
  const pages: Page[] = ["Overview", "Setup", "Builds & releases", "Services", "Backups", "Activity", "Settings"];
  const subtitles: Record<Page, string> = { Overview: "Your clinic status and next steps.", Setup: "Prepare a fresh, disposable installation of CARE.", "Builds & releases": "Build candidates without changing the running clinic.", Services: "Application controls and supporting-service diagnostics.", Backups: "Recovery capabilities and current limitations.", Activity: "Actual operation stages, outcomes, and administrator actions.", Settings: "Configuration for future installation and build operations." };
  return <div className="shell">
    <a className="skip-link" href="#main-content">Skip to content</a>
    <aside className="sidebar"><Brand/><div className="workspace"><span className="workspace-avatar">{state.settings.clinicName[0]?.toUpperCase()}</span><div><strong>{state.settings.clinicName}</strong><small>Single clinic</small></div><span className="badge neutral">POC</span></div>
      <nav aria-label="Main navigation">{pages.map(p => <button key={p} className={page === p ? "nav active" : "nav"} onClick={() => navigate(p)} aria-current={page === p ? "page" : undefined}><Icon name={p}/>{p}{p === "Activity" && active && <span className="nav-dot"/>}</button>)}</nav>
      <div className="sidebar-bottom"><div className="cluster-label">{state.connected ? "Cluster configured" : "No cluster configured"}</div><code title={state.cluster}>{state.cluster}</code><div className="user"><div><strong>{session.username}</strong><small>CNCM administrator</small></div><button className="icon-button" aria-label="Sign out" onClick={() => {
        const logout = async () => { try { await api("logout", "POST", {}); setDirty(false); setState(undefined); setCluster({ pods: [], events: [] }); await refreshSession(); } catch (e) { setError((e as Error).message); } };
        if (dirty) setConfirmation({ title: "Discard edits and sign out?", description: "Your unsaved settings will be discarded.", label: "Sign out", accept: () => { void logout(); } });
        else void logout();
      }}><Icon name="logout"/></button></div></div>
    </aside>
    <main id="main-content" tabIndex={-1}><header className="topbar"><span>Clinic <span className="slash">/</span> <strong>{page}</strong></span><span className="version">v{state.version} · POC</span></header>
      <div className="content">
        <div className="page-heading"><div><h1 ref={heading} tabIndex={-1}>{page}</h1><p>{subtitles[page]}</p></div>{installed && <a className="button primary" href={state.settings.careURL} target="_blank" rel="noreferrer">Open CARE <Icon name="arrow" size={16}/></a>}</div>
        <div className="alert warning">First-install POC. Use disposable test data only. Whole-clinic backup, restore, and upgrades are not available.</div>
        {stateError && <div role="alert" className="alert error">Management status is unavailable: {stateError}. Controls are locked until reconnection.</div>}
        {error && <div role="alert" className="alert error">{error}<button className="dismiss" aria-label="Dismiss error" onClick={() => setError("")}>×</button></div>}
        {notice && <div role="status" className="alert success">{notice}</div>}
        {active && <div role="status" className={`alert ${active.status === "uncertain" ? "warning" : "info"}`}><div><strong>{active.status === "uncertain" ? "An operation needs inspection" : `${active.kind} in progress`}</strong><span>{active.stage}</span></div><button className="text-button" onClick={() => navigate("Activity")}>View activity</button></div>}
        {blockedReason && <p className="control-hint" role="status">{blockedReason}</p>}

        {page === "Overview" && <>
          <section className="card"><div className="card-heading"><h2>Clinic status</h2><Badge value={health}/></div><div className="card-body">
            <p>{installed ? "Workload readiness is reported by Kubernetes. It does not verify CARE login, clinical workflows, or network access from another device." : "Set up supporting services, build CARE, and initialize a fresh clinic."}</p>
            {(cluster.error || cluster.healthError) && <p role="alert">{cluster.error || cluster.healthError}</p>}
            {installed ? controls : <button className="primary" onClick={() => navigate("Setup")}>Start setup</button>}
          </div></section>
          <div className="stats"><div className="stat"><span>Installation</span><strong>{installed ? "Installed" : state.releases.some(r => r.status === "install-uncertain") ? "Needs inspection" : "Not installed"}</strong><small>Separate from live readiness</small></div><div className="stat"><span>Ready pods</span><strong>{cluster.error || !cluster.health ? "—" : cluster.pods.filter(p => p.total > 0 && p.ready === p.total).length}</strong><small>Includes supporting services</small></div><div className="stat"><span>Built releases</span><strong>{state.releases.filter(r => ["built", "installed"].includes(r.status)).length}</strong><small>Pinned commits and image digests</small></div><div className="stat"><span>Recovery protection</span><strong className="muted-value">Not available</strong><small>Persistent storage is not a backup</small></div></div>
          <section className="card"><div className="card-heading"><h2>Latest operation</h2><button className="text-button" onClick={() => navigate("Activity")}>View activity</button></div>{state.operations[0] ? <div className="recent"><div><strong className="capitalize">{state.operations[0].kind}</strong><p>{state.operations[0].stage}</p><small>{state.operations[0].message}</small></div><Badge value={state.operations[0].status}/></div> : <Empty title="No operations yet">Nothing has been deployed automatically by CNCM.</Empty>}</section>
        </>}

        {page === "Setup" && <>
          {installed && <div className="alert info">CARE is already installed. Installation is disabled; use Services for application controls.</div>}
          <ol className="setup-steps" aria-label="Installation sequence"><li>Configure clinic</li><li>Check and prepare</li><li>Build and install</li></ol>
          <section className="card"><div className="card-heading"><h2><span className="step">1</span> Configure your clinic</h2></div>{settingsForm}</section>
          <section className="card"><div className="card-heading"><h2><span className="step">2</span> Check and prepare supporting services</h2></div><div className="card-body"><p>Check the node and default storage, then prepare PostgreSQL, file storage, Redis, and the image registry.</p><div className="actions"><button disabled={blocked || dirty || !!installed} onClick={() => action("preflight")}>Run checks</button><button className="primary" disabled={blocked || dirty || !!installed} onClick={() => action("prepare")}>Prepare services</button></div><small className="hint">Save settings first. Checks do not yet validate capacity or registry trust. Builds require significant memory and disk headroom; node registry configuration remains external to CNCM.</small></div></section>
          <section className="card"><div className="card-heading"><h2><span className="step">3</span> Build and install CARE</h2></div><div className="card-body"><p>After supporting services are ready, build both approved develop branches. Then review a candidate and create the CARE administrator.</p><div className="actions"><button className="primary" disabled={blocked || dirty || !!installed} onClick={() => action("build")}>Build CARE images</button><button onClick={() => navigate("Builds & releases")}>Review releases</button></div></div></section>
        </>}

        {page === "Builds & releases" && <>
          <div className="section-toolbar"><p>Sources: ohcnetwork/care and care_fe · develop only.</p><button className="primary" disabled={blocked} onClick={() => action("build")}>Build candidate</button></div>
          {!state.releases.length && <section className="card"><Empty title="No releases yet">Prepare supporting services in Setup before building a candidate.</Empty></section>}
          {state.releases.map(release => <section className="card" key={release.id}><div className="card-heading"><h2>Release <code>{release.id}</code></h2><Badge value={release.status}/></div><div className="card-body"><div className="release-grid"><div><span>Backend commit</span><a href={`https://github.com/ohcnetwork/care/commit/${release.backendCommit}`} target="_blank" rel="noreferrer"><code>{release.backendCommit.slice(0, 12)}</code></a></div><div><span>Frontend commit</span><a href={`https://github.com/ohcnetwork/care_fe/commit/${release.frontendCommit}`} target="_blank" rel="noreferrer"><code>{release.frontendCommit.slice(0, 12)}</code></a></div><div><span>Created</span><When value={release.created}/></div></div><details><summary>Advanced: immutable image references</summary><pre>{release.backendImage || "Backend image pending"}{"\n"}{release.frontendImage || "Frontend image pending"}</pre></details>
            {release.status === "built" && !installed && <form className="install-form" onSubmit={e => { e.preventDefault(); const f = new FormData(e.currentTarget); action("install", { releaseId: release.id, adminUsername: f.get("adminUsername"), adminPassword: f.get("adminPassword"), adminEmail: f.get("adminEmail") }); }}><h3>Administrator for the CARE application</h3><p>This is separate from your CNCM account.</p><fieldset disabled={blocked}><div className="form-grid"><Field label="CARE administrator username"><input name="adminUsername" required pattern="[a-zA-Z0-9_.\-]{3,64}" autoComplete="off"/></Field><Field label="CARE administrator email"><input name="adminEmail" type="email" required maxLength={254} autoComplete="off"/></Field><Field label="CARE administrator password"><input name="adminPassword" type="password" required minLength={12} maxLength={128} autoComplete="new-password"/></Field></div><button className="primary">Review installation</button></fieldset></form>}
            {release.status === "built" && installed && <p className="hint">Candidate preserved. Applying upgrades is disabled until whole-clinic recovery is implemented and verified.</p>}
          </div></section>)}
        </>}

        {page === "Services" && <>
          <section className="card"><div className="card-heading"><h2>CARE application</h2><Badge value={health}/></div><div className="card-body"><p>Controls affect CARE only. PostgreSQL, file storage, Redis, and the registry remain running.</p>{controls}{!installed && <small className="hint">Install CARE before using these controls.</small>}{stopped && <small className="hint">CARE is stopped. Use Start CARE, not Restart.</small>}</div></section>
          {(cluster.error || cluster.healthError) && <div role="alert" className="alert error">{cluster.error || cluster.healthError}</div>}
          <section className="card"><div className="card-body"><details><summary>Advanced: workload diagnostics</summary><div className="table-wrap"><table><caption>Pods in the CARE namespace</caption><thead><tr><th scope="col">Pod</th><th scope="col">Status</th><th scope="col">Ready</th><th scope="col">Restarts</th><th scope="col">Details</th></tr></thead><tbody>{cluster.pods.map(p => <tr key={p.name}><td><code>{p.name}</code></td><td><Badge value={p.reason || p.phase}/></td><td>{p.ready}/{p.total}</td><td>{p.restarts}</td><td><button className="text-button" onClick={() => { setSelected(p.name); setContainer(p.containers[0] || ""); setLogs(""); }}>Inspect logs<span className="sr-only"> for {p.name}</span></button></td></tr>)}</tbody></table></div>{!cluster.pods.length && <Empty title={cluster.error ? "Workloads unavailable" : "No workloads to show"}>{cluster.error ? "Resolve the cluster connection error before inspecting workloads." : "Prepare supporting services to see pod status here."}</Empty>}</details></div></section>
          {selected && <section className="card"><div className="card-heading"><h2>Pod logs</h2><button onClick={() => setSelected("")}>Close logs</button></div><div className="card-body"><p><code>{selected}</code></p><Field label="Container"><select value={container} onChange={e => { setContainer(e.target.value); setLogs(""); }}>{cluster.pods.find(p => p.name === selected)?.containers.map(c => <option key={c}>{c}</option>)}</select></Field><button disabled={logBusy || !container} onClick={async () => {
            setLogBusy(true);
            try { const data = await api<{ text: string }>(`logs?pod=${encodeURIComponent(selected)}&container=${encodeURIComponent(container)}`); setLogs(data.text || "No log output yet."); } catch (e) { setLogs(`Unable to load logs: ${(e as Error).message}`); } finally { setLogBusy(false); }
          }}>{logBusy ? "Loading logs…" : "Load recent logs"}</button><pre className="log-view" aria-live="polite">{logs || "Choose a container and load its logs."}</pre><small>Last 150 lines, bounded to 32 KiB. Redaction is best-effort; logs may contain sensitive clinical information.</small></div></section>}
          <section className="card"><div className="card-body"><details><summary>Advanced: Kubernetes events</summary>{cluster.eventsError ? <p role="alert">{cluster.eventsError}</p> : cluster.error ? <p>Events are unavailable while the cluster cannot be read.</p> : cluster.events.length ? cluster.events.map((e, i) => <div className="event" key={i}><strong>{e.reason}</strong><span>{e.message}</span></div>) : <p>No events reported.</p>}</details></div></section>
        </>}

        {page === "Backups" && <section className="card"><div className="card-heading"><h2>Recovery is not available yet</h2><Badge value="Not implemented"/></div><div className="card-body"><p>CNCM cannot currently create or restore a backup. Persistent volumes preserve data across ordinary restarts, but do not protect against disk or machine loss.</p><p>CNCM will coordinate CARE-consistent recovery and show backup status. Containers Anywhere will own whole-machine recovery and host backup destinations once its supported interfaces are available.</p><div className="alert warning">A PostgreSQL backup alone does not include uploaded files, credentials, matching images, or CNCM state. Do not use this POC with real patient data.</div></div></section>}

        {page === "Activity" && <><section className="card"><div className="card-heading"><h2>Operation history</h2></div>{operationList}</section><section className="card"><div className="card-heading"><h2>Administrator activity</h2></div><div className="card-body">{auditError ? <p role="alert">Unable to load administrator activity: {auditError}</p> : audit.length ? audit.map((a, i) => <div className="audit" key={i}><strong>{a.actor}</strong><span>{a.action}</span><When value={a.created}/></div>) : <p>No administrator activity reported.</p>}</div></section></>}

        {page === "Settings" && <><section className="card"><div className="card-heading"><h2>Clinic configuration</h2></div>{settingsForm}</section><section className="card"><div className="card-heading"><h2>Current scope</h2></div><div className="card-body"><p>One machine, one cluster, one CARE installation. Only ohcnetwork/care and care_fe develop branches are supported.</p><p>Clinic feature settings, plugins, password recovery, upgrades, and whole-clinic restore are not implemented in this release.</p><a href="https://careui.ohc.network/" target="_blank" rel="noreferrer">UI standard: CareUI</a></div></section></>}
      </div>
      <footer>CNCM v{state.version} · First-install proof of concept · AGPL-3.0-only</footer>
    </main>
    {confirmation && <ConfirmDialog confirmation={confirmation} close={() => setConfirmation(undefined)}/>}
  </div>;
}

createRoot(document.getElementById("root")!).render(<App/>);
