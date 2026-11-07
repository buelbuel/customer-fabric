import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import './style.css';

type Summary = { id: string; version: number };
type Profile = Summary & {
  attributes: Record<string, string | number | boolean>;
  identifiers: { source: string; namespace: string; value: string }[];
  provenance: { path: string; source: string; mutation_id: string; at: string; outcome: string; rule: string }[];
};
type Page = { items: Summary[]; next_cursor?: string };
function App() {
  const [token, setToken] = useState('');
  const [connected, setConnected] = useState(false);
  const [page, setPage] = useState<Page>({ items: [] });
  const [profile, setProfile] = useState<Profile | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  function clearSession() { setToken(''); setPage({ items: [] }); setProfile(null); setConnected(false); }
  async function request<T>(path: string): Promise<T> {
    let response: Response;
    try {
      response = await fetch(path, {
        headers: { Authorization: `Bearer ${token}` }, cache: 'no-store',
        credentials: 'omit', redirect: 'error', signal: AbortSignal.timeout(10_000),
      });
    } catch { throw new Error('Unable to reach the API. Check the local service and try again.'); }
    if (response.status === 401) {
      clearSession();
      throw new Error('401 · Access rejected. Your credential and displayed data have been cleared. Connect again with a valid credential.');
    }
    let body;
    try { body = await response.json(); }
    catch { throw new Error(`${response.status} · The API returned an unreadable response. Try again when the service is available.`); }
    if (!response.ok) throw new Error(`${response.status} · ${body.title || 'Request failed'}: ${body.detail || 'Try again.'}`);
    return body as T;
  }
  async function load(cursor = '') {
    setBusy(true); setError(''); setPage({ items: [] }); setProfile(null);
    try { setPage(await request<Page>(`/v1/profiles?limit=20${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`)); setConnected(true); setProfile(null); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to connect.'); }
    finally { setBusy(false); }
  }
  async function open(id: string) {
    setBusy(true); setError(''); setProfile(null);
    try { setProfile(await request<Profile>(`/v1/profiles/${id}`)); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load profile.'); }
    finally { setBusy(false); }
  }
  function disconnect() { clearSession(); setError(''); }
  return <div className="shell">
    <header><a className="brand" href="/">▦ <span>Customer Fabric</span></a><span className="badge">LOCAL DEVELOPMENT</span></header>
    <main>
      <div className="heading"><div><p className="eyebrow">CUSTOMER OPERATIONS</p><h1>Profiles</h1><p>Inspect customer state, source identifiers, and the decisions behind each change.</p></div>{connected && <button onClick={disconnect} disabled={busy}>Disconnect</button>}</div>
      {!connected && <section className="connect"><h2>Connect to your instance</h2><p>Use a source credential from your local <code>.env</code>. Credentials stay in memory and are cleared on disconnect or reload. Use synthetic customer data only.</p><form onSubmit={e => { e.preventDefault(); void load(); }}><label htmlFor="token">Development access token</label><div className="row"><input id="token" type="password" autoComplete="off" spellCheck={false} disabled={busy} value={token} onChange={e => setToken(e.target.value)} required minLength={32} /><button className="primary" disabled={busy}>{busy ? 'Connecting…' : 'Connect'}</button></div></form></section>}
      {error && <div role="alert" className="error">{error}</div>}
      {connected && <div className="workspace" aria-busy={busy}>
        <section className="list"><div className="section-title"><h2>Customer records</h2><button disabled={busy} onClick={() => void load()}>Refresh</button></div>
          {page.items.length === 0 ? <p className="empty">{error ? 'Profiles are unavailable. Retry after resolving the error.' : busy ? 'Loading profiles…' : 'No profiles yet. Create a synthetic profile using the documented API example.'}</p> : <ul>{page.items.map(p => <li key={p.id}><button className={profile?.id === p.id ? 'selected record' : 'record'} disabled={busy} onClick={() => void open(p.id)}><span className="mono">{p.id}</span><small>Version {p.version} <span aria-hidden="true">↗</span></small></button></li>)}</ul>}
          <div className="pagination"><button disabled={busy} onClick={() => void load()}>First page</button><button disabled={busy || !page.next_cursor} onClick={() => void load(page.next_cursor)}>Next page →</button></div>
        </section>
        <section className="detail">{!profile ? <div className="empty"><h2>{busy ? 'Loading…' : 'Select a profile'}</h2><p>Attributes and provenance will appear here.</p></div> : <>
          <p className="eyebrow">PROFILE · VERSION {profile.version}</p><h2 className="mono profile-id">{profile.id}</h2>
          <h3>Attributes</h3>{Object.keys(profile.attributes).length ? <dl>{Object.entries(profile.attributes).map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{String(value)}</dd></div>)}</dl> : <p>No current attributes.</p>}
          <h3>External identifiers</h3>{profile.identifiers.length ? <div className="table-wrap"><table><thead><tr><th>Source</th><th>Namespace</th><th>Value</th></tr></thead><tbody>{profile.identifiers.map(i => <tr key={`${i.source}/${i.namespace}/${i.value}`}><td>{i.source}</td><td>{i.namespace}</td><td>{i.value}</td></tr>)}</tbody></table></div> : <p>No external identifiers.</p>}
          <h3>Provenance</h3><p className="muted">Most recent 100 attribute decisions. Values are not copied into audit records.</p>
          {profile.provenance.length ? <ol className="history">{profile.provenance.map((d, i) => <li key={`${d.mutation_id}/${d.path}/${i}`}><div><strong>{d.path}</strong> <span className={`outcome ${d.outcome}`}>{d.outcome}</span></div><p>{d.source} · {d.rule}</p><time dateTime={d.at}>{new Date(d.at).toLocaleString()}</time><code>{d.mutation_id}</code></li>)}</ol> : <p>No attribute decisions.</p>}
        </>}</section>
      </div>}
      <footer>Independent infrastructure. No external telemetry. <span>M0 · Development access only</span></footer>
    </main>
  </div>;
}
createRoot(document.getElementById('root')!).render(<App />);
