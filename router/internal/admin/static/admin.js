/* ─── Theme ─────────────────────────────────────────────────── */

function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  var btn = document.getElementById('theme-toggle');
  if (btn) {
    btn.textContent = theme === 'dark' ? '☀' : '☾'; // ☀ / ☾
    btn.setAttribute('aria-label', theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode');
  }
}

function toggleTheme() {
  var current = document.documentElement.getAttribute('data-theme') || 'dark';
  var next = current === 'dark' ? 'light' : 'dark';
  try { localStorage.setItem('llmesh-theme', next); } catch(e) {}
  applyTheme(next);
}

// Sync icon after DOM is ready (theme already applied by inline <head> script)
(function() {
  var theme = document.documentElement.getAttribute('data-theme') || 'dark';
  var btn = document.getElementById('theme-toggle');
  if (btn) {
    btn.textContent = theme === 'dark' ? '☀' : '☾';
    btn.setAttribute('aria-label', theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode');
  }
})();

/* ─── Mobile nav ────────────────────────────────────────────── */

function toggleNav() {
  var links = document.getElementById('nav-links');
  if (links) links.classList.toggle('open');
}

/* ─── Collapsible sections ──────────────────────────────────── */

function toggleSection(id) {
  var body = document.getElementById(id);
  var header = document.querySelector('[data-toggle="' + id + '"]');
  if (!body) return;
  var open = body.classList.toggle('open');
  if (header) {
    var arrow = header.querySelector('.toggle-arrow');
    if (arrow) arrow.textContent = open ? '▴' : '▾'; // ▴ / ▾
  }
}

/* ─── Tabs (unified) ────────────────────────────────────────────

   One declarative system drives every panel-based tab (settings,
   download OS tabs). Markup:

     <div class="tab-container">
       <div class="tab-bar [underline]">
         <button class="tab-btn" data-tab-target="panel-id"
                 [data-tab-hash] [data-tab-store="group"]
                 [data-tab-onactivate="logs"]>…</button>
       </div>
       <div id="panel-id" class="tab-panel">…</div>
     </div>

   data-tab-hash    → reflect the active panel id in the URL hash
   data-tab-store   → remember the selection in localStorage under this key
   data-tab-onactivate="logs" → start the log poller when shown
*/

function activateTab(btn) {
  var targetId = btn.getAttribute('data-tab-target');
  var panel = document.getElementById(targetId);
  var container = btn.closest('.tab-container') || (panel && panel.closest('.tab-container'));
  if (container) {
    container.querySelectorAll('.tab-panel').forEach(function(p) { p.classList.remove('active'); });
    container.querySelectorAll('.tab-btn').forEach(function(b) { b.classList.remove('active'); });
  }
  if (panel) panel.classList.add('active');
  btn.classList.add('active');

  if (btn.hasAttribute('data-tab-hash')) {
    try { history.replaceState(null, '', '#' + targetId); } catch(e) {}
  }
  var store = btn.getAttribute('data-tab-store');
  if (store) {
    try { localStorage.setItem('llmesh-tab-' + store, targetId); } catch(e) {}
  }
  if (btn.getAttribute('data-tab-onactivate') === 'logs') ensureLogsLoaded();
}

/* ─── Docs nav (help page sidebar) ──────────────────────────── */

function showDoc(id, el) {
  document.querySelectorAll('.docs-section').forEach(function(s) { s.classList.remove('active'); });
  document.querySelectorAll('.docs-link').forEach(function(a) { a.classList.remove('active'); });
  var section = document.getElementById(id);
  if (section) section.classList.add('active');
  if (el) el.classList.add('active');
  var items = document.getElementById('docs-nav-items');
  if (items) items.classList.remove('open');
  var chevron = document.getElementById('docs-nav-chevron');
  if (chevron) chevron.textContent = '▾'; // ▾
  try { history.replaceState(null, '', '#' + id); } catch(e) {}
  if (window.__renderDiagrams) window.__renderDiagrams();
}

function toggleDocsNav() {
  var items = document.getElementById('docs-nav-items');
  var chevron = document.getElementById('docs-nav-chevron');
  if (!items) return;
  var open = items.classList.toggle('open');
  if (chevron) chevron.textContent = open ? '▴' : '▾'; // ▴ / ▾
}

/* ─── Polling helper ─────────────────────────────────────────────
   poll(url, intervalMs, onData) fetches JSON on an interval, pausing
   while the browser tab is hidden. `url` may be a string or a function
   returning the current URL. Returns { tick, start, stop }. */

function poll(url, intervalMs, onData) {
  var timer = null;
  function resolveUrl() { return typeof url === 'function' ? url() : url; }
  function tick() {
    fetch(resolveUrl())
      .then(function(r) { if (!r.ok) throw new Error('non-ok'); return r.json(); })
      .then(onData)
      .catch(function() {});
  }
  function start() { if (!timer) { tick(); timer = setInterval(tick, intervalMs); } }
  function stop() { if (timer) { clearInterval(timer); timer = null; } }
  document.addEventListener('visibilitychange', function() {
    if (document.hidden) stop(); else start();
  });
  start();
  return { tick: tick, start: start, stop: stop };
}

/* ─── Log viewer ─────────────────────────────────────────────── */

var _logsPoller = null;

function currentLogCat() {
  var b = document.querySelector('#log-cat-tabs .tab-btn.active');
  return b ? b.getAttribute('data-log-cat') : 'router';
}

function ensureLogsLoaded() {
  if (_logsPoller) { _logsPoller.start(); _logsPoller.tick(); return; }
  _logsPoller = poll(function() {
    return '/portal/api/logs?category=' + encodeURIComponent(currentLogCat()) + '&limit=200';
  }, 5000, renderLogs);
}

function renderLogs(data) {
  var container = document.getElementById('logs-container');
  if (!container) return;
  var wasAtBottom = container.scrollHeight - container.scrollTop <= container.clientHeight + 2;
  container.innerHTML = '';
  if (!data.entries || data.entries.length === 0) {
    var empty = document.createElement('div');
    empty.className = 'logs-empty';
    empty.textContent = 'No log entries yet.';
    container.appendChild(empty);
  } else {
    data.entries.forEach(function(e) {
      var row = document.createElement('div');
      row.className = 'log-row';

      var tEl = document.createElement('span');
      tEl.className = 'log-time';
      try { tEl.textContent = new Date(e.time).toLocaleTimeString(); }
      catch(_) { tEl.textContent = e.time || ''; }
      row.appendChild(tEl);

      var lvEl = document.createElement('span');
      lvEl.className = 'log-level ' + (e.level || '');
      lvEl.textContent = e.level || '';
      row.appendChild(lvEl);

      var msgEl = document.createElement('span');
      msgEl.className = 'log-msg';
      msgEl.textContent = e.msg || '';
      row.appendChild(msgEl);

      if (e.attrs && Object.keys(e.attrs).length > 0) {
        var attrEl = document.createElement('span');
        attrEl.className = 'log-attrs';
        var pairs = [];
        for (var k in e.attrs) {
          if (Object.prototype.hasOwnProperty.call(e.attrs, k)) pairs.push(k + '=' + e.attrs[k]);
        }
        attrEl.textContent = pairs.join(' ');
        attrEl.title = pairs.join('\n');
        row.appendChild(attrEl);
      }
      container.appendChild(row);
    });
    if (wasAtBottom) container.scrollTop = container.scrollHeight;
  }
  var lu = document.getElementById('logs-last-updated');
  if (lu) lu.textContent = 'updated ' + new Date().toLocaleTimeString();
}

/* ─── Clients filter + pagination ──────────────────────────── */

function initClientGroups() {
  var container = document.getElementById('groups-container');
  if (!container) return;

  var filterInput  = document.getElementById('client-filter');
  var summary      = document.getElementById('filter-summary');
  var prevBtn      = document.getElementById('prev-page');
  var nextBtn      = document.getElementById('next-page');
  var pageInfo     = document.getElementById('page-info');
  var paginationEl = document.getElementById('pagination-controls');
  var PER_PAGE     = 10;

  var allGroups = Array.from(container.querySelectorAll('.user-group'));
  var filtered  = allGroups.slice();
  var currentPage = 0;

  function applyFilter() {
    var q = filterInput ? filterInput.value.trim().toLowerCase() : '';
    filtered = q
      ? allGroups.filter(function(g) {
          var u = (g.getAttribute('data-username') || '').toLowerCase();
          var c = (g.getAttribute('data-clients')  || '').toLowerCase();
          return u.indexOf(q) !== -1 || c.indexOf(q) !== -1;
        })
      : allGroups.slice();
    currentPage = 0;
    render();
  }

  function render() {
    var start      = currentPage * PER_PAGE;
    var end        = start + PER_PAGE;
    var pageGroups = new Set(filtered.slice(start, end));
    var total      = filtered.length;
    var totalPages = Math.max(1, Math.ceil(total / PER_PAGE));

    allGroups.forEach(function(g) {
      g.style.display = pageGroups.has(g) ? '' : 'none';
    });

    if (pageInfo) {
      var pageStr = 'Page ' + (currentPage + 1) + ' of ' + totalPages;
      pageInfo.textContent = total < allGroups.length
        ? total + ' user' + (total !== 1 ? 's' : '') + ' matched · ' + pageStr
        : pageStr;
    }
    if (summary) {
      summary.textContent = (filterInput && filterInput.value.trim() && total < allGroups.length)
        ? total + ' of ' + allGroups.length + ' users'
        : '';
    }
    if (prevBtn) prevBtn.disabled = currentPage === 0;
    if (nextBtn) nextBtn.disabled = currentPage >= totalPages - 1;
    if (paginationEl) paginationEl.style.display = totalPages > 1 ? '' : 'none';
  }

  if (filterInput) filterInput.addEventListener('input', applyFilter);

  window.changeClientPage = function(delta) {
    var totalPages = Math.max(1, Math.ceil(filtered.length / PER_PAGE));
    currentPage = Math.max(0, Math.min(totalPages - 1, currentPage + delta));
    render();
    window.scrollTo({ top: 0, behavior: 'smooth' });
  };

  render();
}

/* ─── Dashboard polling ─────────────────────────────────────── */

function initDashboard() {
  var tbody = document.getElementById('client-tbody');
  if (!tbody) return;

  function buildRow(c) {
    var tr = document.createElement('tr');

    var tdName = document.createElement('td');
    tdName.setAttribute('data-label', 'Client');
    tdName.textContent = c.name || '';
    tr.appendChild(tdName);

    // Status class + label come straight from the server (single source of truth).
    var tdStatus = document.createElement('td');
    tdStatus.setAttribute('data-label', 'Status');
    var span = document.createElement('span');
    span.className = 'badge ' + (c.status_class || '');
    span.textContent = c.status_label || c.status || '';
    tdStatus.appendChild(span);
    tr.appendChild(tdStatus);

    var tdLast = document.createElement('td');
    tdLast.className = 'muted';
    tdLast.setAttribute('data-label', 'Last seen');
    tdLast.textContent = c.last_seen || '—';
    tr.appendChild(tdLast);

    var tdModels = document.createElement('td');
    tdModels.className = 'muted';
    tdModels.setAttribute('data-label', 'Models');
    tdModels.textContent = c.models || '—';
    tr.appendChild(tdModels);

    var tdVersion = document.createElement('td');
    tdVersion.className = 'muted';
    tdVersion.setAttribute('data-label', 'Version');
    tdVersion.textContent = c.version || '—';
    tr.appendChild(tdVersion);

    return tr;
  }

  function emptyRow() {
    var tr = document.createElement('tr');
    tr.className = 'empty-row';
    var td = document.createElement('td');
    td.colSpan = 5;
    td.textContent = 'No client tokens registered.';
    tr.appendChild(td);
    return tr;
  }

  function buildStatsRow(row, mono) {
    var tr = document.createElement('tr');
    var td0 = document.createElement('td');
    if (mono) td0.className = 'stats-mono';
    td0.textContent = row.name;
    var td1 = document.createElement('td'); td1.className = 'muted'; td1.textContent = row.requests;
    var td2 = document.createElement('td'); td2.className = 'muted'; td2.textContent = row.prompt_tokens;
    var td3 = document.createElement('td'); td3.className = 'muted'; td3.textContent = row.completion_tokens;
    tr.appendChild(td0); tr.appendChild(td1); tr.appendChild(td2); tr.appendChild(td3);
    return tr;
  }

  function emptyStatsRow() {
    var tr = document.createElement('tr');
    tr.className = 'empty-row';
    var td = document.createElement('td');
    td.colSpan = 4;
    td.textContent = 'No data yet.';
    tr.appendChild(td);
    return tr;
  }

  function refreshStats(d) {
    var modelTbody = document.getElementById('stats-by-model');
    if (modelTbody) {
      modelTbody.innerHTML = '';
      if (d.stats_by_model && d.stats_by_model.length) {
        d.stats_by_model.forEach(function(r) { modelTbody.appendChild(buildStatsRow(r, true)); });
      } else {
        modelTbody.appendChild(emptyStatsRow());
      }
    }
    var userTbody = document.getElementById('stats-by-user');
    if (userTbody) {
      userTbody.innerHTML = '';
      if (d.stats_by_user && d.stats_by_user.length) {
        d.stats_by_user.forEach(function(r) { userTbody.appendChild(buildStatsRow(r, false)); });
      } else {
        userTbody.appendChild(emptyStatsRow());
      }
    }
  }

  function onData(d) {
    var el;
    el = document.getElementById('req-count');      if (el) el.textContent = d.total_requests;
    el = document.getElementById('active-clients'); if (el) el.textContent = d.active_clients;
    el = document.getElementById('api-key-count');  if (el) el.textContent = d.api_key_count;
    el = document.getElementById('token-count');    if (el) el.textContent = d.token_count;

    tbody.innerHTML = '';
    if (d.clients && d.clients.length) {
      d.clients.forEach(function(c) { tbody.appendChild(buildRow(c)); });
    } else {
      tbody.appendChild(emptyRow());
    }

    refreshStats(d);

    var lu = document.getElementById('last-updated');
    if (lu) lu.textContent = 'Updated ' + new Date().toLocaleTimeString();
  }

  poll('/portal/api/dashboard', 10000, onData);
}

/* ─── Live job stats polling ─────────────────────────────────── */

function initJobStats() {
  if (!document.querySelector('[data-job-id]')) return;

  function buildStats(j, liveEl) {
    var parts = [];
    if (j.ttft_ms > 0) {
      parts.push('ttft ' + (j.ttft_ms / 1000).toFixed(1) + 's');
    }
    if (j.delta_count > 0) {
      var tok = j.delta_count + ' tok';
      if (j.first_chunk_at) {
        var elapsedSec = (Date.now() - new Date(j.first_chunk_at).getTime()) / 1000;
        if (elapsedSec > 2) tok += ' · ' + Math.round(j.delta_count / elapsedSec) + ' t/s';
      }
      parts.push(tok);
    } else if (liveEl) {
      var w = liveEl.getAttribute('data-words');
      if (w && parseInt(w) > 0) parts.push(w + 'w in');
    }
    return parts.length ? ' · ' + parts.join(' · ') : '';
  }

  poll('/portal/api/jobs', 2000, function(jobs) {
    if (!jobs) return;
    jobs.forEach(function(j) {
      var row = document.querySelector('[data-job-id="' + j.id + '"]');
      if (!row) return;
      row.classList.toggle('job-processing', j.phase === 'processing');
      row.classList.toggle('job-generating',  j.phase === 'generating');
      var liveEl = row.querySelector('.job-stats-live');
      if (liveEl) liveEl.textContent = buildStats(j, liveEl);
    });
  });
}

/* ─── Click delegation ──────────────────────────────────────── */

document.addEventListener('click', function(e) {
  // Tabs: any button with data-tab-target
  var tabBtn = e.target.closest('[data-tab-target]');
  if (tabBtn) { activateTab(tabBtn); return; }

  // Log category selector
  var logCatBtn = e.target.closest('[data-log-cat]');
  if (logCatBtn) {
    document.querySelectorAll('#log-cat-tabs .tab-btn').forEach(function(t) { t.classList.remove('active'); });
    logCatBtn.classList.add('active');
    if (_logsPoller) _logsPoller.tick();
    return;
  }

  // Toggle visibility of a target element via data-toggle-target (e.g. reveal a form)
  var toggleBtn = e.target.closest('[data-toggle-target]');
  if (toggleBtn) {
    var tgt = document.getElementById(toggleBtn.getAttribute('data-toggle-target'));
    if (tgt) tgt.classList.toggle('hidden');
    return;
  }

  // Close mobile nav when clicking outside
  var navLinks = document.getElementById('nav-links');
  var navToggle = document.getElementById('nav-toggle');
  if (navLinks && navLinks.classList.contains('open')) {
    if (navToggle && !navLinks.contains(e.target) && !navToggle.contains(e.target)) {
      navLinks.classList.remove('open');
    }
  }

  // Copy to clipboard via data-copy (literal text), data-copy-from (element
  // text/value), or data-copy-code (code block text).
  var copyBtn = e.target.closest('[data-copy]');
  if (copyBtn) { copyText(copyBtn.getAttribute('data-copy'), copyBtn); return; }

  var copyFromBtn = e.target.closest('[data-copy-from]');
  if (copyFromBtn) {
    var src = document.getElementById(copyFromBtn.getAttribute('data-copy-from'));
    if (src) copyText(src.textContent || src.value || '', copyFromBtn);
    return;
  }

  var codeBtn = e.target.closest('[data-copy-code]');
  if (codeBtn) {
    var codeEl = document.getElementById(codeBtn.getAttribute('data-copy-code'));
    if (codeEl) copyText(codeEl.textContent || '', codeBtn);
  }
});

function copyText(text, btn) {
  if (navigator.clipboard) {
    navigator.clipboard.writeText(text).catch(function() { fallbackCopy(text); });
  } else {
    fallbackCopy(text);
  }
  copyFeedback(btn);
}

function copyFeedback(btn) {
  var orig = btn.textContent;
  if (orig === '✓') return; // already showing ✓
  btn.textContent = '✓';
  setTimeout(function() { btn.textContent = orig; }, 1200);
}

function fallbackCopy(text) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0;';
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  try { document.execCommand('copy'); } catch(e) {}
  document.body.removeChild(ta);
}

/* ─── Init ──────────────────────────────────────────────────── */

document.addEventListener('DOMContentLoaded', function() {
  initDashboard();
  initClientGroups();
  initJobStats();

  // Restore OS download tab selection from localStorage.
  document.querySelectorAll('.tab-btn[data-tab-store]').forEach(function(btn) {
    var store = btn.getAttribute('data-tab-store');
    try {
      var saved = localStorage.getItem('llmesh-tab-' + store);
      if (saved && btn.getAttribute('data-tab-target') === saved) activateTab(btn);
    } catch(e) {}
  });

  // Restore settings tab or docs section from URL hash.
  var hash = window.location.hash.slice(1);
  if (hash) {
    var hashSection = document.getElementById(hash);
    if (hashSection && hashSection.classList.contains('tab-panel')) {
      var tabBtn = document.querySelector('.tab-btn[data-tab-target="' + hash + '"]');
      if (tabBtn) activateTab(tabBtn);
    } else if (hashSection && hashSection.classList.contains('docs-section')) {
      var hashLink = document.querySelector('.docs-link[onclick*="\'' + hash + '\'"]');
      showDoc(hash, hashLink);
    }
  }

  // Auto-inject copy buttons into all .docs-code blocks.
  document.querySelectorAll('.docs-code').forEach(function(el, i) {
    var id = 'code-block-' + i;
    el.id = id;
    var wrap = document.createElement('div');
    wrap.className = 'docs-code-wrap';
    el.parentNode.insertBefore(wrap, el);
    wrap.appendChild(el);
    var btn = document.createElement('button');
    btn.className = 'btn-code-copy';
    btn.setAttribute('data-copy-code', id);
    btn.textContent = '⎘'; // ⎘
    btn.title = 'Copy';
    wrap.appendChild(btn);
  });
});

/* ─── Elapsed time display ───────────────────────────────────── */

function formatElapsed(isoString) {
  var ms = Date.now() - new Date(isoString).getTime();
  if (ms < 1000) return '< 1s';
  var s = Math.floor(ms / 1000);
  var m = Math.floor(s / 60);
  s = s % 60;
  if (m > 0) return m + 'm ' + (s < 10 ? '0' : '') + s + 's';
  return s + 's';
}

function updateElapsed() {
  document.querySelectorAll('[data-since]').forEach(function(el) {
    el.textContent = formatElapsed(el.getAttribute('data-since'));
  });
}

(function() {
  if (!document.querySelector('[data-since]')) return;
  updateElapsed();
  setInterval(updateElapsed, 1000);
})();
