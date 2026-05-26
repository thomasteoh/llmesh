/* ─── CSRF helpers ──────────────────────────────────────────── */

function getCSRFToken() {
  var input = document.querySelector('input[name="_csrf"]');
  return input ? input.value : '';
}

/* ─── Confirm helper ────────────────────────────────────────── */

function confirmAction(msg) {
  return window.confirm(msg);
}

/* ─── Theme ─────────────────────────────────────────────────── */

function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  var btn = document.getElementById('theme-toggle');
  if (btn) {
    btn.textContent = theme === 'dark' ? '\u2600' : '\u263e'; // ☀ / ☾
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
    btn.textContent = theme === 'dark' ? '\u2600' : '\u263e';
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
    if (arrow) arrow.textContent = open ? '\u25b4' : '\u25be'; // ▴ / ▾
  }
}

/* ─── Docs nav ──────────────────────────────────────────────── */

function showDoc(id, el) {
  document.querySelectorAll('.docs-section').forEach(function(s) {
    s.classList.remove('active');
  });
  document.querySelectorAll('.docs-link').forEach(function(a) {
    a.classList.remove('active');
  });
  var section = document.getElementById(id);
  if (section) section.classList.add('active');
  if (el) el.classList.add('active');
  // Close mobile docs nav after selection
  var items = document.getElementById('docs-nav-items');
  if (items) items.classList.remove('open');
  var chevron = document.getElementById('docs-nav-chevron');
  if (chevron) chevron.textContent = '\u25be'; // ▾
  // Persist section in URL hash
  try { history.replaceState(null, '', '#' + id); } catch(e) {}
  // Render mermaid diagrams in newly visible section
  if (window.__renderDiagrams) window.__renderDiagrams();
}

// showTab activates panelId and marks btn active within the nearest .tab-container.
function showTab(panelId, btn) {
  var panel = document.getElementById(panelId);
  if (!panel) return;
  var container = btn ? btn.closest('.tab-container') : panel.closest('.tab-container');
  if (container) {
    container.querySelectorAll('.tab-panel').forEach(function(p) { p.classList.remove('active'); });
    container.querySelectorAll('.tab-btn').forEach(function(b) { b.classList.remove('active'); });
  }
  panel.classList.add('active');
  if (btn) btn.classList.add('active');
}

function showOsTab(groupId, tabId) {
  var group = document.getElementById(groupId);
  if (!group) return;
  var tab = group.querySelector('[data-tab="' + tabId + '"]');
  showTab(tabId, tab);
  try { localStorage.setItem('llmesh-os-tab-' + groupId, tabId); } catch(e) {}
}

function toggleDocsNav() {
  var items = document.getElementById('docs-nav-items');
  var chevron = document.getElementById('docs-nav-chevron');
  if (!items) return;
  var open = items.classList.toggle('open');
  if (chevron) chevron.textContent = open ? '\u25b4' : '\u25be'; // ▴ / ▾
}

/* ─── Settings tabs ─────────────────────────────────────────── */

function showSettingsTab(id, btn) {
  showTab(id, btn);
  try { history.replaceState(null, '', '#' + id); } catch(e) {}
  if (id === 'tab-logs') ensureLogsLoaded();
}

/* ─── Log viewer ─────────────────────────────────────────────── */

var _logCurrentCat = 'router';
var _logInterval = null;

function showLogCat(cat, btn) {
  _logCurrentCat = cat;
  document.querySelectorAll('#log-cat-tabs .tab-btn').forEach(function(t) {
    t.classList.remove('active');
  });
  if (btn) btn.classList.add('active');
  fetchLogs();
}

function ensureLogsLoaded() {
  fetchLogs();
  if (!_logInterval) {
    _logInterval = setInterval(fetchLogs, 5000);
    // Pause polling when the browser tab goes to background; resume on return.
    document.addEventListener('visibilitychange', function() {
      if (document.hidden) {
        clearInterval(_logInterval);
        _logInterval = null;
      } else if (document.getElementById('tab-logs') &&
                 document.getElementById('tab-logs').classList.contains('active')) {
        fetchLogs();
        _logInterval = setInterval(fetchLogs, 5000);
      }
    });
  }
}

function fetchLogs() {
  var container = document.getElementById('logs-container');
  if (!container) return;
  fetch('/portal/api/logs?category=' + encodeURIComponent(_logCurrentCat) + '&limit=200')
    .then(function(r) {
      if (!r.ok) throw new Error('non-ok');
      return r.json();
    })
    .then(function(data) {
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
          try {
            var d = new Date(e.time);
            tEl.textContent = d.toLocaleTimeString();
          } catch(_) { tEl.textContent = e.time || ''; }
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
              if (Object.prototype.hasOwnProperty.call(e.attrs, k)) {
                pairs.push(k + '=' + e.attrs[k]);
              }
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
    })
    .catch(function() {});
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
        ? total + ' user' + (total !== 1 ? 's' : '') + ' matched \u00b7 ' + pageStr
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

    var cls = c.status === 'connected' ? 'connected'
            : c.status === 'offline'   ? 'offline'
            : 'never_connected';
    var lbl = c.status === 'connected'     ? '\u25cf connected'
            : c.status === 'offline'       ? '\u25cb offline'
            : '\u25cb never connected';
    var tdStatus = document.createElement('td');
    tdStatus.setAttribute('data-label', 'Status');
    var span = document.createElement('span');
    span.className = 'badge ' + cls;
    span.textContent = lbl;
    tdStatus.appendChild(span);
    tr.appendChild(tdStatus);

    var tdLast = document.createElement('td');
    tdLast.className = 'muted';
    tdLast.setAttribute('data-label', 'Last seen');
    tdLast.textContent = c.last_seen || '\u2014';
    tr.appendChild(tdLast);

    var tdModels = document.createElement('td');
    tdModels.className = 'muted';
    tdModels.setAttribute('data-label', 'Models');
    tdModels.textContent = c.models || '\u2014';
    tr.appendChild(tdModels);

    var tdVersion = document.createElement('td');
    tdVersion.className = 'muted';
    tdVersion.setAttribute('data-label', 'Version');
    tdVersion.textContent = c.version || '\u2014';
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

  function refresh() {
    fetch('/portal/api/dashboard').then(function(r) {
      if (!r.ok) throw new Error('non-ok');
      return r.json();
    }).then(function(d) {
      var el;
      el = document.getElementById('req-count');      if (el) el.textContent = d.total_requests;
      el = document.getElementById('active-clients'); if (el) el.textContent = d.active_clients;
      el = document.getElementById('api-key-count'); if (el) el.textContent = d.api_key_count;
      el = document.getElementById('token-count');   if (el) el.textContent = d.token_count;

      tbody.innerHTML = '';
      if (d.clients && d.clients.length) {
        d.clients.forEach(function(c) { tbody.appendChild(buildRow(c)); });
      } else {
        tbody.appendChild(emptyRow());
      }

      refreshStats(d);

      var lu = document.getElementById('last-updated');
      if (lu) lu.textContent = 'Updated ' + new Date().toLocaleTimeString();
    }).catch(function() {});
  }

  setInterval(refresh, 10000);
}

/* ─── Click delegation ──────────────────────────────────────── */

document.addEventListener('click', function(e) {
  // Close mobile nav when clicking outside
  var navLinks = document.getElementById('nav-links');
  var navToggle = document.getElementById('nav-toggle');
  if (navLinks && navLinks.classList.contains('open')) {
    if (navToggle && !navLinks.contains(e.target) && !navToggle.contains(e.target)) {
      navLinks.classList.remove('open');
    }
  }

  // Copy to clipboard via data-copy attribute
  var copyBtn = e.target.closest('[data-copy]');
  if (copyBtn) {
    var text = copyBtn.getAttribute('data-copy');
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).catch(function() { fallbackCopy(text); });
    } else {
      fallbackCopy(text);
    }
    copyFeedback(copyBtn);
    return;
  }

  // Copy from element via data-copy-from attribute
  var copyFromBtn = e.target.closest('[data-copy-from]');
  if (copyFromBtn) {
    var src = document.getElementById(copyFromBtn.getAttribute('data-copy-from'));
    if (src) {
      var val = src.textContent || src.value || '';
      if (navigator.clipboard) {
        navigator.clipboard.writeText(val).catch(function() { fallbackCopy(val); });
      } else {
        fallbackCopy(val);
      }
      copyFeedback(copyFromBtn);
    }
    return;
  }

  // Copy code block via data-copy-code attribute
  var codeBtn = e.target.closest('[data-copy-code]');
  if (codeBtn) {
    var codeEl = document.getElementById(codeBtn.getAttribute('data-copy-code'));
    if (codeEl) {
      var codeText = codeEl.textContent || '';
      if (navigator.clipboard) {
        navigator.clipboard.writeText(codeText).catch(function() { fallbackCopy(codeText); });
      } else {
        fallbackCopy(codeText);
      }
      copyFeedback(codeBtn);
    }
  }
});

function copyFeedback(btn) {
  var orig = btn.textContent;
  if (orig === '\u2713') return; // already showing ✓
  btn.textContent = '\u2713';
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


/* ─── Live job stats polling ─────────────────────────────────── */

function initJobStats() {
  if (!document.querySelector('[data-job-id]')) return;

  function buildStats(j) {
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
    }
    var wordsEl = document.querySelector('[data-job-id="' + j.id + '"] .job-stats-live');
    if (wordsEl) {
      var w = wordsEl.getAttribute('data-words');
      if (w && parseInt(w) > 0 && j.delta_count === 0) parts.push(w + 'w in');
    }
    return parts.length ? ' · ' + parts.join(' · ') : '';
  }

  function refresh() {
    fetch('/portal/api/jobs').then(function(r) {
      if (!r.ok) return null;
      return r.json();
    }).then(function(jobs) {
      if (!jobs) return;
      jobs.forEach(function(j) {
        var row = document.querySelector('[data-job-id="' + j.id + '"]');
        if (!row) return;

        // Update phase class and dot
        row.classList.toggle('job-processing', j.phase === 'processing');
        row.classList.toggle('job-generating',  j.phase === 'generating');

        // Update live stats text
        var liveEl = row.querySelector('.job-stats-live');
        if (liveEl) liveEl.textContent = buildStats(j);
      });
    }).catch(function() {});
  }

  refresh();
  setInterval(refresh, 2000);
}

document.addEventListener('DOMContentLoaded', function() {
  initDashboard();
  initClientGroups();
  initJobStats();

  // Restore settings tab or docs section from URL hash
  var hash = window.location.hash.slice(1);
  if (hash) {
    var hashSection = document.getElementById(hash);
    if (hashSection && hashSection.classList.contains('tab-panel')) {
      var tabBtn = document.querySelector('.tab-btn[onclick*="\'' + hash + '\'"]');
      showSettingsTab(hash, tabBtn);
    } else if (hashSection && hashSection.classList.contains('docs-section')) {
      var hashLink = document.querySelector('.docs-link[onclick*="\'' + hash + '\'"]');
      showDoc(hash, hashLink);
    }
  }

  // Restore OS tab selection from localStorage
  document.querySelectorAll('[id]').forEach(function(group) {
    if (!group.id.endsWith('-tab-group')) return;
    try {
      var saved = localStorage.getItem('llmesh-os-tab-' + group.id);
      if (saved && document.getElementById(saved)) showOsTab(group.id, saved);
    } catch(e) {}
  });

  // Auto-inject copy buttons into all .docs-code blocks
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
    btn.textContent = '\u2398'; // ⎘
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
