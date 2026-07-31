import { chromium } from 'playwright';
const b = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const p = await b.newPage({ viewport: { width: 1280, height: 900 } });
const errs = [];
p.on('console', m => { if (m.type() === 'error') errs.push(m.text()); });
p.on('pageerror', e => errs.push('PAGEERROR: ' + e.message));
await p.goto('http://127.0.0.1:8799/', { waitUntil: 'networkidle' });
await p.getByRole('button', { name: 'Browse' }).click();
// Empty query no longer auto-searches; type and submit like a user.
await p.getByLabel('Search remote providers').fill('ink');
await p.getByRole('button', { name: 'Search' }).click();
await p.waitForSelector('.listing', { timeout: 8000 });
await p.waitForTimeout(1200);
await p.screenshot({ path: 'browse.png', fullPage: true });
console.log('CONSOLE ERRORS:', errs.length ? errs.join('\n') : 'none');
console.log('CARDS:', await p.locator('.listing').count());
console.log('BADGES:', (await p.locator('.badge').allTextContents()).join(', '));
// Tab selection visibility: computed background of the active tab.
const bg = await p.locator('.tabs button.on').evaluate(el => getComputedStyle(el).backgroundColor);
console.log('ACTIVE TAB BG:', bg);
// Library chip styling intact?
await p.getByRole('button', { name: 'Library' }).click();
await p.waitForTimeout(400);
const chipBg = await p.locator('.grid .chip').first().evaluate(el => getComputedStyle(el).backgroundColor).catch(() => 'n/a');
console.log('LIBRARY CHIP BG:', chipBg);
await b.close();
