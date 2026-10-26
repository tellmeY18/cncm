import { expect, test, type Page } from "@playwright/test";

const settings = { clinicName: "Test clinic", careURL: "http://localhost:8090", registry: "localhost:30500" };
const operation = { id: "test-operation", kind: "install", status: "uncertain", stage: "Initialization needs inspection", message: "No automatic retry.", created: "2026-10-26T00:00:00Z", actor: "admin" };
const installed = { id: "release-1", backendCommit: "a".repeat(40), frontendCommit: "b".repeat(40), backendImage: "backend@sha256:" + "a".repeat(64), frontendImage: "frontend@sha256:" + "b".repeat(64), status: "installed", created: "2026-10-26T00:00:00Z" };

async function mockPortal(page: Page, options: { connected?: boolean; uncertain?: boolean; installed?: boolean; health?: string; stateFailure?: boolean } = {}) {
  const requests: { path: string; body: Record<string, unknown> }[] = [];
  let saved = { ...settings };
  let uncertain = !!options.uncertain;
  await page.route("**/api/**", async route => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    if (req.method() !== "GET") {
      const body = req.postDataJSON() as Record<string, unknown>;
      requests.push({ path, body });
      if (path === "/api/settings") saved = body as typeof settings;
      if (path.endsWith("/acknowledge")) uncertain = false;
      return route.fulfill({ json: path === "/api/operations" ? { id: "queued" } : { ok: true } });
    }
    if (path === "/api/session") return route.fulfill({ json: { username: "admin", needsSetup: false } });
    if (path === "/api/state") {
      if (options.stateFailure) return route.fulfill({ status: 503, json: { error: "Status read failed" } });
      return route.fulfill({ json: { settings: saved, operations: uncertain ? [operation] : [], releases: options.installed ? [installed] : [], connected: options.connected ?? true, cluster: "fake-disposable", version: "0.1.0" } });
    }
    if (path === "/api/cluster") return route.fulfill({ json: { pods: [], events: [], health: options.health || "Not installed", ...(!options.connected && options.connected !== undefined ? { error: "No cluster selected." } : {}) } });
    if (path === "/api/audit") return route.fulfill({ json: [] });
    return route.fulfill({ status: 404, json: { error: "Unexpected test request" } });
  });
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Overview", exact: true })).toBeVisible();
  return requests;
}

test("disconnected mode explains controls and keeps backup claims honest", async ({ page }) => {
  await mockPortal(page, { connected: false });
  await expect(page.getByText("Status unavailable", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Setup", exact: true }).click();
  await expect(page.getByRole("button", { name: "Run checks", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Prepare services", exact: true })).toBeDisabled();
  await expect(page.getByText(/No cluster is configured/)).toBeVisible();
  await page.getByRole("button", { name: "Backups", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Recovery is not available yet" })).toBeVisible();
  await expect(page.getByRole("button", { name: /Restore|Back up now/ })).toHaveCount(0);
});

test("CareUI tokens, local fonts, and responsive layout", async ({ page }) => {
  await mockPortal(page);
  const style = await page.locator("body").evaluate(el => ({
    primary: getComputedStyle(el).getPropertyValue("--primary").trim(),
    radius: getComputedStyle(el).getPropertyValue("--radius").trim(),
    font: getComputedStyle(el).fontFamily,
    overflow: document.documentElement.scrollWidth > window.innerWidth,
  }));
  expect(style.primary).toBe("#065f46");
  expect(style.radius).toBe("0.625rem");
  expect(style.font).toContain("Figtree Variable");
  expect(style.overflow).toBe(false);
  expect(await page.locator("link[href*='fonts.googleapis']").count()).toBe(0);
  await expect(page.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("overview.png"), fullPage: true });
});

test("confirmation uses a modal, safe default focus, and exact target", async ({ page }) => {
  const requests = await mockPortal(page);
  await page.getByRole("button", { name: "Setup", exact: true }).click();
  const trigger = page.getByRole("button", { name: "Prepare services", exact: true });
  await trigger.click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: "Prepare supporting services?" })).toBeVisible();
  await expect(dialog.getByText("fake-disposable")).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(dialog).not.toBeVisible();
  await expect(trigger).toBeFocused();
  expect(requests).toHaveLength(0);
  await trigger.click();
  await dialog.getByRole("button", { name: "Prepare services", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: "Request accepted" })).toBeVisible();
  expect(requests).toEqual([{ path: "/api/operations", body: { kind: "prepare", target: "fake-disposable" } }]);
});

test("unsaved settings survive polling and require explicit discard", async ({ page }) => {
  const requests = await mockPortal(page);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("textbox", { name: "Clinic name", exact: true }).fill("Draft clinic");
  await expect(page.getByText("Unsaved changes", { exact: true })).toBeVisible();
  await page.waitForResponse(res => res.url().endsWith("/api/state"));
  await expect(page.getByRole("textbox", { name: "Clinic name", exact: true })).toHaveValue("Draft clinic");
  await page.getByRole("button", { name: "Overview", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("textbox", { name: "Clinic name", exact: true })).toHaveValue("Draft clinic");
  await page.getByRole("button", { name: "Save settings", exact: true }).click();
  await expect(page.getByText(/Settings saved for future operations/)).toBeVisible();
  expect(requests[0]).toEqual({ path: "/api/settings", body: { ...settings, clinicName: "Draft clinic" } });
  await page.getByRole("textbox", { name: "Clinic name", exact: true }).fill("Discard me");
  await page.getByRole("button", { name: "Overview", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Discard and leave", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Overview", exact: true })).toBeFocused();
});

test("uncertain operations stay locked and acknowledgement does not claim recovery", async ({ page }) => {
  const requests = await mockPortal(page, { uncertain: true });
  await page.getByRole("button", { name: "Setup", exact: true }).click();
  await expect(page.getByRole("button", { name: "Prepare services", exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Activity", exact: true }).click();
  await page.getByRole("button", { name: "Acknowledge after inspection" }).click();
  const dialog = page.getByRole("dialog");
  const accept = dialog.getByRole("button", { name: "Acknowledge inspection", exact: true });
  await expect(accept).toBeDisabled();
  await expect(dialog.getByText(/does not recover the clinic/)).toBeVisible();
  await dialog.getByRole("checkbox").check();
  await accept.click();
  await expect(page.getByText("Inspection acknowledged. No recovery or retry was performed.")).toBeVisible();
  expect(requests[0]).toEqual({ path: "/api/operations/test-operation/acknowledge", body: { confirmation: "I inspected the cluster" } });
});

test("installation is not presented as readiness and stopped CARE cannot restart", async ({ page }) => {
  await mockPortal(page, { installed: true, health: "Stopped" });
  await expect(page.getByText("Installed", { exact: true })).toBeVisible();
  await expect(page.getByText("Stopped", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Restart CARE", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Start CARE", exact: true })).toBeEnabled();
});

test("failed operation reports an error, not success", async ({ page }) => {
  await mockPortal(page);
  await page.route("**/api/operations", route => route.fulfill({ status: 409, json: { error: "Another operation needs inspection" } }));
  await page.getByRole("button", { name: "Setup", exact: true }).click();
  await page.getByRole("button", { name: "Run checks", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Run checks", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Another operation needs inspection");
  await expect(page.getByText(/Request accepted/)).toHaveCount(0);
});

test("invalid origin is rejected before saving", async ({ page }) => {
  const requests = await mockPortal(page);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const origin = page.getByRole("textbox", { name: /CARE browser address/ });
  await origin.fill("http://localhost:8090/not-an-origin");
  await page.getByRole("button", { name: "Save settings", exact: true }).click();
  expect(await origin.evaluate((el: HTMLInputElement) => el.validity.patternMismatch)).toBe(true);
  expect(requests).toHaveLength(0);
});

test("failed refresh locks stale controls until reconnection", async ({ page }) => {
  await mockPortal(page);
  await page.getByRole("button", { name: "Setup", exact: true }).click();
  await expect(page.getByRole("button", { name: "Run checks", exact: true })).toBeEnabled();
  await page.route("**/api/state", route => route.fulfill({ status: 503, json: { error: "Connection lost" } }));
  await expect(page.getByRole("alert")).toContainText("Controls are locked until reconnection", { timeout: 10000 });
  await expect(page.getByRole("button", { name: "Run checks", exact: true })).toBeDisabled();
  await page.unroute("**/api/state");
  await page.route("**/api/state", route => route.fulfill({ json: { settings, operations: [], releases: [], connected: true, cluster: "fake-disposable", version: "0.1.0" } }));
  await expect(page.getByRole("button", { name: "Run checks", exact: true })).toBeEnabled({ timeout: 10000 });
});

test("failed initial status loads visibly and can be retried", async ({ page }) => {
  await page.route("**/api/session", route => route.fulfill({ json: { username: "admin", needsSetup: false } }));
  await page.route("**/api/state", route => route.fulfill({ status: 503, json: { error: "Status unavailable from server" } }));
  await page.goto("/");
  await expect(page.getByRole("alert")).toContainText("Status unavailable from server");
  await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeEnabled();
});
