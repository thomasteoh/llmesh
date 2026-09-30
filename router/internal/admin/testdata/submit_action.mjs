// Exercises admin.js's submitAction against stubbed responses.
//
// The portal's forms are progressively enhanced: the script posts them by fetch
// and swaps the result in. That path has no server-side test — the Go tests can
// only pin what the handlers return, not what the page does with it — and it is
// where a created API key went missing, so it is worth driving directly.
//
// admin.js is loaded whole into a vm with a DOM stub thin enough to get through
// its top-level setup. The stub is not a browser: it records what was asked of
// it, which is what the assertions are about.
//
// Usage: node submit_action.mjs <path-to-admin.js>   — exits non-zero on failure.

import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(process.argv[2], 'utf8');

function el(html = '') {
  return {
    innerHTML: html,
    textContent: '',
    className: '',
    style: {},
    hidden: false,
    classList: { add() {}, remove() {}, toggle: () => false, contains: () => false },
    getAttribute: () => null,
    setAttribute() {},
    hasAttribute: () => false,
    removeAttribute() {},
    appendChild() {},
    remove() {},
    closest: () => null,
    contains: () => false,
    querySelector: () => null,
    querySelectorAll: () => [],
    addEventListener() {},
    focus() {},
  };
}

// Pulls <main> out of a document the way DOMParser would, without a parser.
function mainOf(html) {
  const m = /<main[^>]*>([\s\S]*?)<\/main>/i.exec(html);
  return m ? el(m[1]) : null;
}

// Gives a parsed <main> the content column it sits in, holding the page's
// flash banners the way layout.html renders them: beside <main>, not in it.
function withFlashes(main, html) {
  if (!main) return main;
  const flashes = [...html.matchAll(/<div class="flash[^"]*"[^>]*>([^<]*)<\/div>/g)].map((m) => el(m[1]));
  main.parentNode = { querySelectorAll: () => flashes };
  return main;
}

// A content column around the current <main>, recording the banners it holds.
function column(main, texts) {
  const col = { flashes: [] };
  const add = (text) => {
    const f = el(text);
    f.remove = () => {
      col.flashes = col.flashes.filter((x) => x !== f);
    };
    col.flashes.push(f);
  };
  texts.forEach(add);
  col.querySelectorAll = () => col.flashes.slice();
  col.insertBefore = (node) => add(node.innerHTML);
  main.parentNode = col;
  return col;
}

function makeContext(opts) {
  const log = {
    fetches: [],
    formSubmitted: 0,
    navigatedTo: [],
    reloaded: 0,
    initPages: 0,
  };
  const currentMain = el('<p>original</p>');

  const listeners = {};
  const document = {
    documentElement: { getAttribute: () => 'dark', setAttribute() {} },
    getElementById: () => null,
    querySelector: (sel) => (sel === 'main' ? currentMain : null),
    querySelectorAll: () => [],
    addEventListener: (name, fn) => {
      listeners[name] = fn;
    },
    removeEventListener() {},
    createElement: () => el(),
    importNode: (node) => node,
    cookie: '',
    hidden: false,
  };

  const location = {
    pathname: '/portal/api-keys',
    search: '',
    get href() {
      return this.pathname;
    },
    set href(v) {
      log.navigatedTo.push(v);
    },
    reload: () => {
      log.reloaded++;
    },
  };

  const context = {
    document,
    console,
    setInterval: () => 0,
    clearInterval: () => {},
    setTimeout: () => 0,
    Date,
    Math,
    JSON,
    Promise,
    Array,
    Object,
    String,
    Number,
    Error,
    RegExp,
    encodeURIComponent,
    decodeURIComponent,
    isNaN,
    parseInt,
    parseFloat,
    localStorage: {
      getItem: () => null,
      setItem() {},
    },
    history: { replaceState() {}, pushState() {} },
    FormData: class {
      constructor() {}
    },
    HTMLFormElement: class {},
    DOMParser: class {
      parseFromString(html) {
        return { querySelector: (sel) => (sel === 'main' ? withFlashes(mainOf(html), html) : null) };
      }
    },
    fetch: (url, init) => {
      log.fetches.push({ url, method: (init && init.method) || 'GET' });
      return opts.respond(url, init);
    },
  };
  context.window = context;
  context.window.location = location;
  context.globalThis = context;

  vm.createContext(context);
  vm.runInContext(source, context, { filename: 'admin.js' });

  // Real initPage wires the whole page; the stub just records that the swap
  // finished and handed control back.
  context.initPage = () => {
    log.initPages++;
  };

  return { context, log, currentMain, listeners };
}

function response({ status = 200, headers = {}, body = '' }) {
  const lower = {};
  for (const [k, v] of Object.entries(headers)) lower[k.toLowerCase()] = v;
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (name) => lower[name.toLowerCase()] ?? null },
    text: () => Promise.resolve(body),
  });
}

const form = {
  getAttribute: (name) => (name === 'action' ? '/portal/api-keys' : null),
  submit() {},
};

const failures = [];
function check(name, fn) {
  return Promise.resolve()
    .then(fn)
    .then(
      () => console.log(`ok   ${name}`),
      (err) => {
        failures.push(name);
        console.log(`FAIL ${name}\n     ${err.message}`);
      },
    );
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

// Lets the promise chain inside submitAction settle.
const settle = () => new Promise((r) => setImmediate(() => setImmediate(() => setImmediate(r))));

const PAGE = '<html><body><main><div id="banner">sk-alice-SECRET</div></main></body></html>';

await check('a page answering the post is swapped in, secret and all', async () => {
  const { context, log, currentMain } = makeContext({
    respond: () => response({ status: 200, headers: { 'Content-Type': 'text/html; charset=utf-8' }, body: PAGE }),
  });
  const submitted = { n: 0 };
  context.submitAction({ ...form, submit: () => submitted.n++ });
  await settle();

  assert(log.fetches.length === 1, `expected 1 request, got ${log.fetches.length}`);
  assert(
    currentMain.innerHTML.includes('sk-alice-SECRET'),
    `the created secret never reached the page: ${currentMain.innerHTML}`,
  );
  assert(log.navigatedTo.length === 0, `should not navigate: ${log.navigatedTo}`);
  assert(log.reloaded === 0, 'should not reload');
  assert(submitted.n === 0, 'should not re-post the form');
  assert(log.initPages === 1, `page setup should re-run once, ran ${log.initPages}`);
});

await check('204 for the current page refetches and swaps it', async () => {
  const { context, log, currentMain } = makeContext({
    respond: (url, init) =>
      (init && init.method) === 'POST'
        ? response({ status: 204, headers: { 'X-Portal-Location': '/portal/api-keys' } })
        : response({ status: 200, headers: { 'Content-Type': 'text/html' }, body: '<main><p>refreshed</p></main>' }),
  });
  context.submitAction(form);
  await settle();

  assert(log.fetches.length === 2, `expected a post then a get, got ${log.fetches.length}`);
  assert(log.fetches[1].method === 'GET', 'the second request should be the content refetch');
  assert(currentMain.innerHTML.includes('refreshed'), `content not swapped: ${currentMain.innerHTML}`);
  assert(log.navigatedTo.length === 0, `should not navigate: ${log.navigatedTo}`);
});

await check('204 pointing elsewhere navigates', async () => {
  const { context, log } = makeContext({
    respond: () => response({ status: 204, headers: { 'X-Portal-Location': '/portal/settings#tab-users' } }),
  });
  context.submitAction(form);
  await settle();

  assert(
    log.navigatedTo.includes('/portal/settings#tab-users'),
    `expected navigation, got ${JSON.stringify(log.navigatedTo)}`,
  );
});

await check('an expired session sends the page to the login it was given', async () => {
  const { context, log } = makeContext({
    respond: () =>
      response({
        status: 401,
        headers: { 'X-Portal-Location': '/portal/login', 'Content-Type': 'text/plain' },
        body: 'session expired\n',
      }),
  });
  let submitted = 0;
  context.submitAction({ ...form, submit: () => submitted++ });
  await settle();

  assert(
    log.navigatedTo.includes('/portal/login'),
    `expected the login redirect to be followed, navigations: ${JSON.stringify(log.navigatedTo)}`,
  );
});

await check('a refused action is not swallowed', async () => {
  // No destination to follow — a CSRF rejection or a rate limit. Re-posting as
  // a plain form is how the server's own error page gets shown, which is what
  // the code has always claimed to do.
  const { context } = makeContext({
    respond: () => response({ status: 403, headers: { 'Content-Type': 'text/plain' }, body: 'forbidden' }),
  });
  let submitted = 0;
  context.submitAction({ ...form, submit: () => submitted++ });
  await settle();

  assert(submitted === 1, `a refused action did nothing at all (form resubmits: ${submitted})`);
});

await check('a network failure falls back to a real form post', async () => {
  const { context } = makeContext({ respond: () => Promise.reject(new Error('offline')) });
  let submitted = 0;
  context.submitAction({ ...form, submit: () => submitted++ });
  await settle();

  assert(submitted === 1, `expected the form to be resubmitted once, got ${submitted}`);
});

await check('a page that will not swap reloads rather than re-posting', async () => {
  // The response has no <main>, so the swap throws — after the key has already
  // been minted. Re-running the form here would mint a second one.
  const { context, log } = makeContext({
    respond: () =>
      response({ status: 200, headers: { 'Content-Type': 'text/html' }, body: '<html><body>no main</body></html>' }),
  });
  let submitted = 0;
  context.submitAction({ ...form, submit: () => submitted++ });
  await settle();

  assert(submitted === 0, `the action already succeeded; re-posting would repeat it (${submitted} resubmits)`);
  assert(log.reloaded === 1, `expected one reload, got ${log.reloaded}`);
});

await check('a body that fails mid-read after a successful create does not re-post', async () => {
  // The server took the action and started answering, then the read failed.
  // The fallback for a failed request is to post the form again, and here that
  // would be the second key — so acceptance, not failure, decides.
  const { context, log } = makeContext({
    respond: () =>
      Promise.resolve({
        ok: true,
        status: 200,
        headers: { get: (n) => (n.toLowerCase() === 'content-type' ? 'text/html' : null) },
        text: () => Promise.reject(new Error('connection reset')),
      }),
  });
  let submitted = 0;
  context.submitAction({ ...form, submit: () => submitted++ });
  await settle();

  assert(submitted === 0, `re-posted an action the server had already accepted (${submitted} resubmits)`);
  assert(log.reloaded === 1, `expected one reload, got ${log.reloaded}`);
});

await check("a page's banner replaces the one already showing", async () => {
  const { context, currentMain } = makeContext({
    respond: () =>
      response({
        status: 200,
        headers: { 'Content-Type': 'text/html' },
        body: '<html><body><div class="flash error" role="alert">Role mapping: bogus is not name=value</div><main><p>form</p></main></body></html>',
      }),
  });
  const col = column(currentMain, ['Rule saved.']);
  context.submitAction(form);
  await settle();

  const shown = col.flashes.map((f) => f.innerHTML);
  assert(shown.length === 1 && shown[0].includes('bogus'), `banners after the swap: ${JSON.stringify(shown)}`);
});

await check('a page with no banner clears a stale one', async () => {
  const { context, currentMain } = makeContext({
    respond: (url, init) =>
      (init && init.method) === 'POST'
        ? response({ status: 204, headers: { 'X-Portal-Location': '/portal/api-keys' } })
        : response({ status: 200, headers: { 'Content-Type': 'text/html' }, body: '<main><p>refreshed</p></main>' }),
  });
  const col = column(currentMain, ['Rule saved.']);
  context.submitAction(form);
  await settle();

  assert(col.flashes.length === 0, `a stale banner survived: ${JSON.stringify(col.flashes.map((f) => f.innerHTML))}`);
});

for (const [cancelled, want] of [[true, 0], [false, 1]]) {
  await check(`a ${cancelled ? 'cancelled' : 'confirmed'} confirmation posts ${want ? 'the action' : 'nothing'}`, async () => {
    const { context, log, listeners } = makeContext({ respond: () => response({ status: 204 }) });
    const target = Object.assign(new context.HTMLFormElement(), {
      getAttribute: (name) => ({ method: 'POST', action: '/portal/clients/revoke' })[name] ?? null,
      submit() {},
    });
    // An inline onsubmit confirm that returns false marks the event
    // defaultPrevented; it still bubbles to the document listener.
    listeners.submit({ defaultPrevented: cancelled, target, preventDefault() {} });
    await settle();

    const posts = log.fetches.filter((f) => f.method === 'POST');
    assert(posts.length === want, `expected ${want} posts, got ${JSON.stringify(posts)}`);
  });
}

if (failures.length) {
  console.log(`\n${failures.length} failing: ${failures.join(', ')}`);
  process.exit(1);
}
console.log('\nall passed');
