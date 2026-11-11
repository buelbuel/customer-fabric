/// <reference types="vite/client" />
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
type Segment = { name: string; revision: number; rule: { purpose_id: string; match: string; conditions: { attribute: string; operator: string }[] }; created_at: string };
type Excluded = { unavailable: number; consent: number; criteria: number };
type Preview = { revision: number; evaluated_at: string; population: number; members: number; excluded: Excluded;
  sample: { anchor_profile_id: string; conditions: { index: number; attribute: string; operator: string; evidence: { source: string; mutation_id: string }[] }[] }[] };
type Snapshot = { id: string; revision: number; taken_at: string; members: number; population: number };
type Counts = { pending: number; delivered: number; suppressed: number; failed: number };
type ActivationItem = { id: string; segment: string; revision: number; destination: string; created_at: string; counts: Counts };
type ActivationDetail = ActivationItem & { deliveries: { id: string; anchor_profile_id: string; state: string; attempts: number; redrives: number; reason?: string; receipts: { attempt: number; outcome: string; reason: string; status_code?: number; at: string }[] }[]; next_cursor?: string };
type Candidates = { population: number; total: number; candidates: { profile_id: string; source: string; class: string; agree: string[]; disagree: string[] }[] };
const operatorAccess = import.meta.env.VITE_CF_OPERATOR_ACCESS === '1';
const profilesPath = operatorAccess ? '/v1/operator/profiles' : '/v1/profiles';

function App() {
  const [token, setToken] = useState('');
  const [connected, setConnected] = useState(false);
  const [page, setPage] = useState<Page>({ items: [] });
  const [profile, setProfile] = useState<Profile | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [tab, setTab] = useState<'profiles' | 'segments' | 'activations'>('profiles');
  const [activations, setActivations] = useState<ActivationItem[] | null>(null);
  const [activation, setActivation] = useState<ActivationDetail | null>(null);
  const [segments, setSegments] = useState<Segment[] | null>(null);
  const [segment, setSegment] = useState<{ name: string; preview: Preview; snapshots: Snapshot[] } | null>(null);
  const [candidates, setCandidates] = useState<Candidates | null>(null);
  function clearSession() { setToken(''); setPage({ items: [] }); setProfile(null); setConnected(false); setSegments(null); setSegment(null); setCandidates(null); setActivations(null); setActivation(null); setTab('profiles'); }
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
    try { setPage(await request<Page>(`${profilesPath}?limit=20${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`)); setConnected(true); setProfile(null); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to connect.'); }
    finally { setBusy(false); }
  }
  async function openSegments() {
    setTab('segments'); setBusy(true); setError(''); setSegment(null);
    try { setSegments((await request<{ items: Segment[] }>('/v1/operator/segments')).items); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load segments.'); }
    finally { setBusy(false); }
  }
  async function openSegment(name: string) {
    setBusy(true); setError(''); setSegment(null);
    try {
      const [preview, snapshots] = await Promise.all([request<Preview>(`/v1/operator/segments/${encodeURIComponent(name)}/preview?limit=20`),
        request<{ items: Snapshot[] }>(`/v1/operator/segments/${encodeURIComponent(name)}/snapshots`)]);
      setSegment({ name, preview, snapshots: snapshots.items });
    } catch (e) { setError(e instanceof Error ? e.message : 'Unable to preview the segment.'); }
    finally { setBusy(false); }
  }
  async function openActivations() {
    setTab('activations'); setBusy(true); setError(''); setActivation(null);
    try { setActivations((await request<{ items: ActivationItem[] }>('/v1/operator/activations')).items); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load activations.'); }
    finally { setBusy(false); }
  }
  async function openActivation(id: string) {
    setBusy(true); setError(''); setActivation(null);
    try { setActivation(await request<ActivationDetail>(`/v1/operator/activations/${encodeURIComponent(id)}`)); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load the activation.'); }
    finally { setBusy(false); }
  }
  async function moreDeliveries() {
    if (!activation?.next_cursor) return;
    setBusy(true); setError('');
    try {
      const next = await request<ActivationDetail>(`/v1/operator/activations/${encodeURIComponent(activation.id)}?cursor=${encodeURIComponent(activation.next_cursor)}`);
      setActivation({ ...next, deliveries: [...activation.deliveries, ...next.deliveries] });
    }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load more deliveries.'); }
    finally { setBusy(false); }
  }
  async function findCandidates(id: string) {
    setBusy(true); setError(''); setCandidates(null);
    try { setCandidates(await request<Candidates>(`/v1/operator/profiles/${encodeURIComponent(id)}/match-candidates`)); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load match candidates.'); }
    finally { setBusy(false); }
  }
  async function open(id: string) {
    setBusy(true); setError(''); setProfile(null); setCandidates(null);
    try { setProfile(await request<Profile>(`${profilesPath}/${encodeURIComponent(id)}`)); }
    catch (e) { setError(e instanceof Error ? e.message : 'Unable to load profile.'); }
    finally { setBusy(false); }
  }
  function disconnect() { clearSession(); setError(''); }
  return <div className="shell">
    <header><a className="brand" href="/">▦ <span>Customer Fabric</span></a><span className="badge">LOCAL DEVELOPMENT</span></header>
    <main>
      <div className="heading"><div><p className="eyebrow">CUSTOMER OPERATIONS</p><h1>{tab === 'segments' ? 'Segments' : tab === 'activations' ? 'Activations' : 'Profiles'}</h1><p>{tab === 'segments' ? 'Preview who a segment selects now, who it excludes and why, without seeing their values.' : tab === 'activations' ? 'Follow each delivery to a destination: delivered, suppressed at dispatch, or failed.' : 'Inspect customer state, source identifiers, and the decisions behind each change.'}</p></div>{connected && <button onClick={disconnect} disabled={busy}>Disconnect</button>}</div>
      {!connected && <section className="connect"><h2>Connect to your instance</h2><p>{operatorAccess ? 'Use your local development operator credential.' : <>Use a source credential from your local <code>.env</code>.</>} Credentials stay in memory and are cleared on disconnect or reload. Use synthetic customer data only.</p><form onSubmit={e => { e.preventDefault(); void load(); }}><label htmlFor="token">{operatorAccess ? 'Development operator token' : 'Development access token'}</label><div className="row"><input id="token" type="password" autoComplete="off" spellCheck={false} disabled={busy} value={token} onChange={e => setToken(e.target.value)} required minLength={32} /><button className="primary" disabled={busy}>{busy ? 'Connecting…' : 'Connect'}</button></div></form></section>}
      {error && <div role="alert" className="error">{error}</div>}
      {connected && operatorAccess && <nav className="tabs" aria-label="Operator views"><button aria-current={tab === 'profiles' ? 'page' : undefined} disabled={busy} onClick={() => { setTab('profiles'); setError(''); }}>Profiles</button><button aria-current={tab === 'segments' ? 'page' : undefined} disabled={busy} onClick={() => void openSegments()}>Segments</button><button aria-current={tab === 'activations' ? 'page' : undefined} disabled={busy} onClick={() => void openActivations()}>Activations</button></nav>}
      {connected && tab === 'activations' && <div className="workspace" aria-busy={busy}>
        <section className="list"><div className="section-title"><h2>Recent activations</h2><button disabled={busy} onClick={() => void openActivations()}>Refresh</button></div>
          {!activations?.length ? <p className="empty">{busy ? 'Loading activations…' : 'No activations yet. Activate a segment with POST /v1/operator/segments/{name}/activations.'}</p> : <ul>{activations.map(a => <li key={a.id}><button className={activation?.id === a.id ? 'selected record' : 'record'} disabled={busy} onClick={() => void openActivation(a.id)}><span className="mono">{a.segment} → {a.destination}</span><small>{a.counts.delivered} delivered · {a.counts.suppressed} suppressed · {a.counts.failed} failed · {a.counts.pending} pending</small></button></li>)}</ul>}
        </section>
        <section className="detail">{!activation ? <div className="empty"><h2>{busy ? 'Loading…' : 'Select an activation'}</h2><p>Every delivery is decided again right before it is sent; receipts record decisions and status codes, never payloads.</p></div> : <>
          <p className="eyebrow">ACTIVATION · {activation.segment} REVISION {activation.revision}</p><h2 className="mono profile-id">{activation.id}</h2>
          <dl><div><dt>Destination</dt><dd>{activation.destination}</dd></div><div><dt>Created</dt><dd><time dateTime={activation.created_at}>{new Date(activation.created_at).toLocaleString()}</time></dd></div><div><dt>Deliveries</dt><dd>{activation.counts.delivered} delivered · {activation.counts.suppressed} suppressed · {activation.counts.failed} failed · {activation.counts.pending} pending</dd></div></dl>
          <h3>Deliveries</h3>{activation.deliveries.length ? <div className="table-wrap"><table><thead><tr><th>Customer</th><th>State</th><th>Reason</th><th>Attempts</th><th>Last status</th></tr></thead><tbody>{activation.deliveries.map(d => <tr key={d.id}><td className="mono">{d.anchor_profile_id}</td><td><span className={`outcome ${d.state === 'delivered' ? '' : 'rejected'}`}>{d.state}</span></td><td>{d.reason || '—'}</td><td>{d.attempts}{d.redrives ? ` (${d.redrives} redrives)` : ''}</td><td>{d.receipts.at(-1)?.status_code ?? '—'}</td></tr>)}</tbody></table></div> : <p>No deliveries were queued.</p>}
          {activation.next_cursor && <button type="button" disabled={busy} onClick={() => void moreDeliveries()}>Load more deliveries</button>}
        </>}</section>
      </div>}
      {connected && tab === 'segments' && <div className="workspace" aria-busy={busy}>
        <section className="list"><div className="section-title"><h2>Segments</h2><button disabled={busy} onClick={() => void openSegments()}>Refresh</button></div>
          {!segments?.length ? <p className="empty">{busy ? 'Loading segments…' : 'No segments yet. Define one with POST /v1/operator/segments/{name}.'}</p> : <ul>{segments.map(g => <li key={g.name}><button className={segment?.name === g.name ? 'selected record' : 'record'} disabled={busy} onClick={() => void openSegment(g.name)}><span className="mono">{g.name}</span><small>Revision {g.revision} · {g.rule.conditions.length} {g.rule.conditions.length === 1 ? 'condition' : 'conditions'}, match {g.rule.match}</small></button></li>)}</ul>}
        </section>
        <section className="detail">{!segment ? <div className="empty"><h2>{busy ? 'Loading…' : 'Select a segment'}</h2><p>A live preview evaluates every customer now. Members must permit the segment's purpose; values are never shown.</p></div> : <>
          <p className="eyebrow">SEGMENT · REVISION {segment.preview.revision}</p><h2 className="mono profile-id">{segment.name}</h2>
          <dl><div><dt>Members</dt><dd>{segment.preview.members} of {segment.preview.population} customers</dd></div><div><dt>Excluded: consent</dt><dd>{segment.preview.excluded.consent}</dd></div><div><dt>Excluded: criteria</dt><dd>{segment.preview.excluded.criteria}</dd></div><div><dt>Excluded: unavailable</dt><dd>{segment.preview.excluded.unavailable}</dd></div><div><dt>Evaluated</dt><dd><time dateTime={segment.preview.evaluated_at}>{new Date(segment.preview.evaluated_at).toLocaleString()}</time></dd></div></dl>
          <h3>Sample members</h3><p className="muted">Each member is explained by the conditions that admitted them and the provenance of the deciding values.</p>
          {segment.preview.sample.length ? <ol className="history">{segment.preview.sample.map(m => <li key={m.anchor_profile_id}><code>{m.anchor_profile_id}</code>{m.conditions.map(c => <p key={c.index}>{c.attribute} {c.operator} · {c.evidence.map(e => e.source).join(', ') || 'no value needed'}</p>)}</li>)}</ol> : <p>No current members.</p>}
          <h3>Snapshots</h3>{segment.snapshots.length ? <div className="table-wrap"><table><thead><tr><th>Taken</th><th>Revision</th><th>Members</th></tr></thead><tbody>{segment.snapshots.map(n => <tr key={n.id}><td><time dateTime={n.taken_at}>{new Date(n.taken_at).toLocaleString()}</time></td><td>{n.revision}</td><td>{n.members} of {n.population}</td></tr>)}</tbody></table></div> : <p>No snapshots yet.</p>}
        </>}</section>
      </div>}
      {connected && tab === 'profiles' && <div className="workspace" aria-busy={busy}>
        <section className="list"><div className="section-title"><h2>Customer records</h2><button disabled={busy} onClick={() => void load()}>Refresh</button></div>
          {page.items.length === 0 ? <p className="empty">{error ? 'Profiles are unavailable. Retry after resolving the error.' : busy ? 'Loading profiles…' : 'No profiles yet. Create a synthetic profile using the documented API example.'}</p> : <ul>{page.items.map(p => <li key={p.id}><button className={profile?.id === p.id ? 'selected record' : 'record'} disabled={busy} onClick={() => void open(p.id)}><span className="mono">{p.id}</span><small>Version {p.version} <span aria-hidden="true">↗</span></small></button></li>)}</ul>}
          <div className="pagination"><button disabled={busy} onClick={() => void load()}>First page</button><button disabled={busy || !page.next_cursor} onClick={() => void load(page.next_cursor)}>Next page →</button></div>
        </section>
        <section className="detail">{!profile ? <div className="empty"><h2>{busy ? 'Loading…' : 'Select a profile'}</h2><p>Attributes and provenance will appear here.</p></div> : <>
          <p className="eyebrow">PROFILE · VERSION {profile.version}</p><h2 className="mono profile-id">{profile.id}</h2>
          <h3>Attributes</h3>{Object.keys(profile.attributes).length ? <dl>{Object.entries(profile.attributes).map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{String(value)}</dd></div>)}</dl> : <p>No current attributes.</p>}
          <h3>External identifiers</h3>{profile.identifiers.length ? <div className="table-wrap"><table><thead><tr><th>Source</th><th>Namespace</th><th>Value</th></tr></thead><tbody>{profile.identifiers.map(i => <tr key={`${i.source}/${i.namespace}/${i.value}`}><td>{i.source}</td><td>{i.namespace}</td><td>{i.value}</td></tr>)}</tbody></table></div> : <p>No external identifiers.</p>}
          {operatorAccess && <><h3>Match candidates</h3><p className="muted">Proposals for review only: nothing is linked or merged. Signals are named, never shown.</p>
            {!candidates ? <button disabled={busy} onClick={() => void findCandidates(profile.id)}>Find match candidates</button> : candidates.candidates.length ? <>{candidates.total > candidates.candidates.length && <p className="muted">Showing the first {candidates.candidates.length} of {candidates.total} candidates.</p>}<div className="table-wrap"><table><thead><tr><th>Class</th><th>Record</th><th>Source</th><th>Agree</th><th>Disagree</th></tr></thead><tbody>{candidates.candidates.map(c => <tr key={c.profile_id}><td><span className={`outcome ${c.class === 'conflicting' ? 'rejected' : ''}`}>{c.class}</span></td><td className="mono">{c.profile_id}</td><td>{c.source}</td><td>{c.agree.join(', ')}</td><td>{c.disagree.join(', ') || '—'}</td></tr>)}</tbody></table></div></> : <p>No candidates among {candidates.population} source records.</p>}</>}
          <h3>Provenance</h3><p className="muted">Most recent 100 attribute decisions. Values are not copied into audit records.</p>
          {profile.provenance.length ? <ol className="history">{profile.provenance.map((d, i) => <li key={`${d.mutation_id}/${d.path}/${i}`}><div><strong>{d.path}</strong> <span className={`outcome ${d.outcome}`}>{d.outcome}</span></div><p>{d.source} · {d.rule}</p><time dateTime={d.at}>{new Date(d.at).toLocaleString()}</time><code>{d.mutation_id}</code></li>)}</ol> : <p>No attribute decisions.</p>}
        </>}</section>
      </div>}
      <footer>Independent infrastructure. No external telemetry. <span>{operatorAccess ? 'M1 · Development operator access' : 'M0 · Development access only'}</span></footer>
    </main>
  </div>;
}
createRoot(document.getElementById('root')!).render(<App />);
