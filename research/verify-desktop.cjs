const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const path = require('node:path');

(async () => {
  const browser = await chromium.connectOverCDP('http://127.0.0.1:9223');
  const page = browser.contexts().flatMap(context => context.pages()).find(p => p.url().includes('wails'));
  assert.ok(page, 'Wails WebView should expose its application page');
  await page.waitForFunction(() => !!window.go?.main?.App);
  const snapshot = await page.evaluate(() => window.go.main.App.GetSnapshot());
  assert.equal(snapshot.sources.length, 73, 'Desktop Go binding must return real sources');
  await page.locator('.article-row').first().waitFor();
  await page.screenshot({ path: path.join(__dirname, 'desktop-app.png') });
  console.log(`PASS: Wails runtime, Go bindings, ${snapshot.sources.length} sources, ${snapshot.articles.length} real articles.`);
  await browser.close();
})().catch(error => { console.error(error); process.exit(1); });
