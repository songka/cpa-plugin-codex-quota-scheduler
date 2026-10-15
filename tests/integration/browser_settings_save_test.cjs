const fs = require('fs');
const assert = require('assert');
const path = require('path');
const { chromium } = require('playwright');

const fixture = process.argv[2] || path.join('dist', 'verification', 'reserve-ui-render.html');
const html = fs.readFileSync(fixture, 'utf8');
const statusPath = `${fixture}.json`;
const status = JSON.parse(fs.readFileSync(statusPath, 'utf8'));
let currentSettings = { ...status.settings, webhook_configured: false, telegram_chat_id_configured: false };
const requests = [];
let failNextPatch = false;

function jsonResponse(body) {
  return JSON.stringify(body);
}

async function main() {
  const browserPath = process.env.CHROMIUM_PATH;
  const launchOptions = browserPath ? { executablePath: browserPath } : {};
  const browser = await chromium.launch({ ...launchOptions, headless: true });
  try {
    const page = await browser.newPage();
    const pageErrors = [];

    page.on('pageerror', error => pageErrors.push(String(error)));
    await page.route('**/*', async route => {
      const request = route.request();
      const url = new URL(request.url());
      const pathname = url.pathname;
      const body = request.postDataJSON ? request.postDataJSON() : undefined;
      const isDocument = request.resourceType() === 'document';
      if (pathname.includes('/v0/management')) {
        requests.push({ method: request.method(), pathname, body });
      }

      if (isDocument) {
        await route.fulfill({ status: 200, contentType: 'text/html', body: html });
        return;
      }
      if (pathname === '/v0/management/plugins/codex-quota-reserve/status') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: jsonResponse({ ...status, settings: currentSettings }),
        });
        return;
      }
      if (pathname === '/v0/management/plugins/codex-quota-reserve/settings' &&
          request.method() === 'PUT') {
        currentSettings = { ...currentSettings, ...(body || {}) };
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: jsonResponse(currentSettings),
        });
        return;
      }
      if (pathname === '/v0/management/plugins/codex-quota-reserve/config' &&
          request.method() === 'PATCH') {
        if (failNextPatch) {
          failNextPatch = false;
          await route.fulfill({
            status: 502,
            contentType: 'application/json',
            body: jsonResponse({ error: 'simulated CPA sync failure' }),
          });
          return;
        }
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: jsonResponse({ ok: true }),
        });
        return;
      }
      if (pathname === '/v0/management/plugins/codex-quota-reserve/retry/status') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: jsonResponse({ mode: 'off', requests: 0, retries: 0, succeeded: 0, failed: 0, shadow_hits: 0 }),
        });
        return;
      }
      if (pathname === '/v0/management/plugins/codex-quota-reserve/history') {
        const now = Date.now();
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: jsonResponse({
            a: [
              { at: new Date(now - 3600000).toISOString(), five_hour: 55, long_window: 60, event: 'reset' },
              { at: new Date(now).toISOString(), five_hour: 50, long_window: 50, event: 'protection' },
            ],
          }),
        });
        return;
      }
      if (pathname.includes('/v0/management')) {
        await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
        return;
      }
      await route.fulfill({ status: 404, contentType: 'text/plain', body: 'Not found' });
    });

    await page.goto('http://reserve.test/plugins/codex-quota-reserve/status');
    await page.fill('#managementKey', 'integration-fixture-key');
    const loadData = page.locator('#loadData');
    if (await loadData.isVisible()) {
      await Promise.all([
        page.waitForResponse(response =>
          response.url().endsWith('/v0/management/plugins/codex-quota-reserve/status') &&
          response.request().method() === 'GET'),
        loadData.click(),
      ]);
    }
    await page.waitForFunction(() => !document.getElementById('settingsPanel')?.hidden);
    await page.locator('article.card').first().waitFor();
    await page.click('#historyLoad');
    await page.locator('#historyChart polyline').waitFor();
    assert.strictEqual(await page.locator('#historyChart circle').count(), 2);
    await page.locator('button.openEdit').first().click();
    assert(await page.locator('#editReserve5H').isVisible());
    assert.strictEqual(await page.locator('#editReserveLong').inputValue(), '');
    await page.locator('#editDialog').evaluate(dialog => dialog.close());
    await page.locator('#settingsPanel summary').click();
    assert.strictEqual(await page.locator('#reserve5H').inputValue(), '10');
    assert.strictEqual(await page.locator('#reserveMargin').inputValue(), '2');
    assert(await page.locator('#reserveStrict').isChecked());
    await page.fill('#reserveWarning', '17');

    const beforeFirstSave = requests.length;
    const [putResponse, patchResponse] = await Promise.all([
      page.waitForResponse(response =>
        response.url().endsWith('/v0/management/plugins/codex-quota-reserve/settings') &&
        response.request().method() === 'PUT'),
      page.waitForResponse(response =>
        response.url().endsWith('/v0/management/plugins/codex-quota-reserve/config') &&
        response.request().method() === 'PATCH'),
      page.click('#saveSettings'),
    ]);
    assert(putResponse.ok(), 'settings PUT failed');
    assert(patchResponse.ok(), 'CPA config PATCH failed');
    const firstSaveRequests = requests.slice(beforeFirstSave);
    const put = firstSaveRequests.find(item => item.method === 'PUT' && item.pathname.endsWith('/settings'));
    const patch = firstSaveRequests.find(item => item.method === 'PATCH' && item.pathname.endsWith('/config'));
    assert(put, 'settings PUT was not sent');
    assert(patch, 'CPA config PATCH was not sent');
    assert.strictEqual(put.body.reserve_warning_percent, 17);
    for (const forbidden of ['webhook_url', 'telegram_chat_id', 'enabled', 'store']) {
      assert(!(forbidden in patch.body), `private/host field leaked into CPA PATCH: ${forbidden}`);
    }
    assert.strictEqual(patch.body.reserve_warning_percent, 17);
    await page.waitForFunction(() => document.getElementById('reserveWarning')?.value === '17');
    assert.strictEqual(currentSettings.reserve_warning_percent, 17);

    failNextPatch = true;
    await page.fill('#reserveWarning', '18');
    await Promise.all([
      page.waitForResponse(response =>
        response.url().endsWith('/v0/management/plugins/codex-quota-reserve/config') &&
        response.request().method() === 'PATCH'),
      page.click('#saveSettings'),
    ]);
    await page.waitForFunction(() => {
      const text = document.getElementById('notice')?.textContent || '';
      return text.includes('同步') || text.toLowerCase().includes('sync');
    });
    const failedNotice = await page.locator('#notice').textContent();
    assert(failedNotice.includes('同步') || failedNotice.toLowerCase().includes('sync'));

    await Promise.all([
      page.waitForResponse(response =>
        response.url().endsWith('/v0/management/plugins/codex-quota-reserve/config') &&
        response.request().method() === 'PATCH'),
      page.click('#saveSettings'),
    ]);
    const successfulRetry = requests.filter(item =>
      item.method === 'PATCH' && item.pathname.endsWith('/config')).at(-1);
    assert(successfulRetry, 'retry PATCH was not sent');
    assert.strictEqual(successfulRetry.body.reserve_warning_percent, 18);
    assert(!pageErrors.length, `browser page errors: ${pageErrors.join('; ')}`);
    if (process.env.RESERVE_SCREENSHOT_DIR) {
      const output = path.resolve(process.env.RESERVE_SCREENSHOT_DIR);
      fs.mkdirSync(output, { recursive: true });
      for (const [name, viewport] of [
        ['desktop', { width: 1440, height: 1000 }],
        ['mobile', { width: 390, height: 844 }],
      ]) {
        await page.setViewportSize(viewport);
        await page.screenshot({ path: path.join(output, `settings-${name}.png`), fullPage: true });
        assert(await page.evaluate(() =>
          document.documentElement.scrollWidth <= window.innerWidth),
        `${name} page has horizontal overflow`);
      }
    }
    console.log('Browser passed: account editor, history, settings PUT, CPA config PATCH, private-field filtering, partial failure, and retry.');
  } finally {
    await browser.close();
  }
}

main().catch(error => {
  console.error(error.stack || error);
  process.exitCode = 1;
});
