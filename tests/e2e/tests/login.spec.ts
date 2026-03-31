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
    await expect(page.locator(".sign-in")).toBeVisible();
    await expect(page.locator(".left-nav")).not.toBeVisible();
  });

  test("login redirects to authenticated apps page", async ({ page }) => {
    await page.goto("/");
    await page.locator('.sign-in a[href="/login"]').click();
    await dexLogin(page);

    await expect(page).toHaveURL("/");
    await expect(page.locator(".left-nav")).toBeVisible();
    await expect(page.locator(".sign-in")).not.toBeVisible();
  });

  test("session-expired overlay is hidden after login", async ({ page }) => {
    await page.goto("/");
    await page.locator('.sign-in a[href="/login"]').click();
    await dexLogin(page);

    const overlay = page.locator("#session-expired-overlay");
    await expect(overlay).toBeHidden();
    // Verify computed style — catches CSS specificity bugs where the
    // class is present but overridden by a later rule.
    const display = await overlay.evaluate(
      (el) => getComputedStyle(el).display,
    );
    expect(display).toBe("none");
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
    await expect(page.locator(".sign-in")).not.toBeVisible();

    await page.goto("/");
    await expect(page.locator(".left-nav")).toBeVisible();
  });
});

test.describe("unauthenticated access", () => {
  test("protected pages redirect to login", async ({ page }) => {
    await page.goto("/deployments");
    // Should end up on the Dex login form (redirected through /login).
    await expect(page.locator('input[name="login"]')).toBeVisible();
  });
});
