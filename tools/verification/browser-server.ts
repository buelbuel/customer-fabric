// Local browser-test fixture only. No mocks; /v1 is forwarded to the real API.
const files = new Map([['/', 'browser.html'], ['/browser.js', 'browser.js']]);
Bun.serve({
  hostname: '0.0.0.0', port: 5175,
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname.startsWith('/v1/')) {
      const upstream = new URL(url.pathname + url.search, 'http://api:8080');
      return fetch(new Request(upstream, request), { redirect: 'error' });
    }
    const file = files.get(url.pathname);
    if (!file || request.method !== 'GET') return new Response('Not found', {status: 404});
    return new Response(Bun.file(new URL(file, import.meta.url)), {headers:{'Cache-Control':'no-store','Content-Security-Policy':"default-src 'none'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'",'X-Content-Type-Options':'nosniff'}});
  },
});
