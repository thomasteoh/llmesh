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

var _pollers = [];

function poll(url, intervalMs, onData) {
  var timer = null, busy = false, disposed = false;
  function resolveUrl() { return typeof url === 'function' ? url() : url; }
  function tick() {
    // One request at a time. setInterval does not wait, so a response slower
    // than the interval would otherwise overlap the next and the two could be
    // applied out of order, reviving rows the later one had already dropped.
    if (busy || disposed) return;
    busy = true;
    fetch(resolveUrl())
      .then(function(r) { if (!r.ok) throw new Error('non-ok'); return r.json(); })
      // A response can land after the content it was written against has been
      // swapped out. Clearing the interval does not cancel a request already in
      // flight, so the callback has to check for itself.
      .then(function(d) { if (!disposed) onData(d); })
      .catch(function() {})
      .then(function() { busy = false; });
  }
  function start() { if (!timer) { tick(); timer = setInterval(tick, intervalMs); } }
  function stop() { if (timer) { clearInterval(timer); timer = null; } }
  function onVisibility() { if (document.hidden) stop(); else start(); }
  document.addEventListener('visibilitychange', onVisibility);
  // dispose is stop plus releasing the visibility listener, so a poller
  // belonging to replaced content does not linger and restart itself the next
  // time the tab is focused.
  var handle = {
    tick: tick, start: start, stop: stop,
    dispose: function() {
      disposed = true;
      stop();
      document.removeEventListener('visibilitychange', onVisibility);
    }
  };
  _pollers.push(handle);
  start();
  return handle;
}

/* stopPollers disposes every poller. Called before content is swapped, since
   the elements the running ones were written against are about to go away. */
function stopPollers() {
  _pollers.forEach(function(p) { p.dispose(); });
  _pollers = [];
  // Cached separately so it survives tab switches; clear it too or the next
  // visit to the logs tab restarts a poller that has been disposed.
  _logsPoller = null;
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

  /* swapCard replaces a card's contents with server-rendered markup.
     Skipped when nothing changed, so the DOM is not rebuilt every poll, and
     when the card holds input the user has started — these cards carry the
     alias editor, and swapping it out mid-word would eat what was typed or
     move the caret out from under them. */
  function swapCard(id, html) {
    var el = document.getElementById(id);
    // An empty string means the server failed to render, which is a reason to
    // leave the last good markup alone. A card with genuinely nothing to show
    // renders as whitespace, and is hidden rather than left stale.
    if (!el || typeof html !== 'string' || html === '') return;
    if (el.innerHTML === html) return;
    if (el.contains(document.activeElement)) return;
    var dirty = Array.prototype.some.call(el.querySelectorAll('input'), function(i) {
      return i.type !== 'hidden' && i.value !== '';
    });
    if (dirty) return;
    el.innerHTML = html;
    var card = document.getElementById(id + '-card');
    if (card) card.hidden = html.trim() === '';
  }

  function onData(d) {
    var el;
    el = document.getElementById('req-count');      if (el) el.textContent = d.total_requests;
    el = document.getElementById('active-clients'); if (el) el.textContent = d.active_clients;
    el = document.getElementById('api-key-count');  if (el) el.textContent = d.api_key_count;
    el = document.getElementById('token-count');    if (el) el.textContent = d.token_count;

    // Which models are being served, and whether each alias target is
    // reachable, change as workers come and go — the same events the client
    // table below is polling for.
    swapCard('active-models', d.active_models_html);
    swapCard('alias-chains', d.alias_chains_html);

    tbody.innerHTML = '';
    if (d.clients && d.clients.length) {
      d.clients.forEach(function(c) { tbody.appendChild(buildRow(c)); });
    } else {
      tbody.appendChild(emptyRow());
    }

    var lu = document.getElementById('last-updated');
    if (lu) lu.textContent = 'Updated ' + new Date().toLocaleTimeString();
  }

  poll('/portal/api/dashboard', 10000, onData);
}

/* ─── Live job stats polling ─────────────────────────────────── */

function initJobStats() {
  // Deliberately not gated on a job being present. Gating on the first render
  // meant a page opened while the fleet was idle never started polling at all,
  // so the first job to arrive was invisible until a manual reload.
  if (!document.querySelector('[data-conn-jobs]')) return;

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

  function knownIds() {
    return Array.from(document.querySelectorAll('[data-job-id]'))
      .map(function(el) { return el.getAttribute('data-job-id'); });
  }

  poll(function() {
    // Telling the server what is already on screen keeps it from re-rendering
    // markup for jobs that have not changed, which is nearly all of them.
    return '/portal/api/jobs?known=' + encodeURIComponent(knownIds().join(','));
  }, 2000, function(data) {
    if (!data || !data.jobs) return;

    var live = Object.create(null);
    data.jobs.forEach(function(j) {
      live[j.id] = true;
      var row = document.querySelector('[data-job-id="' + cssEscape(j.id) + '"]');
      if (!row) {
        // A job that started after this page rendered. The server sends it as
        // markup from the same template the page used, so there is one
        // definition of a job row rather than a second one living here.
        if (!j.html) return;
        // j.conn is the connection's ID, so this document-wide lookup lands on
        // exactly one container. Keyed on the name it would land on whichever
        // same-named machine happened to render first.
        var container = document.querySelector('[data-conn-jobs="' + cssEscape(j.conn) + '"]');
        if (!container) return;
        container.insertAdjacentHTML('beforeend', j.html);
        row = container.lastElementChild;
        if (!row) return;
      }
      row.classList.toggle('job-processing', j.phase === 'processing');
      row.classList.toggle('job-generating',  j.phase === 'generating');
      var liveEl = row.querySelector('.job-stats-live');
      if (liveEl) liveEl.textContent = buildStats(j, liveEl);
    });

    // Drop rows for jobs that have finished. Without this the list only ever
    // grew, and a page left open showed work that ended long ago.
    document.querySelectorAll('[data-job-id]').forEach(function(row) {
      if (!live[row.getAttribute('data-job-id')]) row.remove();
    });

    (data.connections || []).forEach(function(c) {
      var el = document.querySelector('[data-conn-capacity="' + cssEscape(c.conn) + '"]');
      if (el) el.textContent = c.in_flight + ' / ' + c.max_concurrent;
    });

    updateElapsed();
  });
}

/* ─── Live connections (clients page) ────────────────────────────
   Which clients are connected, so one starting or dropping shows without a
   reload. Separate from the jobs poll and slower, because connecting is a rarer
   event than a token arriving, and separate from the page render because that
   also summarises 24 hours of performance per machine — a query per client,
   over a window far too long to be worth re-reading every few seconds. */

function initConnections() {
  var container = document.getElementById('groups-container') ||
                  document.querySelector('[data-token-conns]');
  if (!container && !document.querySelector('[data-token-status]')) return;

  function knownConns() {
    return Array.from(document.querySelectorAll('[data-conn-row]'))
      .map(function(el) { return el.getAttribute('data-conn-row'); });
  }

  poll(function() {
    return '/portal/api/connections?known=' + encodeURIComponent(knownConns().join(','));
  }, 5000, function(data) {
    if (!data || !data.tokens) return;

    // Insert connections that appeared. Markup comes from the same conn-row
    // template the page was built from, so there is one definition of it.
    (data.new || []).forEach(function(c) {
      var conns = document.querySelector('[data-token-conns="' + cssEscape(c.token_hash) + '"]');
      if (!conns || conns.querySelector('[data-conn-row="' + cssEscape(c.id) + '"]')) return;
      conns.insertAdjacentHTML('beforeend', c.html);
    });

    data.tokens.forEach(function(t) {
      var live = Object.create(null);
      t.conns.forEach(function(n) { live[n] = true; });

      var conns = document.querySelector('[data-token-conns="' + cssEscape(t.token_hash) + '"]');
      if (conns) {
        // Drop connections that went away, and the jobs container that trails
        // each one — they are siblings, not nested.
        conns.querySelectorAll('[data-conn-row]').forEach(function(row) {
          if (live[row.getAttribute('data-conn-row')]) return;
          var jobs = conns.querySelector(
            '[data-conn-jobs="' + cssEscape(row.getAttribute('data-conn-row')) + '"]');
          if (jobs) jobs.remove();
          row.remove();
        });
      }

      var status = document.querySelector('[data-token-status="' + cssEscape(t.token_hash) + '"]');
      if (status) {
        var badge = status.querySelector('.badge');
        if (badge) {
          badge.className = 'badge ' + (t.status_class || '');
          badge.textContent = t.status_label || '';
        }
      }
      var seen = document.querySelector('[data-token-lastseen="' + cssEscape(t.token_hash) + '"]');
      if (seen) seen.textContent = t.last_seen || '—';
    });
  });
}

/* cssEscape quotes a value for use inside an attribute selector. Client names
   are user-chosen, so they can contain quotes and backslashes. */
function cssEscape(v) {
  return String(v == null ? '' : v).replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

/* ─── Usage & performance panel (dashboard) ──────────────────────
   Renders one chart as inline SVG (no external chart library) from two
   endpoints. Controls: range (24h/7d/30d/90d), grouping (model/user/key),
   metric.

   Count metrics (tokens/requests) come from /portal/api/usage and stack:
   each series is a slice of that bucket's total. Rate metrics (tok/s, TTFT)
   come from /portal/api/perf and cannot stack — two models each generating
   40 tok/s do not add up to 80 — so they draw as lines instead. Rate series
   also carry nulls for buckets with no traffic, where a line must break
   rather than dip to a zero that never happened. */

/* PERF_METRICS maps a metric id to its axis label. Membership doubles as the
   test for which endpoint and which chart style a metric needs. */
var PERF_METRICS = { gen_tps: 'tok/s', prompt_tps: 'tok/s', ttft: 'ms' };

function isPerfMetric(m) { return Object.prototype.hasOwnProperty.call(PERF_METRICS, m); }

var CHART_COLORS = ['--chart-1','--chart-2','--chart-3','--chart-4','--chart-5',
                    '--chart-6','--chart-7','--chart-8','--chart-9','--chart-10'];

function usageColor(i, name) {
  var v = name === 'other' ? '--chart-other' : CHART_COLORS[i % CHART_COLORS.length];
  return getComputedStyle(document.documentElement).getPropertyValue(v).trim() || '#888';
}

function fmtNum(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(n >= 1e10 ? 0 : 1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + 'k';
  return String(n);
}

/* fmtDur renders a millisecond duration the way the Clients page does, so the
   same figure reads identically in both places. */
function fmtDur(ms) {
  if (!(ms > 0)) return '—';
  if (ms < 1000) return Math.round(ms) + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + ' s';
  return (ms / 60000).toFixed(1) + ' min';
}

/* fmtRate renders a tokens-per-second figure. */
function fmtRate(v) {
  if (!(v > 0)) return '—';
  if (v >= 1000) return (v / 1000).toFixed(1) + 'k tok/s';
  if (v >= 100) return v.toFixed(0) + ' tok/s';
  return v.toFixed(1) + ' tok/s';
}

/* fmtMoney renders integer micro-units of the configured currency. Small amounts
   keep more decimals: a per-bucket cost rounded to two places would read as 0.00
   and look like the chart is broken. Mirrors FormatMoney in Go. */
function fmtMoney(micro) {
  var v = (micro || 0) / 1e6;
  if (!v) return '0.00';
  var av = Math.abs(v);
  if (av >= 1) return v.toFixed(2);
  if (av >= 0.01) return v.toFixed(4);
  return v.toFixed(6);
}

/* fmtMetricValue formats a chart/legend value for whichever metric is showing.
   The metric is passed in rather than read from state so a value can never be
   rendered in the unit of a metric the data on screen is not for. */
function fmtMetricValue(metric, v) {
  if (v === null || v === undefined) return '—';
  if (metric === 'ttft') return fmtDur(v);
  if (isPerfMetric(metric)) return fmtRate(v);
  if (metric === 'cost') return fmtMoney(v);
  return fmtNum(v);
}

function fmtBucket(b, hourly) {
  var d = new Date(hourly ? b : b + 'T00:00:00Z');
  if (hourly) {
    return d.toLocaleString(undefined, { hour: 'numeric' }) +
      (d.getHours() === 0 ? ' · ' + d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) : '');
  }
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' });
}

function initUsage() {
  var svg = document.getElementById('usage-chart');
  if (!svg) return;

  var state = { range: '7d', group: 'model', metric: 'tokens', data: null, perf: null };
  try {
    state.range  = localStorage.getItem('llmesh-usage-range')  || state.range;
    state.group  = localStorage.getItem('llmesh-usage-group')  || state.group;
    state.metric = localStorage.getItem('llmesh-usage-metric') || state.metric;
  } catch (e) {}

  /* chartData returns whichever payload backs the selected metric, or null when it
     has not arrived yet. The two share an envelope shape (range/buckets/series) so
     the renderer only has to branch on how values are read and drawn.

     A perf payload is only usable for the metric it was actually fetched for — its
     `values` are that metric's numbers, in that metric's unit. Two clicks in quick
     succession leave two requests in flight, and if they resolve out of order the
     held payload can be for the previous metric; rendering it would label tok/s as
     ms. The server echoes `metric` back precisely so this is checkable. */
  function chartData() {
    if (!isPerfMetric(state.metric)) return state.data;
    if (!state.perf || state.perf.metric !== state.metric) return null;
    return state.perf;
  }

  /* chartMetric is the metric the data on screen actually represents. Formatting
     reads this rather than state.metric so a value can never be rendered in the
     wrong unit. */
  function chartMetric() {
    var d = chartData();
    return d && d.metric ? d.metric : state.metric;
  }

  function syncButtons() {
    [['usage-range','data-usage-range',state.range],
     ['usage-group','data-usage-group',state.group],
     ['usage-metric','data-usage-metric',state.metric]].forEach(function(cfg) {
      var wrap = document.getElementById(cfg[0]);
      if (!wrap) return;
      wrap.querySelectorAll('.seg-btn').forEach(function(b) {
        b.classList.toggle('active', b.getAttribute(cfg[1]) === cfg[2]);
      });
    });
  }

  /* seriesValues returns one number (or null, for rates with no data) per bucket.
     Rate series arrive pre-computed in `values`, because a bucket's rate has to be
     derived from its summed tokens and summed time server-side and cannot be
     recovered from per-bucket averages. */
  function seriesValues(s) {
    var metric = chartMetric();
    if (isPerfMetric(metric)) return s.values;
    if (metric === 'requests') return s.requests;
    if (metric === 'cost') return s.cost_micro || [];
    return s.prompt_tokens.map(function(p, i) { return p + s.completion_tokens[i]; });
  }

  function render() {
    var d = chartData();
    var empty = document.getElementById('usage-empty');
    if (!d) {
      // No usable data for the selected metric — mid-fetch, or the fetch failed.
      // Clear rather than leave the previous metric's chart on screen under a
      // highlighted button, which would read as this metric's numbers.
      svg.innerHTML = '';
      var legend0 = document.getElementById('usage-legend');
      var totals0 = document.getElementById('usage-totals');
      if (legend0) legend0.innerHTML = '';
      if (totals0) totals0.innerHTML = '';
      if (empty) {
        empty.textContent = 'Loading…';
        empty.classList.remove('hidden');
      }
      return;
    }
    var metric = chartMetric();
    var perf = isPerfMetric(metric);
    var wrap = svg.parentElement;
    var W = Math.max(280, wrap.clientWidth);
    var H = 240;
    var padL = 44, padR = 6, padT = 8, padB = 22;
    svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
    svg.removeAttribute('preserveAspectRatio');
    svg.innerHTML = '';

    var n = d.buckets.length;
    var hourly = d.range === '24h' || d.range === '7d';
    var series = d.series || [];
    // Bars stack, so the axis must reach the tallest total. Lines overlay, so it
    // only needs to reach the single largest value.
    var peaks = [];
    var anyData = false;
    for (var i = 0; i < n; i++) {
      var t = 0;
      series.forEach(function(s) {
        var v = seriesValues(s)[i];
        if (v === null || v === undefined) return;
        anyData = anyData || v > 0;
        if (perf) { t = Math.max(t, v); } else { t += v; }
      });
      peaks.push(t);
    }
    var maxV = Math.max.apply(null, [1].concat(peaks));

    if (empty) {
      empty.textContent = perf
        ? 'No performance data recorded in this period.'
        : 'No usage recorded in this period.';
      empty.classList.toggle('hidden', anyData);
    }

    var plotW = W - padL - padR, plotH = H - padT - padB;
    var ns = 'http://www.w3.org/2000/svg';

    // Horizontal grid lines + y labels at 0 / half / max.
    [0, 0.5, 1].forEach(function(f) {
      var y = padT + plotH * (1 - f);
      var line = document.createElementNS(ns, 'line');
      line.setAttribute('class', 'grid-line');
      line.setAttribute('x1', padL); line.setAttribute('x2', W - padR);
      line.setAttribute('y1', y); line.setAttribute('y2', y);
      svg.appendChild(line);
      var lbl = document.createElementNS(ns, 'text');
      lbl.setAttribute('class', 'axis-label');
      lbl.setAttribute('x', padL - 6); lbl.setAttribute('y', y + 3.5);
      lbl.setAttribute('text-anchor', 'end');
      lbl.textContent = fmtMetricValue(metric, Math.round(maxV * f));
      svg.appendChild(lbl);
    });

    var slot = plotW / n;
    var barW = Math.max(1, Math.min(slot * 0.72, 40));

    // X labels: ~6 evenly spaced.
    var step = Math.max(1, Math.ceil(n / 6));
    for (var bi = 0; bi < n; bi += step) {
      var tx = document.createElementNS(ns, 'text');
      tx.setAttribute('class', 'axis-label');
      tx.setAttribute('x', padL + slot * bi + slot / 2);
      tx.setAttribute('y', H - 7);
      tx.setAttribute('text-anchor', 'middle');
      tx.textContent = fmtBucket(d.buckets[bi], hourly);
      svg.appendChild(tx);
    }

    function yFor(v) { return padT + plotH * (1 - v / maxV); }

    if (perf) {
      // One polyline per series. A null value ends the current run and starts a
      // fresh one after the gap, so idle periods read as absent rather than slow.
      series.forEach(function(s, si) {
        var vals = seriesValues(s) || [];
        var colour = usageColor(si, s.name);
        var run = [];
        function flushRun() {
          if (run.length === 1) {
            // A lone point has no segment to draw; mark it so it stays visible.
            var dot = document.createElementNS(ns, 'circle');
            dot.setAttribute('class', 'perf-point');
            dot.setAttribute('cx', run[0][0]); dot.setAttribute('cy', run[0][1]);
            dot.setAttribute('r', 2);
            dot.setAttribute('fill', colour);
            svg.appendChild(dot);
          } else if (run.length > 1) {
            var pl = document.createElementNS(ns, 'polyline');
            pl.setAttribute('class', 'perf-line');
            pl.setAttribute('points', run.map(function(p) { return p[0] + ',' + p[1]; }).join(' '));
            pl.setAttribute('fill', 'none');
            pl.setAttribute('stroke', colour);
            pl.setAttribute('stroke-width', '2');
            pl.setAttribute('stroke-linejoin', 'round');
            pl.setAttribute('stroke-linecap', 'round');
            svg.appendChild(pl);
          }
          run = [];
        }
        for (var b = 0; b < n; b++) {
          var v = vals[b];
          if (v === null || v === undefined) { flushRun(); continue; }
          run.push([padL + slot * b + slot / 2, yFor(v)]);
        }
        flushRun();
      });
    } else {
      // Stacked bars.
      for (var b = 0; b < n; b++) {
        var yAcc = padT + plotH;
        var x = padL + slot * b + (slot - barW) / 2;
        series.forEach(function(s, si) {
          var v = seriesValues(s)[b];
          if (v <= 0) return;
          var h = (v / maxV) * plotH;
          yAcc -= h;
          var rect = document.createElementNS(ns, 'rect');
          rect.setAttribute('class', 'bar');
          rect.setAttribute('x', x); rect.setAttribute('y', yAcc);
          rect.setAttribute('width', barW); rect.setAttribute('height', Math.max(h, 0.5));
          rect.setAttribute('fill', usageColor(si, s.name));
          svg.appendChild(rect);
        });
      }
    }

    // Transparent hover strips for the tooltip, added last so they sit on top.
    for (var hb = 0; hb < n; hb++) {
      var hover = document.createElementNS(ns, 'rect');
      hover.setAttribute('x', padL + slot * hb); hover.setAttribute('y', padT);
      hover.setAttribute('width', slot); hover.setAttribute('height', plotH);
      hover.setAttribute('fill', 'transparent');
      hover.setAttribute('data-bucket-idx', hb);
      svg.appendChild(hover);
    }

    // Legend + totals.
    var legend = document.getElementById('usage-legend');
    if (legend) {
      legend.innerHTML = '';
      series.forEach(function(s, si) {
        var item = document.createElement('span');
        item.className = 'legend-item';
        var sw = document.createElement('span');
        sw.className = 'legend-swatch';
        sw.style.background = usageColor(si, s.name);
        item.appendChild(sw);
        item.appendChild(document.createTextNode(s.name || '(none)'));
        var val = document.createElement('span');
        val.className = 'legend-val';
        // For rates the legend shows the window-wide figure, not a sum: adding
        // tokens/sec across buckets would be meaningless. Cost does sum, but the
        // two bases are added only here, where the split is already reported in
        // the totals row below.
        val.textContent = perf ? fmtMetricValue(metric, s.average)
          : metric === 'cost' ? fmtMoney((s.actual_cost_micro || 0) + (s.estimated_cost_micro || 0))
          : fmtNum(metric === 'requests' ? s.total_requests : s.total_tokens);
        item.appendChild(val);
        legend.appendChild(item);
      });
    }
    var totals = document.getElementById('usage-totals');
    if (totals) {
      totals.innerHTML = '';
      /* Each entry is [value, label]. */
      var cur = d.currency ? ' ' + d.currency : '';
      var parts;
      if (perf) {
        // Rates are window-wide figures rather than sums of the buckets.
        parts = [[fmtNum(d.totals.samples), 'requests measured'],
                 [fmtRate(d.totals.gen_tokens_per_sec), 'generation'],
                 [fmtRate(d.totals.prompt_tokens_per_sec), 'prompt eval']];
      } else if (metric === 'cost') {
        /* Charged and estimated are never summed into one headline figure. A
           modelled number added to a real invoice produces something that is
           neither, and it is the one mistake this feature exists to prevent. */
        parts = [[fmtMoney(d.totals.actual_cost_micro) + cur, 'charged'],
                 [fmtMoney(d.totals.estimated_cost_micro) + cur, 'estimated']];
      } else {
        parts = [[fmtNum(d.totals.requests), 'Requests'],
                 [fmtNum(d.totals.prompt_tokens), 'Prompt tokens'],
                 [fmtNum(d.totals.completion_tokens), 'Completion tokens']];
      }
      parts.forEach(function(pair) {
        var sp = document.createElement('span');
        var b = document.createElement('b');
        b.textContent = pair[0];
        sp.appendChild(b);
        sp.appendChild(document.createTextNode(' ' + pair[1]));
        totals.appendChild(sp);
      });
      /* Unpriced requests are only surfaced under the cost metric, where they
         are the reason a total may understate. Staying silent about them would
         make an incomplete figure look authoritative. */
      if (metric === 'cost' && d.totals.unpriced_requests > 0) {
        var warn = document.createElement('span');
        warn.className = 'usage-total-warn';
        warn.title = 'These requests ran on models with no rate configured, so they contribute nothing to the figures above. Set rates under Settings → Pricing.';
        var wb = document.createElement('b');
        wb.textContent = fmtNum(d.totals.unpriced_requests);
        warn.appendChild(wb);
        warn.appendChild(document.createTextNode(' requests unpriced'));
        totals.appendChild(warn);
      }
    }
  }

  /* renderPerfTiles fills the summary cards. They always reflect the selected
     range, whichever metric the chart happens to be showing. */
  function renderPerfTiles() {
    var p = state.perf;
    if (!p) return;
    var t = p.totals || {};
    function set(id, text) {
      var el = document.getElementById(id);
      if (el) el.textContent = text;
    }
    set('perf-gen-tps', fmtRate(t.gen_tokens_per_sec));
    set('perf-prompt-tps', fmtRate(t.prompt_tokens_per_sec));
    set('perf-ttft', fmtDur(t.avg_ttft_ms));
    set('perf-queue', fmtDur(t.avg_queue_ms));

    var label = document.getElementById('perf-window-label');
    if (label) {
      label.textContent = t.samples
        ? '· last ' + p.range + ' · ' + fmtNum(t.samples) + ' requests measured'
        : '· last ' + p.range;
    }
    var note = document.getElementById('perf-note');
    if (note) {
      if (!t.samples) {
        note.textContent = 'No completed requests in this period yet.';
      } else {
        var parts = ['Worst TTFT ' + fmtDur(t.max_ttft_ms),
                     'worst end-to-end ' + fmtDur(t.max_total_ms)];
        // Below 100% some samples were timed by the router from the outside
        // (dispatch to first token, first token to done) rather than reported by
        // the backend, which makes those speeds approximate.
        if (t.backend_measured_frac < 0.999) {
          parts.push(Math.round(t.backend_measured_frac * 100) +
                     '% backend-reported (rest approximated by the router)');
        }
        note.textContent = parts.join(' · ') + '.';
      }
    }
  }

  /* Tooltip */
  var tip = document.getElementById('usage-tip');
  svg.addEventListener('mousemove', function(e) {
    var d = chartData();
    if (!tip || !d) return;
    var t = e.target.closest('[data-bucket-idx]');
    if (!t) { tip.style.display = 'none'; return; }
    var b = parseInt(t.getAttribute('data-bucket-idx'), 10);
    var hourly = d.range === '24h' || d.range === '7d';
    tip.innerHTML = '';
    var title = document.createElement('div');
    title.className = 'tip-title';
    title.textContent = fmtBucket(d.buckets[b], hourly);
    tip.appendChild(title);
    var any = false;
    (d.series || []).forEach(function(s, si) {
      var v = seriesValues(s)[b];
      if (v === null || v === undefined || !(v > 0)) return;
      any = true;
      var row = document.createElement('div');
      row.className = 'tip-row';
      var name = document.createElement('span');
      name.className = 'tip-name';
      var sw = document.createElement('span');
      sw.className = 'tip-swatch';
      sw.style.background = usageColor(si, s.name);
      name.appendChild(sw);
      name.appendChild(document.createTextNode(s.name || '(none)'));
      var val = document.createElement('span');
      val.className = 'tip-val';
      val.textContent = fmtMetricValue(chartMetric(), v);
      row.appendChild(name); row.appendChild(val);
      tip.appendChild(row);
    });
    if (!any) {
      var none = document.createElement('div');
      none.className = 'tip-row';
      none.textContent = isPerfMetric(chartMetric()) ? 'no data' : 'no usage';
      tip.appendChild(none);
    }
    tip.style.display = 'block';
    var tw = tip.offsetWidth, th = tip.offsetHeight;
    var xPos = Math.min(e.clientX + 14, window.innerWidth - tw - 8);
    var yPos = Math.min(e.clientY + 14, window.innerHeight - th - 8);
    tip.style.left = xPos + 'px';
    tip.style.top = yPos + 'px';
  });
  svg.addEventListener('mouseleave', function() { if (tip) tip.style.display = 'none'; });

  var usagePoller = poll(function() {
    return '/portal/api/usage?range=' + encodeURIComponent(state.range) +
           '&group=' + encodeURIComponent(state.group);
  }, 60000, function(d) {
    state.data = d;
    if (!isPerfMetric(state.metric)) render();
  });

  // The performance endpoint is polled unconditionally: the summary tiles show
  // regardless of which metric the chart is on. Its `metric` parameter only
  // selects which series the chart half of the payload carries.
  var perfPoller = poll(function() {
    var m = isPerfMetric(state.metric) ? state.metric : 'gen_tps';
    return '/portal/api/perf?range=' + encodeURIComponent(state.range) +
           '&group=' + encodeURIComponent(state.group) +
           '&metric=' + encodeURIComponent(m);
  }, 60000, function(d) {
    state.perf = d;
    renderPerfTiles();
    if (isPerfMetric(state.metric)) render();
  });

  document.addEventListener('click', function(e) {
    var rb = e.target.closest('[data-usage-range]');
    var gb = e.target.closest('[data-usage-group]');
    var mb = e.target.closest('[data-usage-metric]');
    if (!rb && !gb && !mb) return;
    if (rb) state.range = rb.getAttribute('data-usage-range');
    if (gb) state.group = gb.getAttribute('data-usage-group');
    if (mb) state.metric = mb.getAttribute('data-usage-metric');
    try {
      localStorage.setItem('llmesh-usage-range', state.range);
      localStorage.setItem('llmesh-usage-group', state.group);
      localStorage.setItem('llmesh-usage-metric', state.metric);
    } catch (err) {}
    syncButtons();
    // Render first so the chart reflects the new selection at once: switching to a
    // count metric draws immediately from data already held, and switching to a
    // rate metric clears to "Loading…" rather than leaving the previous metric's
    // chart under a highlighted button. Then refetch — a rate series is specific
    // to its metric, so unlike the usage chart it cannot be re-rendered in place.
    render();
    if (rb || gb) {
      usagePoller.tick();
      perfPoller.tick();
    } else if (isPerfMetric(state.metric)) {
      perfPoller.tick();
    }
  });

  var resizeTimer = null;
  window.addEventListener('resize', function() {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(render, 150);
  });

  syncButtons();
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

/* ─── Portal actions ─────────────────────────────────────────────
   Every action in the portal is a real form posting to a real endpoint, and
   still works with scripting off — the server answers those with the redirect
   it always has. What follows upgrades them: the post goes out by fetch, the
   server replies 204 with the destination, and only the page's content region
   is pulled and swapped. The sidebar, stylesheet, and scripts stay put, and so
   does the scroll position, which a reload threw away on every revoke. */

/* swapContent replaces <main> with the same region from url, then re-runs the
   page setup so the new markup gets its handlers and pollers. */
function swapContent(url) {
  return fetch(url, { headers: { 'X-Portal-Fetch': '1' }, credentials: 'same-origin' })
    .then(function(r) {
      if (!r.ok) throw new Error('non-ok');
      return r.text();
    })
    .then(function(html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var next = doc.querySelector('main');
      var current = document.querySelector('main');
      if (!next || !current) throw new Error('no main');
      stopPollers();
      current.innerHTML = next.innerHTML;
      var hash = url.indexOf('#') !== -1 ? url.slice(url.indexOf('#') + 1) : '';
      initPage(hash);
    });
}

/* submitAction posts a form without navigating. Anything unexpected — a
   non-2xx, a network failure, a page that will not swap — falls back to
   submitting the form normally, so a failure shows the server's own error page
   rather than silently doing nothing. */
function submitAction(form) {
  var url = form.getAttribute('action') || window.location.pathname;
  fetch(url, {
    method: 'POST',
    body: new FormData(form),
    headers: { 'X-Portal-Fetch': '1' },
    credentials: 'same-origin',
    redirect: 'follow'
  }).then(function(r) {
    if (!r.ok && r.status !== 204) throw new Error('action failed');
    // Several actions finish somewhere other than where they started, and some
    // carry a #tab the page has to re-select.
    return r.headers.get('X-Portal-Location') || window.location.pathname;
  }, function() {
    // Only this branch means the action did not go through. Re-run it as a
    // plain form post so the browser shows whatever the server has to say,
    // including a login page if the session expired underneath us.
    form.submit();
    return null;
  }).then(function(dest) {
    if (dest === null) return; // already resubmitted
    if (stripHash(dest) !== stripHash(window.location.pathname + window.location.search)) {
      window.location.href = dest;
      return;
    }
    // Past this point the action has already been applied. If refreshing the
    // content fails, reload — never re-post, or a failed refresh turns one
    // "create key" into two.
    return swapContent(dest).catch(function() { window.location.href = dest; });
  });
}

function stripHash(u) {
  var i = u.indexOf('#');
  return i === -1 ? u : u.slice(0, i);
}

/* Delegated so it covers rows added after load. A form with an inline
   onsubmit confirm that returns false never fires a submit event, so the
   confirmation still gates the action exactly as before. */
document.addEventListener('submit', function(e) {
  var form = e.target;
  if (!(form instanceof HTMLFormElement)) return;
  if ((form.getAttribute('method') || '').toUpperCase() !== 'POST') return;
  var action = form.getAttribute('action') || '';
  // Only portal actions. Anything posting elsewhere is left alone.
  if (action && action.charAt(0) !== '/') return;
  if (action && action.indexOf('/portal/') !== 0) return;
  // Logout must actually navigate: the session it is ending is the one the
  // swapped-in content would be fetched with.
  if (action.indexOf('/portal/logout') === 0) return;
  if (form.getAttribute('data-no-enhance') !== null) return;
  e.preventDefault();
  submitAction(form);
});

/* ─── Init ──────────────────────────────────────────────────── */

/* initPage wires up whatever content is currently in <main>. Runs at load and
   again after an action swaps the content in, so newly rendered markup gets the
   same handlers and pollers the server-rendered markup got.
   hash selects a settings tab or docs section, and defaults to the URL's. */
function initPage(hash) {
  initDashboard();
  initUsage();
  initClientGroups();
  initJobStats();
  initConnections();

  // Restore OS download tab selection from localStorage.
  document.querySelectorAll('.tab-btn[data-tab-store]').forEach(function(btn) {
    var store = btn.getAttribute('data-tab-store');
    try {
      var saved = localStorage.getItem('llmesh-tab-' + store);
      if (saved && btn.getAttribute('data-tab-target') === saved) activateTab(btn);
    } catch(e) {}
  });

  // Restore settings tab or docs section from URL hash.
  if (hash === undefined) hash = window.location.hash.slice(1);
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
}

document.addEventListener('DOMContentLoaded', function() { initPage(); });

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
  // Runs unconditionally: rows arrive after load now, and gating on one being
  // present at startup left their timers frozen at an em-dash.
  updateElapsed();
  setInterval(updateElapsed, 1000);
})();
