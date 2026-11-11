'use strict';

// Regression tests for drag-and-drop on the admin upload page.
// Run with: node --test scripts/admin-dropzone.test.js
//
// Drag events need a real DataTransfer and a real <input type="file">, so this
// drives Chromium through Playwright against the upload form markup taken from
// web/templates/admin/upload.html. It skips when Playwright is not installed.

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

let chromium;
try {
    ({ chromium } = require('playwright'));
} catch (error) {
    chromium = null;
}

const root = path.join(__dirname, '..');
const script = fs.readFileSync(path.join(root, 'web', 'static', 'js', 'admin.js'), 'utf8');
const template = fs.readFileSync(path.join(root, 'web', 'templates', 'admin', 'upload.html'), 'utf8');
const form = template
    .slice(template.indexOf('<form action="/admin/upload"'), template.indexOf('</form>') + '</form>'.length)
    .replace('{{template "admin_csrf" .}}', '')
    .replace('{{.Formats}}', '.pdf, .pptx');

const html = '<!doctype html><html><body>' +
    '<div class="hidden" id="client-error"></div><div class="hidden" id="client-status"></div>' +
    '<div id="upload-section">' + form + '</div><section id="progress-section" class="hidden"></section>' +
    '<p id="outside">outside</p></body></html>';

// Dispatches a drag event carrying files named `names` on `selector`.
function drag(page, selector, type, names) {
    return page.evaluate(([selector, type, names]) => {
        const transfer = new DataTransfer();
        names.forEach((name) => transfer.items.add(new File(['x'], name)));
        const event = new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: transfer });
        document.querySelector(selector).dispatchEvent(event);
        return event.defaultPrevented;
    }, [selector, type, names]);
}

function state(page) {
    return page.evaluate(() => ({
        files: Array.from(document.getElementById('file').files).map((file) => file.name),
        listed: document.querySelectorAll('#file-list li').length,
        error: document.getElementById('client-error').classList.contains('hidden') ? '' : document.getElementById('client-error').textContent,
        highlighted: document.getElementById('dropzone').classList.contains('dragover'),
    }));
}

test('admin upload dropzone', { skip: chromium ? false : 'playwright is not installed' }, async (t) => {
    const browser = await chromium.launch();
    t.after(() => browser.close());

    async function open() {
        const page = await browser.newPage();
        await page.setContent(html);
        await page.addScriptTag({ content: script });
        return page;
    }

    await t.test('files dropped on the label text reach the file input', async () => {
        const page = await open();
        assert.strictEqual(await drag(page, '.dropzone-content strong', 'drop', ['deck.pdf', 'slides.PPTX']), true,
            'an unhandled drop makes the browser open the file and leave the page');
        const result = await state(page);
        assert.deepStrictEqual(result.files, ['deck.pdf', 'slides.PPTX']);
        assert.strictEqual(result.listed, 2);
        assert.strictEqual(result.error, '');
    });

    await t.test('unsupported files are skipped and named', async () => {
        const page = await open();
        await drag(page, '#dropzone', 'drop', ['deck.pdf', 'archive.zip']);
        let result = await state(page);
        assert.deepStrictEqual(result.files, ['deck.pdf']);
        assert.match(result.error, /archive\.zip/);

        await drag(page, '#dropzone', 'drop', ['photo.png']);
        result = await state(page);
        assert.deepStrictEqual(result.files, ['deck.pdf'], 'a drop with nothing usable keeps the current selection');
        assert.match(result.error, /photo\.png/);
    });

    await t.test('the file picker applies the same format rules', async () => {
        const page = await open();
        await page.setInputFiles('#file', [
            { name: 'deck.pdf', mimeType: 'application/pdf', buffer: Buffer.from('x') },
            { name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('x') },
        ]);
        const result = await state(page);
        assert.deepStrictEqual(result.files, ['deck.pdf']);
        assert.match(result.error, /notes\.txt/);
    });

    await t.test('the highlight survives crossing child elements', async () => {
        const page = await open();
        await drag(page, '#dropzone', 'dragenter', ['deck.pdf']);
        await drag(page, '.dropzone-content', 'dragenter', ['deck.pdf']);
        await drag(page, '#dropzone', 'dragleave', ['deck.pdf']);
        assert.strictEqual((await state(page)).highlighted, true);
        await drag(page, '.dropzone-content', 'dragleave', ['deck.pdf']);
        assert.strictEqual((await state(page)).highlighted, false);
    });

    await t.test('a file released outside the dropzone does not leave the page', async () => {
        const page = await open();
        assert.strictEqual(await drag(page, '#outside', 'dragover', ['deck.pdf']), true);
        assert.strictEqual(await drag(page, '#outside', 'drop', ['deck.pdf']), true);
        assert.deepStrictEqual((await state(page)).files, []);
    });
});
