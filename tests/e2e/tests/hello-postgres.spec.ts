import { test, expect, type BrowserContext, type Page } from "@playwright/test";

// Two-user RLS isolation end-to-end. Each user runs in its own
// BrowserContext so Dex + blockyard + Shiny sessions never bleed.
// The Shiny app talks directly to Postgres using per-user credentials
// minted by vault — blockyard is not on the data path. This spec
// validates the full wire: OIDC login → provisioning → session header
// injection → vault creds fetch → PG connect → RLS filtering.
//
// Board IDs and JSON payloads are unique per user so cross-leakage
// assertions are unambiguous. Uses a single `serial` test because
// state is ordered: user1 must write before user2 can assert not-seen.

const APP = "/app/hello-postgres/";
const USER1 = { email: "demo@example.com", password: "password" };
const USER2 = { email: "demo2@example.com", password: "password" };

async function dexLogin(page: Page, email: string, password: string) {
  await page.waitForSelector('input[name="login"]', { timeout: 30_000 });
  await page.fill('input[name="login"]', email);
  await page.fill('input[name="password"]', password);
  await page.click('button[type="submit"]');
  await page.waitForLoadState("networkidle");
}

// openApp navigates to the hello-postgres app and waits for Shiny to
// finish handshake. Shiny marks the body busy during connect; the
// #save button is disabled until `server` has run at least once.
async function openApp(page: Page) {
  await page.goto(APP);
  // Shiny inputs render eagerly but bindings only wire after WS is up.
  // Wait for the action button to be visible + enabled before proceeding.
  const save = page.locator("#save");
  await expect(save).toBeVisible({ timeout: 60_000 });
  await expect(save).toBeEnabled({ timeout: 60_000 });
}

async function loginAndOpen(
  context: BrowserContext,
  user: { email: string; password: string },
): Promise<Page> {
  const page = await context.newPage();
  await page.goto("/login");
  await dexLogin(page, user.email, user.password);
  await openApp(page);
  return page;
}

async function saveBoard(page: Page, boardId: string, data: string) {
  await page.fill("#board_id", boardId);
  await page.fill("#data", data);
  await page.click("#save");
  await expect(page.locator("#status")).toContainText(`saved ${boardId}`);
}

async function listBoards(page: Page): Promise<string> {
  await page.click("#list");
  await expect(page.locator("#status")).toContainText("listed boards");
  // Return the rendered text inside the boards table for negative
  // assertions (doesn't matter whether the table is empty-HTML or
  // absent; .innerText gives "" either way).
  return (await page.locator("#boards").innerText()) ?? "";
}

test.describe.serial("hello-postgres RLS isolation", () => {
  let ctx1: BrowserContext;
  let ctx2: BrowserContext;
  let page1: Page;
  let page2: Page;
  // Unique per-run so re-runs against the same dev stack don't collide
  // on the (owner_sub, board_id) unique index.
  const stamp = Date.now().toString(36);
  const board1 = `user1-${stamp}`;
  const board2 = `user2-${stamp}`;

  test.beforeAll(async ({ browser }) => {
    ctx1 = await browser.newContext();
    ctx2 = await browser.newContext();
    page1 = await loginAndOpen(ctx1, USER1);
    page2 = await loginAndOpen(ctx2, USER2);
  });

  test.afterAll(async () => {
    await ctx1?.close();
    await ctx2?.close();
  });

  test("user1 saves a board and sees only their own in list", async () => {
    await saveBoard(page1, board1, `{"owner":"user1","stamp":"${stamp}"}`);
    const listed = await listBoards(page1);
    expect(listed).toContain(board1);
    expect(listed).not.toContain(board2);
  });

  test("user2 does not see user1's board before saving", async () => {
    const listed = await listBoards(page2);
    expect(listed).not.toContain(board1);
  });

  test("user2 saves their own and sees only that in list", async () => {
    await saveBoard(page2, board2, `{"owner":"user2","stamp":"${stamp}"}`);
    const listed = await listBoards(page2);
    expect(listed).toContain(board2);
    expect(listed).not.toContain(board1);
  });

  test("user1 still sees only their own after user2 wrote", async () => {
    const listed = await listBoards(page1);
    expect(listed).toContain(board1);
    expect(listed).not.toContain(board2);
  });

  test("user1 loads their board and the JSON round-trips", async () => {
    await page1.fill("#board_id", board1);
    await page1.click("#load");
    await expect(page1.locator("#status")).toContainText(`loaded ${board1}`);
    const got = await page1.locator("#data").inputValue();
    // Postgres stores jsonb canonically and returns it with spaces
    // around colons on text-cast, so substring-matching the literal
    // JSON payload is brittle. Parse and assert on values.
    const parsed = JSON.parse(got);
    expect(parsed.owner).toBe("user1");
    expect(parsed.stamp).toBe(stamp);
  });

  test("user2 loading user1's board id returns not-visible", async () => {
    await page2.fill("#board_id", board1);
    await page2.click("#load");
    // RLS filters the SELECT to zero rows — the app reports "no board
    // visible" rather than leaking user1's JSON back.
    await expect(page2.locator("#status")).toContainText(
      `no board '${board1}' visible`,
    );
  });
});
