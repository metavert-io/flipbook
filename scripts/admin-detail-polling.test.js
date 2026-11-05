'use strict';

// Regression tests for the status polling on the publication detail page.
// Run with: node --test scripts/admin-detail-polling.test.js
//
// admin.js runs against a small DOM stub, so no browser or npm install is needed.

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'admin.js'), 'utf8');

function textNode() {
    return { textContent: '' };
}

// Loads the page once with the given rendered status. `responses` are the
// successive /status payloads; the last one repeats. Timers run until the
// script stops scheduling them or asks for a reload.
async function loadPage(renderedStatus, responses) {
    const nodes = {
        '[data-status-heading]': textNode(),
        '[data-status-message]': textNode(),
        '[data-poll-network]': textNode(),
    };
    const container = {
        getAttribute(name) {
            if (name === 'data-current-status') return renderedStatus;
            if (name === 'data-poll-url') return '/admin/flipbooks/test/status';
            return null;
        },
        querySelector(selector) {
            return nodes[selector] || null;
        },
    };
    const timers = [];
    const page = { fetches: 0, reloads: 0, nodes };
    const context = {
        console,
        fetch() {
            const payload = responses[Math.min(page.fetches, responses.length - 1)];
            page.fetches += 1;
            return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(payload) });
        },
        document: {
            readyState: 'complete',
            documentElement: { classList: { add() {} } },
            addEventListener() {},
            getElementById() { return null; },
            querySelector(selector) { return selector === '[data-poll-container]' ? container : null; },
            querySelectorAll() { return []; },
        },
    };
    context.window = context;
    context.setTimeout = (fn) => { timers.push(fn); return timers.length; };
    context.location = { reload() { page.reloads += 1; } };

    vm.runInNewContext(source, context);

    for (let step = 0; step < 50 && page.reloads === 0; step += 1) {
        await new Promise((resolve) => setImmediate(resolve));
        if (timers.length) timers.shift()();
        else if (step > 0) break;
    }
    return page;
}

const idle = (status, error = '') => ({ status, error, page_count: 12, jobs: [] });
const running = (status) => ({ status, error: '', page_count: 0, jobs: [{ status: 'running', progress: 40, progress_message: 'rendering' }] });

test('a publication already in error does not reload', async () => {
    const page = await loadPage('error', [idle('error', 'Conversion failed')]);
    assert.strictEqual(page.reloads, 0);
    assert.strictEqual(page.fetches, 1, 'polling should stop once nothing is in progress');
    assert.strictEqual(page.nodes['[data-status-message]'].textContent, 'Conversion failed');
});

test('a ready publication does not poll', async () => {
    const page = await loadPage('ready', [idle('ready')]);
    assert.strictEqual(page.fetches, 0);
    assert.strictEqual(page.reloads, 0);
});

test('a retry that fails again reloads once to show the new result', async () => {
    const page = await loadPage('error', [running('error'), idle('error', 'Still broken')]);
    assert.strictEqual(page.reloads, 1);
});

test('a retry that succeeds reloads', async () => {
    const page = await loadPage('error', [running('converting'), idle('ready')]);
    assert.strictEqual(page.reloads, 1);
});

test('a conversion that fails reloads, and the reloaded page settles', async () => {
    const first = await loadPage('converting', [running('converting'), idle('error', 'Conversion failed')]);
    assert.strictEqual(first.reloads, 1);
    const second = await loadPage('error', [idle('error', 'Conversion failed')]);
    assert.strictEqual(second.reloads, 0);
});

test('a conversion that finished before the first poll reloads', async () => {
    const page = await loadPage('converting', [idle('ready')]);
    assert.strictEqual(page.reloads, 1);
});
