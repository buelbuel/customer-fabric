// Synthetic activation destination for local demos and contracts. It keeps what it receives in memory
// and answers 503 on /hook/unavailable so retries can be observed.
const received: unknown[] = [];

Bun.serve({
  port: 8090,
  async fetch(req) {
    const url = new URL(req.url);
    if (req.method === 'POST' && url.pathname.startsWith('/hook/')) {
      const status = url.pathname === '/hook/unavailable' ? 503 : 200;
      received.push({
        path: url.pathname,
        idempotency_key: req.headers.get('idempotency-key'),
        attempt: req.headers.get('cf-delivery-attempt'),
        signature: req.headers.get('cf-signature'),
        body: await req.text(),
        status,
      });
      if (received.length > 1000) received.shift();
      return new Response(null, {status});
    }
    if (req.method === 'GET' && url.pathname === '/received') return Response.json(received);
    return new Response(null, {status: 404});
  },
});
