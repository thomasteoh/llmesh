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

/* ─── Usage panel (dashboard) ────────────────────────────────────
   Fetches /portal/api/usage and renders a stacked bar chart as inline
   SVG (no external chart library). Controls: range (24h/7d/30d/90d),
   grouping (model/user/key), metric (tokens/requests). */

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

  var state = { range: '7d', group: 'model', metric: 'tokens', data: null };
  try {
    state.range  = localStorage.getItem('llmesh-usage-range')  || state.range;
    state.group  = localStorage.getItem('llmesh-usage-group')  || state.group;
    state.metric = localStorage.getItem('llmesh-usage-metric') || state.metric;
  } catch (e) {}

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

  function seriesValues(s) {
    if (state.metric === 'requests') return s.requests;
    return s.prompt_tokens.map(function(p, i) { return p + s.completion_tokens[i]; });
  }

  function render() {
    var d = state.data;
    if (!d) return;
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
    var stackTotals = [];
    for (var i = 0; i < n; i++) {
      var t = 0;
      series.forEach(function(s) { t += seriesValues(s)[i]; });
      stackTotals.push(t);
    }
    var maxV = Math.max.apply(null, [1].concat(stackTotals));

    var empty = document.getElementById('usage-empty');
    if (empty) empty.classList.toggle('hidden', maxV > 1 || stackTotals.some(function(v){ return v > 0; }));

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
      lbl.textContent = fmtNum(Math.round(maxV * f));
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
      // Transparent hover strip for the tooltip.
      var hover = document.createElementNS(ns, 'rect');
      hover.setAttribute('x', padL + slot * b); hover.setAttribute('y', padT);
      hover.setAttribute('width', slot); hover.setAttribute('height', plotH);
      hover.setAttribute('fill', 'transparent');
      hover.setAttribute('data-bucket-idx', b);
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
        val.textContent = state.metric === 'requests'
          ? fmtNum(s.total_requests)
          : fmtNum(s.total_tokens);
        item.appendChild(val);
        legend.appendChild(item);
      });
    }
    var totals = document.getElementById('usage-totals');
    if (totals) {
      totals.innerHTML = '';
      [['Requests', d.totals.requests], ['Prompt tokens', d.totals.prompt_tokens],
       ['Completion tokens', d.totals.completion_tokens]].forEach(function(pair) {
        var sp = document.createElement('span');
        var b = document.createElement('b');
        b.textContent = fmtNum(pair[1]);
        sp.appendChild(b);
        sp.appendChild(document.createTextNode(' ' + pair[0]));
        totals.appendChild(sp);
      });
    }
  }

  /* Tooltip */
  var tip = document.getElementById('usage-tip');
  svg.addEventListener('mousemove', function(e) {
    if (!tip || !state.data) return;
    var t = e.target.closest('[data-bucket-idx]');
    if (!t) { tip.style.display = 'none'; return; }
    var b = parseInt(t.getAttribute('data-bucket-idx'), 10);
    var d = state.data;
    var hourly = d.range === '24h' || d.range === '7d';
    tip.innerHTML = '';
    var title = document.createElement('div');
    title.className = 'tip-title';
    title.textContent = fmtBucket(d.buckets[b], hourly);
    tip.appendChild(title);
    var any = false;
    (d.series || []).forEach(function(s, si) {
      var v = seriesValues(s)[b];
      if (v <= 0) return;
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
      val.textContent = fmtNum(v);
      row.appendChild(name); row.appendChild(val);
      tip.appendChild(row);
    });
    if (!any) {
      var none = document.createElement('div');
      none.className = 'tip-row';
      none.textContent = 'no usage';
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
  }, 60000, function(d) { state.data = d; render(); });

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
    if (mb) render(); else usagePoller.tick();
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

/* ─── Init ──────────────────────────────────────────────────── */

document.addEventListener('DOMContentLoaded', function() {
  initDashboard();
  initUsage();
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
