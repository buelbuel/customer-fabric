const form = document.querySelector('#request');
const field = id => document.querySelector(`#${id}`).value;
form.addEventListener('submit', async event => {
  event.preventDefault();
  const output = document.querySelector('#response');
  const path = field('path');
  if (!/^\/v1\/profiles(?:\/[0-9a-f-]+)?$/.test(path)) { output.textContent = 'Only local profile API paths are allowed.'; return; }
  const headers = { Authorization: `Bearer ${field('token')}`, 'Content-Type': 'application/json' };
  if (field('key')) headers['Idempotency-Key'] = field('key');
  if (field('etag')) headers['If-Match'] = field('etag');
  try {
    const response = await fetch(path, { method: field('method'), headers, credentials: 'omit', redirect: 'error', cache: 'no-store', body: field('method') === 'GET' ? undefined : field('body'), signal: AbortSignal.timeout(10_000) });
    output.textContent = `${response.status}\n${JSON.stringify(await response.json(), null, 2)}`;
  } catch { output.textContent = 'Network request failed.'; }
});
