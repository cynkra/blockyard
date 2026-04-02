import { test, expect, type Page } from "@playwright/test";

const DEX_EMAIL = "demo@example.com";
const DEX_PASSWORD = "password";

/** Complete the Dex OIDC login flow on the current page. */
async function dexLogin(page: Page) {
  await page.waitForSelector('input[name="login"]', { timeout: 30_000 });
  await page.fill('input[name="login"]', DEX_EMAIL);
  await page.fill('input[name="password"]', DEX_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForLoadState("networkidle");
}

test.describe("login flow", () => {
  test("landing page shows sign-in button", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator('main a[href="/login"]')).toBeVisible();
    await expect(page.locator(".left-nav")).not.toBeVisible();
  });

  test("login redirects to authenticated apps page", async ({ page }) => {
    await page.goto("/");
    await page.locator('main a[href="/login"]').click();
    await dexLogin(page);

    await expect(page).toHaveURL("/");
    await expect(page.locator(".left-nav")).toBeVisible();
    await expect(page.locator('main a[href="/login"]')).not.toBeVisible();
  });

  test("session-expired overlay is hidden after login", async ({ page }) => {
    await page.goto("/");
    await page.locator('main a[href="/login"]').click();
    await dexLogin(page);

    const overlay = page.locator("#session-expired-overlay");
    await expect(overlay).toBeHidden();
    // DaisyUI modal uses display:grid + visibility:hidden when closed,
    // not display:none. Check visibility to catch CSS specificity bugs.
    const visibility = await overlay.evaluate(
      (el) => getComputedStyle(el).visibility,
    );
    expect(visibility).toBe("hidden");
  });
});

test.describe("authenticated navigation", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/login");
    await dexLogin(page);
  });

  test("left-nav links are clickable", async ({ page }) => {
    await page.click('a[href="/deployments"]');
    await expect(page).toHaveURL(/deployments/);
    await expect(
      page.locator('a[href="/deployments"].active'),
    ).toBeVisible();

    await page.click('a[href="/profile"]');
    await expect(page).toHaveURL(/profile/);
    await expect(page.locator('a[href="/profile"].active')).toBeVisible();

    await page.click('a[href="/"]');
    await expect(page).toHaveURL("/");
  });

  test("session persists across page navigations", async ({ page }) => {
    await page.goto("/deployments");
    await expect(page.locator(".left-nav")).toBeVisible();
    await expect(page.locator('main a[href="/login"]')).not.toBeVisible();

    await page.goto("/");
    await expect(page.locator(".left-nav")).toBeVisible();
  });
});

test.describe("unauthenticated access", () => {
  test("protected pages redirect to login", async ({ page }) => {
    await page.goto("/deployments");
    // Redirect chain: /deployments → /login → Dex. Wait for the Dex
    // page to load before checking for the form input.
    await page.waitForURL(/\/auth\//, { timeout: 15000 });
    await expect(page.locator('input[name="login"]')).toBeVisible();
  });
});
