'use strict';

document.documentElement.classList.add('js');

(function () {
    var pollingInterval = 1500;
    var pollingTimeout = 60 * 60 * 1000;

    function byId(id) {
        return document.getElementById(id);
    }

    function formatSize(bytes) {
        if (!Number.isFinite(bytes) || bytes < 0) return '';
        if (bytes < 1024) return bytes + ' B';
        if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB';
        if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + ' MB';
        return (bytes / 1073741824).toFixed(1) + ' GB';
    }

    function delay(milliseconds) {
        return new Promise(function (resolve) { window.setTimeout(resolve, milliseconds); });
    }

    function showClientError(message) {
        var region = byId('client-error');
        if (!region) return;
        region.textContent = message;
        region.classList.remove('hidden');
    }

    function showClientStatus(message) {
        var region = byId('client-status');
        if (!region) return;
        region.textContent = message;
        region.classList.remove('hidden');
    }

    function clearClientMessages() {
        ['client-error', 'client-status'].forEach(function (id) {
            var region = byId(id);
            if (region) {
                region.textContent = '';
                region.classList.add('hidden');
            }
        });
    }

    function initNavigation() {
        var toggle = document.querySelector('[data-nav-toggle]');
        var links = document.querySelector('[data-nav-links]');
        if (!toggle || !links) return;
        function close() {
            links.classList.remove('nav-open');
            toggle.setAttribute('aria-expanded', 'false');
        }
        toggle.addEventListener('click', function () {
            var open = links.classList.toggle('nav-open');
            toggle.setAttribute('aria-expanded', String(open));
        });
        document.addEventListener('keydown', function (event) {
            if (event.key === 'Escape') {
                close();
                toggle.focus();
            }
        });
        window.addEventListener('resize', function () {
            if (window.innerWidth > 900) close();
        });
    }

    function initTabs() {
        var tabs = Array.prototype.slice.call(document.querySelectorAll('.upload-tab'));
        if (!tabs.length) return;
        function activate(tab, focus) {
            tabs.forEach(function (candidate) {
                var selected = candidate === tab;
                candidate.classList.toggle('active', selected);
                candidate.setAttribute('aria-selected', String(selected));
                candidate.tabIndex = selected ? 0 : -1;
                var panel = byId(candidate.getAttribute('data-tab'));
                if (panel) {
                    panel.classList.toggle('active', selected);
                    panel.hidden = !selected;
                }
            });
            if (focus) tab.focus();
        }
        tabs.forEach(function (tab, index) {
            tab.addEventListener('click', function () { activate(tab, false); });
            tab.addEventListener('keydown', function (event) {
                var target = null;
                if (event.key === 'ArrowRight') target = tabs[(index + 1) % tabs.length];
                if (event.key === 'ArrowLeft') target = tabs[(index - 1 + tabs.length) % tabs.length];
                if (event.key === 'Home') target = tabs[0];
                if (event.key === 'End') target = tabs[tabs.length - 1];
                if (target) {
                    event.preventDefault();
                    activate(target, true);
                }
            });
            var panel = byId(tab.getAttribute('data-tab'));
            if (panel) panel.hidden = !tab.classList.contains('active');
        });
    }

    function initConfirmations() {
        document.querySelectorAll('form[data-confirm]').forEach(function (form) {
            form.addEventListener('submit', function (event) {
                if (!window.confirm(form.getAttribute('data-confirm'))) event.preventDefault();
            });
        });
        document.querySelectorAll('form[method="POST"]').forEach(function (form) {
            if (form.matches('[data-upload-form], [data-import-form], [data-bulk-form], [data-replace-form], [data-confirm]')) return;
            form.addEventListener('submit', function () {
                form.setAttribute('aria-busy', 'true');
                window.setTimeout(function () {
                    form.querySelectorAll('button[type="submit"]').forEach(function (button) { button.disabled = true; });
                }, 0);
            });
        });
    }

    function copyText(value) {
        if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(value);
        return new Promise(function (resolve, reject) {
            var temporary = document.createElement('textarea');
            temporary.value = value;
            temporary.setAttribute('readonly', '');
            temporary.style.position = 'fixed';
            temporary.style.opacity = '0';
            document.body.appendChild(temporary);
            temporary.select();
            try {
                if (document.execCommand('copy')) resolve();
                else reject(new Error('Copy command was rejected'));
            } catch (error) {
                reject(error);
            } finally {
                temporary.remove();
            }
        });
    }

    function initCopyButtons() {
        document.querySelectorAll('[data-copy-target]').forEach(function (button) {
            button.addEventListener('click', function () {
                var target = byId(button.getAttribute('data-copy-target'));
                if (!target) return;
                var value = 'value' in target ? target.value : target.textContent;
                copyText(value).then(function () {
                    var previous = button.textContent;
                    button.textContent = 'Copied';
                    showClientStatus('Copied to clipboard.');
                    window.setTimeout(function () { button.textContent = previous; }, 1800);
                }).catch(function () {
                    if (target.focus) target.focus();
                    if (target.select) target.select();
                    showClientError('Automatic copy was unavailable. The text is selected; copy it manually.');
                });
            });
        });
    }

    function initBulkActions() {
        var form = document.querySelector('[data-bulk-form]');
        if (!form) return;
        var items = Array.prototype.slice.call(form.querySelectorAll('[data-bulk-item]'));
        var selectAll = form.querySelector('[data-select-all]');
        var count = form.querySelector('[data-selected-count]');
        var action = form.querySelector('[data-bulk-action]');
        function update() {
            var selected = items.filter(function (item) { return item.checked; }).length;
            if (count) count.textContent = String(selected);
            if (selectAll) {
                selectAll.checked = selected > 0 && selected === items.length;
                selectAll.indeterminate = selected > 0 && selected < items.length;
            }
        }
        if (selectAll) {
            selectAll.addEventListener('change', function () {
                items.forEach(function (item) { item.checked = selectAll.checked; });
                update();
            });
        }
        items.forEach(function (item) { item.addEventListener('change', update); });
        form.addEventListener('submit', function (event) {
            clearClientMessages();
            var selected = items.filter(function (item) { return item.checked; }).length;
            if (!selected) {
                event.preventDefault();
                showClientError('Select at least one publication before applying a bulk action.');
                return;
            }
            var currentAction = action ? action.value : '';
            var tags = byId('bulk-tags');
            var collection = byId('bulk-collection');
            if ((currentAction === 'add_tags' || currentAction === 'remove_tags') && tags && !tags.value.trim()) {
                event.preventDefault();
                showClientError('Enter at least one tag for this bulk action.');
                tags.focus();
                return;
            }
            if ((currentAction === 'add_collection' || currentAction === 'remove_collection') && collection && !collection.value) {
                event.preventDefault();
                showClientError('Choose a collection for this bulk action.');
                collection.focus();
                return;
            }
            if (currentAction === 'trash' && !window.confirm('Move the selected publications to trash? Public access stops immediately.')) {
                event.preventDefault();
                return;
            }
            form.setAttribute('aria-busy', 'true');
        });
        update();
    }

    function makeCell(row, className) {
        var cell = document.createElement('td');
        if (className) cell.className = className;
        row.appendChild(cell);
        return cell;
    }

    function createQueueRow(file, index) {
        var body = byId('upload-queue-body');
        var row = document.createElement('tr');
        row.id = 'upload-item-' + index;
        var heading = document.createElement('th');
        heading.scope = 'row';
        var name = document.createElement('strong');
        name.textContent = file.name;
        var size = document.createElement('span');
        size.className = 'table-secondary';
        size.textContent = formatSize(file.size);
        heading.appendChild(name);
        heading.appendChild(size);
        row.appendChild(heading);
        row._uploadCell = makeCell(row);
        row._conversionCell = makeCell(row);
        row._resultCell = makeCell(row);
        row._uploadCell.textContent = 'Waiting';
        row._conversionCell.textContent = 'Not started';
        row._resultCell.textContent = '—';
        body.appendChild(row);
        return row;
    }

    function responseError(xhr) {
        var fallback = 'The request failed with HTTP ' + xhr.status + '.';
        try {
            var response = JSON.parse(xhr.responseText);
            if (response.error) return response.error;
            if (response.items && response.items[0] && response.items[0].error) return response.items[0].error;
        } catch (ignore) {
            if (xhr.responseText && xhr.responseText.length < 500) return xhr.responseText;
        }
        return fallback;
    }

    // Large documents are sent as a resumable sequence of chunk requests. The
    // edge proxy aborts any request whose body takes longer than 60 seconds,
    // so a single 177 MB request fails on anything slower than ~24 Mbit/s.
    // Each chunk is sized from measured throughput to take about 8 seconds,
    // a stalled chunk is aborted after 30 seconds without progress, and
    // failed chunks are retried from the offset the server confirms.
    var chunkMinBytes = 1048576;
    var chunkTargetSeconds = 8;
    var chunkStallTimeout = 30 * 1000;
    var chunkMaxRetries = 10;

    function csrfTokenFor(form) {
        var input = form && form.querySelector('input[name="csrf_token"]');
        return input ? input.value : '';
    }

    function hiddenFields(form) {
        var data = new FormData();
        Array.prototype.slice.call(form.querySelectorAll('input[type="hidden"][name]')).forEach(function (input) {
            data.append(input.name, input.value);
        });
        return data;
    }

    function requestFailure(message, status, retryable, received) {
        var error = new Error(message);
        error.status = status;
        error.retryable = retryable;
        error.received = received;
        return error;
    }

    function parseJSON(text) {
        try { return JSON.parse(text); } catch (ignore) { return null; }
    }

    function sessionRequest(method, url, token, body, onProgress) {
        return new Promise(function (resolve, reject) {
            var xhr = new XMLHttpRequest();
            var lastActivity = Date.now();
            var stalled = false;
            var watchdog = window.setInterval(function () {
                if (Date.now() - lastActivity > chunkStallTimeout) {
                    stalled = true;
                    xhr.abort();
                }
            }, 1000);
            xhr.open(method, url);
            xhr.setRequestHeader('X-Requested-With', 'XMLHttpRequest');
            xhr.setRequestHeader('Accept', 'application/json');
            xhr.setRequestHeader('X-CSRF-Token', token);
            var isBlob = typeof Blob !== 'undefined' && body instanceof Blob;
            if (body) xhr.setRequestHeader('Content-Type', isBlob ? 'application/octet-stream' : 'application/json');
            xhr.upload.addEventListener('progress', function (event) {
                lastActivity = Date.now();
                if (onProgress) onProgress(event.loaded);
            });
            xhr.upload.addEventListener('load', function () { lastActivity = Date.now(); });
            xhr.addEventListener('load', function () {
                window.clearInterval(watchdog);
                var response = parseJSON(xhr.responseText);
                if (xhr.status >= 200 && xhr.status < 300) {
                    if (response || xhr.status === 204) {
                        resolve(response || {});
                        return;
                    }
                    // An expired sign-in redirects to the HTML login page.
                    reject(requestFailure('Your sign-in has expired. Sign in again in another tab, then retry the upload.', xhr.status, false));
                    return;
                }
                var message = response && response.error ? response.error : 'The upload request failed with HTTP ' + xhr.status + '.';
                if (xhr.status === 403 && !(response && response.error)) message = 'The security token was rejected. Reload the page and upload again.';
                var retryable = xhr.status === 408 || xhr.status === 409 || xhr.status === 429 || xhr.status >= 500;
                reject(requestFailure(message, xhr.status, retryable, response && Number.isFinite(response.received) ? response.received : null));
            });
            xhr.addEventListener('error', function () {
                window.clearInterval(watchdog);
                reject(requestFailure('The connection was interrupted.', 0, true));
            });
            xhr.addEventListener('abort', function () {
                window.clearInterval(watchdog);
                reject(requestFailure(stalled ? 'The connection stalled.' : 'The upload was cancelled.', 0, stalled));
            });
            xhr.send(body ? (isBlob ? body : JSON.stringify(body)) : null);
        });
    }

    // Uploads file in chunks and resolves with the completed upload session ID.
    // onProgress receives the bytes the server has confirmed plus the bytes of
    // the chunk in flight; onStatus receives retry notices.
    function uploadInChunks(form, file, onProgress, onStatus) {
        var token = csrfTokenFor(form);
        var sessionURL = null;
        return sessionRequest('POST', '/admin/uploads', token, { filename: file.name, size: file.size }).then(function (session) {
            sessionURL = session.url || '/admin/uploads/' + encodeURIComponent(session.id);
            var chunkSize = session.chunk_size || 4 * chunkMinBytes;
            var maxChunk = session.max_chunk_size || 32 * chunkMinBytes;
            var offset = session.received || 0;
            var failures = 0;
            function next() {
                if (offset >= file.size) return session.id;
                var start = offset;
                var end = Math.min(file.size, start + chunkSize);
                var started = Date.now();
                return sessionRequest('PUT', sessionURL + '?offset=' + start, token, file.slice(start, end), function (loaded) {
                    onProgress(start + Math.min(loaded, end - start));
                }).then(function (status) {
                    failures = 0;
                    var seconds = Math.max((Date.now() - started) / 1000, 0.05);
                    var fitted = Math.floor(((end - start) / seconds) * chunkTargetSeconds / chunkMinBytes) * chunkMinBytes;
                    chunkSize = Math.max(chunkMinBytes, Math.min(maxChunk, chunkSize * 2, fitted));
                    offset = status.received;
                    onProgress(offset);
                    return next();
                }, function (error) {
                    if (!error.retryable) throw error;
                    failures += 1;
                    if (failures > chunkMaxRetries) {
                        throw requestFailure(error.message + ' The upload stopped after ' + chunkMaxRetries + ' retries; check your connection and upload the file again.', error.status, false);
                    }
                    chunkSize = Math.max(chunkMinBytes, Math.floor(chunkSize / 2 / chunkMinBytes) * chunkMinBytes);
                    var wait = Math.min(30, Math.pow(2, failures - 1));
                    onStatus(error.message + ' Retrying in ' + wait + 's (' + failures + '/' + chunkMaxRetries + ')…');
                    return delay(wait * 1000).then(function () {
                        return sessionRequest('GET', sessionURL, token).then(function (current) {
                            offset = current.received;
                        }, function (statusError) {
                            if (!statusError.retryable) throw statusError;
                        });
                    }).then(function () {
                        onProgress(offset);
                        return next();
                    });
                });
            }
            return next();
        }).catch(function (error) {
            if (sessionURL) sessionRequest('DELETE', sessionURL, token).catch(function () { /* expires on its own */ });
            throw error;
        });
    }

    function uploadProgressText(loaded, total, started) {
        var percentage = total > 0 ? Math.min(100, Math.floor((loaded / total) * 100)) : 100;
        var elapsed = Math.max((Date.now() - started) / 1000, 0.1);
        var speed = loaded / elapsed;
        var remaining = speed > 0 ? Math.ceil((total - loaded) / speed) : 0;
        return {
            percentage: percentage,
            detail: formatSize(loaded) + ' of ' + formatSize(total) + (remaining > 1 && loaded < total ? ' · about ' + formatDuration(remaining) + ' remaining' : '')
        };
    }

    function formatDuration(seconds) {
        if (seconds < 90) return seconds + 's';
        return Math.round(seconds / 60) + ' min';
    }

    function showUploadProgress(row, loaded, total, started) {
        var state = uploadProgressText(loaded, total, started);
        row._uploadCell.textContent = 'Uploading ' + state.percentage + '%';
        var progress = byId('upload-progress');
        var fill = byId('progress-fill');
        var detail = byId('upload-detail');
        if (progress) {
            progress.classList.remove('hidden');
            progress.setAttribute('aria-valuenow', String(state.percentage));
        }
        if (fill) fill.style.width = state.percentage + '%';
        if (detail) detail.textContent = state.detail;
    }

    function uploadRequest(form, file, single, row) {
        if (!file) return submitUploadForm(form, new FormData(form), row, 'Downloading from Google Slides');
        var started = Date.now();
        return uploadInChunks(form, file, function (loaded) {
            showUploadProgress(row, loaded, file.size, started);
        }, function (message) {
            row._uploadCell.textContent = message;
        }).then(function (uploadID) {
            var data = hiddenFields(form);
            if (single) {
                var title = form.querySelector('input[name="title"]');
                if (title) data.append('title', title.value);
            }
            data.append('upload_id', uploadID);
            return submitUploadForm(form, data, row, 'Finishing upload…');
        });
    }

    function submitUploadForm(form, data, row, label) {
        return new Promise(function (resolve, reject) {
            var xhr = new XMLHttpRequest();
            xhr.open('POST', form.action);
            xhr.setRequestHeader('X-Requested-With', 'XMLHttpRequest');
            xhr.setRequestHeader('Accept', 'application/json');
            xhr.timeout = 10 * 60 * 1000;
            xhr.addEventListener('load', function () {
                var response = null;
                try { response = JSON.parse(xhr.responseText); } catch (ignore) { /* handled below */ }
                if (xhr.status < 200 || xhr.status >= 300 || !response) {
                    reject(new Error(responseError(xhr)));
                    return;
                }
                var item = response.items && response.items.length ? response.items[0] : response;
                if (!item || !item.id) {
                    reject(new Error(item && item.error ? item.error : responseError(xhr)));
                    return;
                }
                resolve(item);
            });
            xhr.addEventListener('error', function () { reject(new Error('Network error while finishing the upload. Check the library before retrying so you do not create duplicates.')); });
            xhr.addEventListener('timeout', function () { reject(new Error('Upload timed out. Check the library before retrying so you do not create duplicates.')); });
            xhr.addEventListener('abort', function () { reject(new Error('Upload was cancelled.')); });
            row._uploadCell.textContent = label;
            xhr.send(data);
        });
    }

    function initReplaceUpload() {
        var form = document.querySelector('[data-replace-form]');
        if (!form) return;
        var input = form.querySelector('input[type="file"]');
        var status = form.querySelector('[data-replace-status]');
        var button = form.querySelector('button[type="submit"]');
        function say(text) {
            if (status) status.textContent = text;
        }
        form.addEventListener('submit', function (event) {
            if (!input || !input.files || !input.files.length) return;
            event.preventDefault();
            clearClientMessages();
            var file = input.files[0];
            var started = Date.now();
            if (button) button.disabled = true;
            input.disabled = true;
            form.setAttribute('aria-busy', 'true');
            say('Uploading 0%');
            uploadInChunks(form, file, function (loaded) {
                var state = uploadProgressText(loaded, file.size, started);
                say('Uploading ' + state.percentage + '% · ' + state.detail);
            }, say).then(function (uploadID) {
                say('Finishing upload…');
                var data = hiddenFields(form);
                data.append('upload_id', uploadID);
                return fetch(form.action, {
                    method: 'POST', body: data, credentials: 'same-origin',
                    headers: { 'Accept': 'application/json', 'X-Requested-With': 'XMLHttpRequest' }
                }).then(function (response) {
                    return response.json().catch(function () { return null; }).then(function (body) {
                        if (!response.ok || !body || !body.id) {
                            throw new Error(body && body.error ? body.error : 'The replacement could not be saved (HTTP ' + response.status + '). Reload the page to check its revisions before retrying.');
                        }
                        return body;
                    });
                });
            }).then(function (body) {
                say('Uploaded. Conversion has started.');
                window.location.href = '/admin/flipbooks/' + encodeURIComponent(body.id) + '?notice=' + encodeURIComponent('Replacement uploaded. The current version remains live until conversion succeeds.');
            }).catch(function (error) {
                say('');
                showClientError(error.message);
                if (button) button.disabled = false;
                input.disabled = false;
                form.removeAttribute('aria-busy');
            });
        });
    }

    function addResultLink(row, href, text, primary) {
        var link = document.createElement('a');
        link.href = href;
        link.textContent = text;
        link.className = primary ? 'btn btn-primary btn-sm' : 'btn btn-sm';
        row._resultCell.appendChild(link);
    }

    function setQueueError(row, message, conversion) {
        var cell = conversion ? row._conversionCell : row._uploadCell;
        cell.textContent = message;
        cell.classList.add('field-error');
        row._resultCell.textContent = '';
    }

    function activeJob(jobs) {
        if (!jobs || !jobs.length) return null;
        for (var index = 0; index < jobs.length; index += 1) {
            var status = jobs[index].status;
            if (status === 'queued' || status === 'running' || status === 'retry') return jobs[index];
        }
        return null;
    }

    function pollPublication(statusURL, row) {
        var deadline = Date.now() + pollingTimeout;
        var networkFailures = 0;
        function check() {
            if (Date.now() > deadline) {
                row._conversionCell.textContent = 'Status unknown: polling timed out. Open Manage to check the real conversion state.';
                return Promise.resolve(false);
            }
            return fetch(statusURL, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin' }).then(function (response) {
                if (!response.ok) throw new Error('Status request failed with HTTP ' + response.status + '.');
                return response.json();
            }).then(function (data) {
                networkFailures = 0;
                var job = activeJob(data.jobs);
                var progress = job && Number.isFinite(job.progress) ? job.progress : (data.revisions && data.revisions[0] ? data.revisions[0].progress : 0);
                if (data.status === 'ready' && !job) {
                    row._conversionCell.textContent = 'Ready · ' + data.page_count + ' pages';
                    row._resultCell.textContent = '';
                    addResultLink(row, '/admin/flipbooks/' + data.id, 'Manage', false);
                    addResultLink(row, '/v/' + data.slug, 'Preview', true);
                    return true;
                }
                if (data.status === 'error') {
                    row._conversionCell.textContent = data.error || 'Conversion failed';
                    row._conversionCell.classList.add('field-error');
                    row._resultCell.textContent = '';
                    addResultLink(row, '/admin/flipbooks/' + data.id, 'Review and retry', false);
                    return true;
                }
                row._conversionCell.textContent = (data.status || 'pending') + (progress ? ' · ' + progress + '%' : '') + (job && job.progress_message ? ' · ' + job.progress_message : '');
                return delay(pollingInterval).then(check);
            }).catch(function (error) {
                networkFailures += 1;
                if (networkFailures >= 5) {
                    row._conversionCell.textContent = 'Status unknown: repeated status-request failures. Open Manage to check the real conversion state. ' + error.message;
                    return false;
                }
                row._conversionCell.textContent = 'Status request failed; retrying (' + networkFailures + '/5). Conversion may still be running.';
                return delay(pollingInterval * networkFailures).then(check);
            });
        }
        return check();
    }

    function initUploadQueue() {
        var form = document.querySelector('[data-upload-form]');
        var importForm = document.querySelector('[data-import-form]');
        if (!form && !importForm) return;
        var input = byId('file');
        var info = byId('file-info');
        var list = byId('file-list');
        if (input) {
            input.addEventListener('change', function () {
                list.textContent = '';
                Array.prototype.slice.call(input.files).forEach(function (file) {
                    var item = document.createElement('li');
                    item.textContent = file.name + ' · ' + formatSize(file.size);
                    list.appendChild(item);
                });
                info.classList.toggle('hidden', !input.files.length);
            });
            var dropzone = byId('dropzone');
            ['dragenter', 'dragover'].forEach(function (name) {
                dropzone.addEventListener(name, function (event) {
                    event.preventDefault();
                    dropzone.classList.add('dragover');
                });
            });
            ['dragleave', 'drop'].forEach(function (name) {
                dropzone.addEventListener(name, function () { dropzone.classList.remove('dragover'); });
            });
        }

        function begin(files, sourceForm, isImport) {
            clearClientMessages();
            byId('upload-section').classList.add('hidden');
            byId('progress-section').classList.remove('hidden');
            byId('upload-queue-body').textContent = '';
            var created = 0;
            var failed = 0;
            var sequence = Promise.resolve();
            // Uploads run one at a time, but conversions are tracked in
            // parallel so a long conversion does not hold up the next upload.
            var conversions = [];
            files.forEach(function (file, index) {
                sequence = sequence.then(function () {
                    var row = createQueueRow(file, index);
                    row._uploadCell.textContent = isImport ? 'Requesting import' : 'Uploading 0%';
                    return uploadRequest(sourceForm, isImport ? null : file, files.length === 1, row).then(function (item) {
                        created += 1;
                        row._uploadCell.textContent = 'Uploaded';
                        row._resultCell.textContent = '';
                        addResultLink(row, item.manage_url || '/admin/flipbooks/' + item.id, 'Manage', false);
                        if (item.error) {
                            failed += 1;
                            setQueueError(row, item.error, true);
                            addResultLink(row, item.manage_url || '/admin/flipbooks/' + item.id, 'Review and retry', false);
                            return;
                        }
                        row._conversionCell.textContent = 'Queued';
                        conversions.push(pollPublication(item.status_url || '/admin/flipbooks/' + item.id + '/status', row));
                    }).catch(function (error) {
                        failed += 1;
                        setQueueError(row, error.message, false);
                        row._conversionCell.textContent = 'Not started';
                    });
                });
            });
            sequence.then(function () {
                var progress = byId('upload-progress');
                if (progress) progress.classList.add('hidden');
                return Promise.all(conversions);
            }).then(function () {
                byId('queue-summary').textContent = created + ' publication(s) created; ' + failed + ' file error(s). Review each row for its actual conversion state.';
                if (failed) showClientError(failed + ' file(s) need attention. No failed item is reported as ready.');
                else showClientStatus('All files were uploaded. Review the conversion results above.');
            });
        }

        if (form) {
            form.addEventListener('submit', function (event) {
                event.preventDefault();
                var files = Array.prototype.slice.call(input.files);
                if (!files.length) {
                    showClientError('Choose at least one document.');
                    return;
                }
                if (files.length > 50) {
                    showClientError('Upload at most 50 documents at a time.');
                    return;
                }
                begin(files, form, false);
            });
        }
        if (importForm) {
            importForm.addEventListener('submit', function (event) {
                event.preventDefault();
                var url = byId('import-url');
                if (!url.value.trim()) {
                    showClientError('Enter a Google Slides URL.');
                    return;
                }
                begin([{ name: 'Google Slides import', size: 0 }], importForm, true);
            });
        }
    }

    function initDetailPolling() {
        var container = document.querySelector('[data-poll-container]');
        if (!container) return;
        var renderedStatus = container.getAttribute('data-current-status');
        var initiallyActive = renderedStatus !== 'ready' || container.querySelector('.badge-job-queued, .badge-job-running, .badge-job-retry, .badge-pending, .badge-converting');
        if (!initiallyActive) return;
        var sawActiveJob = false;
        var heading = container.querySelector('[data-status-heading]');
        var message = container.querySelector('[data-status-message]');
        var network = container.querySelector('[data-poll-network]');
        var deadline = Date.now() + pollingTimeout;
        var failures = 0;
        function check() {
            if (Date.now() > deadline) {
                if (network) network.textContent = 'Automatic status refresh stopped after 15 minutes. Reload to check the current state.';
                return;
            }
            fetch(container.getAttribute('data-poll-url'), { headers: { 'Accept': 'application/json' }, credentials: 'same-origin' }).then(function (response) {
                if (!response.ok) throw new Error('HTTP ' + response.status);
                return response.json();
            }).then(function (data) {
                failures = 0;
                if (network) network.textContent = '';
                if (heading) heading.textContent = 'Conversion status: ' + data.status;
                if (message && data.status !== 'ready') message.textContent = data.error || 'The worker is still processing this publication.';
                var job = activeJob(data.jobs);
                if (job) sawActiveJob = true;
                if (!job && (data.status === 'ready' || data.status === 'error')) {
                    // Reload only for a result this page has not shown yet. A publication
                    // that was already in this state would otherwise reload forever.
                    if (data.status === renderedStatus && !sawActiveJob) return;
                    if (message && data.status === 'ready') message.textContent = data.page_count + ' pages converted.';
                    window.setTimeout(function () { window.location.reload(); }, 600);
                    return;
                }
                if (job && message) message.textContent = (job.progress_message || data.status) + (job.progress ? ' · ' + job.progress + '%' : '');
                window.setTimeout(check, pollingInterval);
            }).catch(function () {
                failures += 1;
                if (network) network.textContent = 'Status refresh failed (' + failures + '/5). The worker may still be running.';
                if (failures >= 5) {
                    if (network) network.textContent = 'Automatic status refresh stopped after repeated network failures. Reload to check the current state.';
                    return;
                }
                window.setTimeout(check, pollingInterval * failures);
            });
        }
        check();
    }

    function escapeAttribute(value) {
        return String(value).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    }

    function initEmbedConfigurator() {
        var form = document.querySelector('[data-embed-config]');
        if (!form) return;
        var output = document.querySelector('[data-embed-output]');
        var preview = document.querySelector('[data-embed-preview]');
        var previewTimer = null;
        function values() {
            var width = Math.max(200, Math.min(4000, Number(byId('embed-width').value) || 800));
            var height = Math.max(200, Math.min(4000, Number(byId('embed-height').value) || 600));
            var aspect = byId('embed-aspect').value;
            if (aspect !== 'custom') {
                var parts = aspect.split(':').map(Number);
                height = Math.round(width * parts[1] / parts[0]);
                byId('embed-height').value = String(height);
            }
            var source = form.getAttribute('data-base-url') + '/embed/' + form.getAttribute('data-slug');
            if (byId('embed-page-enabled').checked) {
                var start = byId('embed-start-page') ? byId('embed-start-page').value : '1';
                source += '?page=' + encodeURIComponent(start);
            }
            return { width: width, height: height, source: source, responsive: byId('embed-responsive').checked };
        }
        function code(config) {
            var style = config.responsive
                ? 'border:none;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,0.1);width:100%;max-width:' + config.width + 'px;aspect-ratio:' + config.width + '/' + config.height + ';height:auto;'
                : 'border:none;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,0.1);';
            return '<iframe src="' + escapeAttribute(config.source) + '" width="' + config.width + '" height="' + config.height + '" frameborder="0" allowfullscreen title="Interactive flipbook" loading="lazy" style="' + style + '"></iframe>';
        }
        function update(withPreview) {
            var config = values();
            output.value = code(config);
            if (withPreview && preview) {
                window.clearTimeout(previewTimer);
                previewTimer = window.setTimeout(function () {
                    preview.textContent = '';
                    var iframe = document.createElement('iframe');
                    iframe.title = 'Embed preview';
                    iframe.src = config.source;
                    iframe.width = String(config.width);
                    iframe.height = String(config.height);
                    iframe.loading = 'lazy';
                    preview.appendChild(iframe);
                }, 500);
            }
        }
        form.addEventListener('submit', function (event) { event.preventDefault(); });
        form.addEventListener('input', function () { update(true); });
        form.addEventListener('change', function () { update(true); });
        update(false);
    }

    function init() {
        initNavigation();
        initTabs();
        initConfirmations();
        initCopyButtons();
        initBulkActions();
        initUploadQueue();
        initReplaceUpload();
        initDetailPolling();
        initEmbedConfigurator();
    }

    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
    else init();
}());
