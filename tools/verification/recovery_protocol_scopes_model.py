#!/usr/bin/env python3
"""Bounded model of ADR 0010 across intersecting erasure scopes.

Bounded verification, not a proof. It enumerates interleavings within small budgets
and says nothing about behaviour outside them, or about real PostgreSQL, networks or
storage. Lock acquisition is modelled as atomic for a whole lock set, so lock-order
deadlocks are out of scope.

Subjects A, B and C hold bindings on profiles in a chain: A and B share profile 1, B
and C share profile 2. An intent is split into per-binding subscopes. Unlink stands for
every commit-first event, including withdrawals. The oracle functions do not read the
configuration, so a mutant changes the protocol but never the property it is judged by.
Each mutant must produce a counterexample, printed as a full trace.
"""
from collections import deque
from typing import NamedTuple, Optional
import sys

ITEMS = {"a0": 0, "a1": 1, "b1": 1, "b2": 2, "c2": 2, "c3": 3}
SUBJECT = {"a0": "A", "a1": "A", "b1": "B", "b2": "B", "c2": "C", "c3": "C"}
SUBJECTS = ("A", "B", "C")


def bindings_of(subject, linked):
    return frozenset(b for b in linked if SUBJECT[b] == subject)


def items_of(binds):
    return frozenset(ITEMS[b] for b in binds)


class Db(NamedTuple):
    linked: frozenset = frozenset()
    erased: frozenset = frozenset()
    holds: frozenset = frozenset()
    unproven: frozenset = frozenset()  # bindings whose separation from other people cannot be proven
    unjournaled: frozenset = frozenset()  # commit-first unlinks awaiting the journal
    unproven_queue: frozenset = frozenset()  # lost separation awaiting the journal
    gen: int = 0
    isolated: bool = False
    anchor: frozenset = frozenset()
    hb: int = 0  # position of the last committed heartbeat, 1-based
    applied: int = 0  # dense low watermark: every earlier position is resolved
    timeline: int = 0


class Entry(NamedTuple):
    id: str
    kind: str
    gen: int
    subject: Optional[str] = None
    locks: frozenset = frozenset()
    binds: frozenset = frozenset()
    withheld: frozenset = frozenset()
    auth: bool = False
    prev: int = 0


class Journal(NamedTuple):
    entries: tuple = ()
    up: bool = True
    base: int = 0


class Worker(NamedTuple):
    op: str
    pc: str
    gen: int
    id: str
    expected: int
    subject: Optional[str]
    locks: frozenset
    binds: frozenset
    withheld: frozenset
    auth: bool
    holding: bool
    zombie: bool
    verified_upto: int


class Ghost(NamedTuple):
    fault: Optional[str] = None
    committed: frozenset = frozenset()  # bindings a worker's erasure truly committed
    aborted: frozenset = frozenset()  # subjects whose attempt observably rolled back
    acked: frozenset = frozenset()
    journaled: frozenset = frozenset()
    witnessed: frozenset = frozenset()
    late_lost: frozenset = frozenset()
    accepted_losses: frozenset = frozenset()
    undeclared: bool = False
    restored: bool = False
    timeline_lost: bool = False
    covered_at: frozenset = frozenset()  # (write, heartbeat position that committed after it)
    salvage_skipped: bool = False
    ever_unproven: frozenset = frozenset()


class State(NamedTuple):
    db: Db
    journal: Journal
    witness: frozenset
    witness_up: bool
    workers: tuple
    late: Optional[tuple]
    sweep: Optional[tuple]  # (phase, intent id, targets, locks)
    hb_flight: Optional[int]  # the one heartbeat transaction between append and commit
    snaps: tuple
    old: Optional[Db]
    old_stopped: bool
    restore: Optional[str]
    activation: bool
    ghost: Ghost
    next_id: int
    budget: tuple


BUDGET_NAMES = ("erase_a", "erase_b", "erase_c", "hold", "release", "refuse", "resume", "unlink", "share", "link",
                "snap", "restore", "undeclared", "outage", "heartbeat", "crash", "timeout", "abort", "old_write",
                "witness", "witness_outage", "failover_ok", "failover_bad", "advance", "prune")


class LimitReached(Exception):
    pass


class Config(NamedTuple):
    budget: tuple
    hold_items: frozenset = frozenset({1})
    hold_owner: str = "A"
    initial: frozenset = frozenset({"a0", "a1", "b1", "b2"})
    unproven: frozenset = frozenset()
    no_generation_check: bool = False
    match_hold_by_subject: bool = False
    later_holds_only: bool = False
    hold_blocks_whole_intent: bool = False
    no_expected_position: bool = False
    no_rescan_on_conflict: bool = False
    no_scope_shrink: bool = False
    forward_uses_current_links: bool = False
    forward_ignores_separation: bool = False
    treat_unproven_as_separable: bool = False
    void_on_absence: bool = False
    no_isolation: bool = False
    activation_stays_open: bool = False
    restore_forwards_before_window: bool = False
    no_salvage: bool = False
    hold_without_locks: bool = False
    act_before_reconcile: bool = False
    no_forward_claim: bool = False
    partial_replay_locks: bool = False
    ignore_refusals: bool = False
    refusal_by_locks: bool = False
    newer_intent_by_locks: bool = False
    auto_resume_on_release: bool = False
    release_unconfirmed: bool = False
    resume_without_authorization: bool = False
    resume_stale_scope: bool = False
    no_heartbeat_check: bool = False
    heartbeat_no_cas: bool = False
    heartbeat_strict_prev: bool = False
    non_idempotent_flush: bool = False
    timeline_reference_in_db: bool = False
    lossless_failover_unrecorded: bool = False
    watermark_on_commit: bool = False
    ack_without_suppression: bool = False
    journaled_on_commit: bool = False
    recovery_ignores_witness: bool = False
    witness_in_db_domain: bool = False
    check_undetectable: bool = False
    check_hold_tradeoff: bool = False
    check_separation_rollback: bool = False


def gen_of(j, upto=None):
    entries = j.entries if upto is None else j.entries[:upto]
    return sum(1 for e in entries if e.kind == "gen")


def timeline_of(j):
    return sum(1 for e in j.entries if e.kind == "timeline")


def head(j):
    return len(j.entries)


def spend(s, name):
    i = BUDGET_NAMES.index(name)
    if s.budget[i] <= 0:
        return None
    return s._replace(budget=s.budget[:i] + (s.budget[i] - 1,) + s.budget[i + 1:])


def put(s, i, w):
    return s._replace(workers=s.workers[:i] + (w,) + s.workers[i + 1:])


def drop(s, i):
    return s._replace(workers=s.workers[:i] + (None,) + s.workers[i + 1:])


def fault(ghost, message):
    return ghost if ghost.fault else ghost._replace(fault=message)


def held_items(s):
    out = frozenset()
    for w in s.workers:
        if w and w.holding:
            out |= w.locks
    if s.sweep:
        out |= s.sweep[3]
    return out


def position_of(j, kind, id_):
    return next((k for k, e in enumerate(j.entries) if e.kind == kind and e.id == id_), None)


def confirmed(j, id_):
    return position_of(j, "confirm", id_) is not None


def confirmed_binds(j, id_):
    """A confirmation names the subscopes that executed, not the whole intent."""
    position = position_of(j, "confirm", id_)
    return frozenset() if position is None else j.entries[position].binds


# Oracles. These read only the journal and database, never the configuration.

def oracle_hold_forbids(j, i, item):
    """A hold stops an intent at position i unless its release was confirmed before i."""
    for e in j.entries:
        if e.kind == "hold" and item in e.locks:
            end = position_of(j, "release_confirm", e.id)
            if end is None or i < end:
                return True
    return False


def oracle_overruled(j, i, intent):
    for k, e in enumerate(j.entries):
        if k > i and e.kind in ("intent", "refusal") and e.subject == intent.subject:
            return True
    return intent.gen < gen_of(j, i)


def oracle_stopped(j, i, intent):
    return position_of(j, "void", intent.id) is not None or oracle_overruled(j, i, intent)


def separation_lost(db, j, b):
    return b in db.unproven or any(e.kind == "unprove" and b in e.binds for e in j.entries)


def was_blocked(j, b):
    """A binding some intent named or withheld while a hold stopped it."""
    return any(e.kind == "intent" and b in e.binds | e.withheld and oracle_hold_forbids(j, i, ITEMS[b])
               for i, e in enumerate(j.entries))


def execution_fault(s, position, targets, intent):
    j, db, g = s.journal, s.db, s.ghost
    for b in sorted(targets):
        if oracle_hold_forbids(j, position, ITEMS[b]):
            return "erasure executed against a hold not released before the intent"
        if separation_lost(db, j, b):
            return "erasure executed on a binding whose separation cannot be proven"
        earlier = any(e.kind == "intent" and k < position and b in e.binds | e.withheld and oracle_hold_forbids(j, k, ITEMS[b])
                      for k, e in enumerate(j.entries))
        if earlier and not intent.auth:
            return "erasure resumed after a hold without a recorded authorization"
    if oracle_stopped(j, position, intent):
        return "erasure executed against a refusal, void or newer intent of the same subject"
    if intent.subject in g.aborted:
        return "erasure executed after an observed rollback"
    return None


# Protocol.

def hold_end(cfg, j, e):
    kind = "release" if cfg.release_unconfirmed else "release_confirm"
    return position_of(j, kind, e.id)


def active_holds(cfg, j):
    out = frozenset()
    for e in j.entries:
        if e.kind == "hold" and hold_end(cfg, j, e) is None:
            out |= e.locks
    return out


def hold_blocks(cfg, j, i, intent, item):
    for k, e in enumerate(j.entries):
        if e.kind != "hold":
            continue
        if cfg.later_holds_only and k < i:
            continue
        if cfg.match_hold_by_subject:
            if e.subject != intent.subject:
                continue
        elif not (item in e.locks or (cfg.hold_blocks_whole_intent and e.locks & intent.locks)):
            continue
        end = hold_end(cfg, j, e)
        if end is None or i < end:
            if cfg.auto_resume_on_release and end is not None:
                continue
            return True
    return False


def intent_stopped(cfg, j, i, intent):
    for k, e in enumerate(j.entries):
        if e.kind == "void" and e.id == intent.id:
            return True
        if k > i and e.kind == "intent" and (e.locks & intent.locks if cfg.newer_intent_by_locks else e.subject == intent.subject):
            return True
        if k > i and e.kind == "refusal" and not cfg.ignore_refusals:
            if e.locks & intent.locks if cfg.refusal_by_locks else e.subject == intent.subject:
                return True
    return not cfg.no_generation_check and intent.gen < gen_of(j, i)


def intent_targets(cfg, db, j, i, e, linked, unproven):
    """The subscopes of an unconfirmed intent that may still execute."""
    if cfg.forward_uses_current_links:
        target = bindings_of(e.subject, linked)
    elif cfg.no_scope_shrink:
        target = e.binds
    else:
        later = frozenset().union(*[x.binds for k, x in enumerate(j.entries) if x.kind == "unlink" and k > i] or [frozenset()])
        target = e.binds - later - (e.binds - linked - db.erased)
    if not cfg.forward_ignores_separation and not (cfg.resume_stale_scope and e.auth):
        target -= unproven
    return frozenset(b for b in target if not hold_blocks(cfg, j, i, e, ITEMS[b]))


def replay(cfg, db, j, defer=False):
    """Roll forward. With defer, unconfirmed intents are left pending."""
    linked, erased, unproven = db.linked, db.erased, db.unproven
    holds = active_holds(cfg, j)
    for e in j.entries:
        if e.kind == "unlink":
            linked -= e.binds
        elif e.kind == "unprove":
            unproven |= e.binds
    forwarded = []
    for i, e in enumerate(j.entries):
        if e.kind != "intent":
            continue
        if confirmed(j, e.id):
            later = frozenset().union(*[x.binds for k, x in enumerate(j.entries) if x.kind == "unlink" and k > i] or [frozenset()])
            done = confirmed_binds(j, e.id)
            erased |= done - later - (done - linked - db.erased)
        elif not defer and not intent_stopped(cfg, j, i, e):
            target = intent_targets(cfg, db, j, i, e, linked, unproven)
            if target - erased:
                forwarded.append((i, e, target - erased))
                erased |= target
    return db._replace(linked=linked, erased=erased, holds=holds, unproven=unproven), forwarded


def needs_reconcile(cfg, db, j, locks=None):
    new, _ = replay(cfg, db, j)
    if locks is None:
        return new.erased != db.erased or new.holds != db.holds or new.linked != db.linked or new.unproven != db.unproven
    return bool(items_of(new.erased ^ db.erased) & locks or (new.holds ^ db.holds) & locks or items_of(new.linked ^ db.linked) & locks)


def anchors(entries):
    return frozenset((e.id, e.kind) for e in entries)


def ack(db, entries):
    return db._replace(anchor=db.anchor | anchors(entries))


def heartbeat_ok(cfg, db, j):
    if cfg.no_heartbeat_check:
        return True
    for position, e in enumerate(j.entries, start=1):
        if e.kind == "heartbeat" and position > db.hb and (e.prev != db.hb if cfg.heartbeat_strict_prev else e.prev > db.hb):
            return False
    return True


def verified(cfg, s):
    db, j = s.db, s.journal
    if db.isolated or db.gen != gen_of(j) or not db.anchor <= anchors(j.entries):
        return False
    if not cfg.timeline_reference_in_db and db.timeline != timeline_of(j):
        return False
    if not cfg.recovery_ignores_witness and s.witness & db.linked:
        return False
    return heartbeat_ok(cfg, db, j)


def can_dispatch(cfg, s):
    return s.journal.up and s.activation and verified(cfg, s) and not needs_reconcile(cfg, s.db, s.journal)


def begin(cfg, s, i, op, subject, locks, binds, withheld, auth, budget_name, id_=None):
    n = spend(s, budget_name)
    if not n:
        return None
    w = Worker(op, "append", s.db.gen, id_ or f"{op}{s.next_id}", head(s.journal), subject, locks, binds, withheld, auth, True, False, head(s.journal))
    return put(n._replace(next_id=s.next_id + 1), i, w)


def separated(db, j, linked):
    return frozenset(b for b in linked if separation_lost(db, j, b))


def resolved(cfg, db, j, k):
    e = j.entries[k]
    if e.kind == "intent":
        if position_of(j, "void", e.id) is not None or intent_stopped(cfg, j, k, e):
            return True
        if confirmed(j, e.id):
            return not (confirmed_binds(j, e.id) & db.linked - db.erased)
        return not (intent_targets(cfg, db, j, k, e, db.linked, db.unproven) - db.erased)
    if e.kind == "unlink":
        return not (e.binds & db.linked)
    return True


def begin_erasures(cfg, s, i, jgen):
    db, j = s.db, s.journal
    busy = held_items(s)
    for subject, name in (("A", "erase_a"), ("B", "erase_b"), ("C", "erase_c")):
        live = bindings_of(subject, db.linked) - db.erased
        unproven = frozenset() if cfg.treat_unproven_as_separable else separated(db, j, live)
        protected = frozenset(b for b in live if ITEMS[b] in db.holds or (was_blocked(j, b) and not cfg.resume_without_authorization))
        binds = live - unproven - protected
        locks = items_of(binds)
        gate = cfg.act_before_reconcile or not needs_reconcile(cfg, db, j, locks)
        if cfg.hold_blocks_whole_intent and items_of(live) & db.holds:
            continue
        if binds and not (locks & busy) and gate:
            n = begin(cfg, s, i, "erase", subject, locks, binds, live - binds, False, name)
            if n:
                yield (f"erase{subject}.begin", n._replace(ghost=n.ghost._replace(aborted=n.ghost.aborted - {subject})))


def begin_controls(cfg, s, i, busy):
    db, j = s.db, s.journal
    items = cfg.hold_items
    if not (items <= active_holds(cfg, j)) and (cfg.hold_without_locks or not (items & busy)):
        n = begin(cfg, s, i, "hold", cfg.hold_owner, items, frozenset(), frozenset(), False, "hold")
        if n:
            yield ("hold.begin", put(n, i, n.workers[i]._replace(holding=not cfg.hold_without_locks)))
    for e in j.entries:
        if e.kind == "hold" and hold_end(cfg, j, e) is None and not (e.locks & busy):
            n = begin(cfg, s, i, "release", e.subject, e.locks, frozenset(), frozenset(), False, "release", id_=e.id)
            if n:
                resume_at_commit = position_of(j, "release", e.id) is not None
                yield ("release.begin", put(n, i, n.workers[i]._replace(pc="commit" if resume_at_commit else "append")))
    for subject in SUBJECTS:
        open_intents = [e for k, e in enumerate(j.entries) if e.kind == "intent" and e.subject == subject and not intent_stopped(cfg, j, k, e) and not confirmed(j, e.id)]
        if open_intents and not (open_intents[-1].locks & busy):
            n = begin(cfg, s, i, "refuse", subject, open_intents[-1].locks, frozenset(), frozenset(), False, "refuse")
            if n:
                yield (f"refuse{subject}.begin", n)


def begin_resumes(cfg, s, i, busy):
    db, j = s.db, s.journal
    for subject in SUBJECTS:
        live = bindings_of(subject, db.linked) - db.erased
        if not any(was_blocked(j, b) for b in live):
            continue
        if cfg.resume_stale_scope:
            intent = [e for e in j.entries if e.kind == "intent" and e.subject == subject][-1]
            binds = intent.binds | intent.withheld
            binds = frozenset(b for b in binds if ITEMS[b] not in active_holds(cfg, j))
        else:
            binds = frozenset(b for b in live - separated(db, j, live) if ITEMS[b] not in active_holds(cfg, j))
        locks = items_of(binds)
        if binds and not (locks & busy) and not needs_reconcile(cfg, db, j, locks):
            n = begin(cfg, s, i, "erase", subject, locks, binds, frozenset(), True, "resume")
            if n:
                yield (f"controller.authorize_resume{subject}", n)


def step_worker(cfg, s, i, w, jgen):
    db, j, g = s.db, s.journal, s.ghost
    kind = {"erase": "intent", "hold": "hold", "release": "release", "refuse": "refusal"}[w.op]
    entry = Entry(w.id, kind, w.gen, w.subject, w.locks, w.binds, w.withheld, w.auth)
    if w.pc == "append":
        if j.up and position_of(j, kind, w.id) is not None:
            yield (f"{w.id}.already_appended", put(s, i, w._replace(pc="commit")))  # appends are idempotent by event ID
        elif j.up:
            moved = gen_of(j) != gen_of(j, min(w.verified_upto, head(j)))
            if moved and not cfg.no_generation_check:
                yield (f"{w.id}.abort_generation", drop(s, i))
            elif w.expected == head(j) or cfg.no_expected_position:
                yield (f"{w.id}.append", put(s._replace(journal=j._replace(entries=j.entries + (entry,))), i,
                       w._replace(pc="commit", expected=head(j) + 1)))
            elif w.op == "erase" and not cfg.no_rescan_on_conflict and any(
                    (e.kind == "hold" and e.locks & w.locks) or (e.kind == "refusal" and e.subject == w.subject) for e in j.entries[w.expected:]):
                yield (f"{w.id}.blocked_by_decision", drop(s, i))
            else:
                yield (f"{w.id}.reread", put(s, i, w._replace(expected=head(j))))
            if s.late is None:
                n = spend(s, "timeout")
                if n:
                    yield (f"{w.id}.timeout", put(n._replace(late=(w.expected, entry)), i, w._replace(pc="unknown")))
        else:
            yield (f"{w.id}.not_applied", drop(s, i))
    elif w.pc == "unknown":
        if j.up and position_of(j, kind, w.id) is not None:
            yield (f"{w.id}.found", put(s, i, w._replace(pc="commit")))
        elif j.up:
            yield (f"{w.id}.reported_unknown", drop(s, i))
    elif w.pc == "commit":
        if w.zombie:
            if s.old is None or s.old_stopped:
                yield (f"{w.id}.fenced", drop(s, i))
            elif w.op == "erase":
                yield (f"{w.id}.commit_on_old", put(s._replace(old=s.old._replace(erased=s.old.erased | w.binds),
                       ghost=g._replace(committed=g.committed | w.binds)), i, w._replace(pc="confirm", holding=False)))
            elif w.op == "hold":
                yield (f"{w.id}.commit_on_old", drop(s._replace(old=s.old._replace(holds=s.old.holds | w.locks)), i))
            else:
                yield (f"{w.id}.fenced", drop(s, i))
            return
        if db.isolated or db.gen != w.gen:
            yield (f"{w.id}.fenced", drop(s, i))
            return
        acked = ack(db, j.entries)
        if w.op == "hold":
            yield (f"{w.id}.commit", drop(s._replace(db=acked._replace(holds=db.holds | w.locks)), i))
        elif w.op == "refuse":
            yield (f"{w.id}.commit", drop(s._replace(db=acked), i))
        elif w.op == "release":
            remaining = frozenset().union(*[e.locks for e in j.entries if e.kind == "hold" and e.id != w.id and hold_end(cfg, j, e) is None] or [frozenset()])
            yield (f"{w.id}.commit", put(s._replace(db=acked._replace(holds=(db.holds - w.locks) | remaining)), i, w._replace(pc="confirm")))
        else:
            position = position_of(j, "intent", w.id)
            position = head(j) if position is None else position
            target = w.binds & (db.linked | db.erased) if not cfg.no_scope_shrink else w.binds
            if not cfg.forward_ignores_separation and not (cfg.resume_stale_scope and w.auth) and not cfg.treat_unproven_as_separable:
                target -= separated(db, j, target)
            intent = Entry(w.id, "intent", w.gen, w.subject, w.locks, w.binds, w.withheld, w.auth)
            problem = execution_fault(s, position, target, intent)
            ghost = fault(g, problem) if problem else g
            db_after = acked._replace(erased=db.erased | target)
            if cfg.watermark_on_commit:
                db_after = db_after._replace(applied=max(db.applied, position + 1))
            yield (f"{w.id}.commit", put(s._replace(db=db_after, ghost=ghost._replace(committed=ghost.committed | target)), i, w._replace(pc="confirm", holding=False, binds=target)))
            n = spend(s, "abort")
            if n and j.up:
                void = Entry(w.id, "void", w.gen, w.subject, w.locks, w.binds)
                yield (f"{w.id}.abort_voided", drop(n._replace(journal=j._replace(entries=j.entries + (void,)),
                       ghost=n.ghost._replace(aborted=n.ghost.aborted | {w.subject})), i))
    elif w.pc == "confirm":
        confirm_kind = "release_confirm" if w.op == "release" else "confirm"
        if j.up and w.gen == jgen and not w.zombie:
            yield (f"{w.id}.confirm", drop(s._replace(journal=j._replace(entries=j.entries + (Entry(w.id, confirm_kind, w.gen, w.subject, binds=w.binds),))), i))
        else:
            yield (f"{w.id}.confirm_lost", drop(s, i))


def step_sweeper(cfg, s, ok, busy, jgen):
    db, j, g = s.db, s.journal, s.ghost
    if s.sweep:
        phase, id_, targets, locks = s.sweep
        if phase == "execute":
            position = position_of(j, "intent", id_)
            intent = j.entries[position]
            problem = execution_fault(s, position, targets, intent)
            yield ("sweep.execute", s._replace(db=ack(db._replace(erased=db.erased | targets), j.entries), sweep=("confirm", id_, targets, locks),
                   ghost=fault(g, problem) if problem else g))
        elif j.up:
            yield ("sweep.confirm", s._replace(journal=j._replace(entries=j.entries + (Entry(id_, "confirm", jgen, binds=targets),)), sweep=None))
        else:
            yield ("sweep.confirm_lost", s._replace(sweep=None))
        return
    if not (ok and j.up and needs_reconcile(cfg, db, j)):
        return
    facts, _ = replay(cfg, db, j, defer=True)
    if not busy and (facts.holds != db.holds or facts.linked != db.linked or facts.unproven != db.unproven or facts.erased != db.erased):
        yield ("sweep.facts", s._replace(db=ack(facts, j.entries)))
    if not s.activation:
        return
    if cfg.void_on_absence:
        unresolved = [e for e in j.entries if e.kind == "intent" and not any(x.kind in ("void", "confirm") and x.id == e.id for x in j.entries)]
        if unresolved and not busy:
            yield ("sweep.void", s._replace(journal=j._replace(entries=j.entries + (Entry(unresolved[0].id, "void", jgen, unresolved[0].subject),))))
            return
    _, forwarded = replay(cfg, db, j)
    if forwarded and facts == db:
        position, e, targets = forwarded[0]
        locks = frozenset({max(e.locks)}) if cfg.partial_replay_locks else e.locks
        if locks & busy:
            return
        entries = j.entries if cfg.no_forward_claim else j.entries + (Entry(e.id, "forward", jgen),)
        yield ("sweep.claim", s._replace(journal=j._replace(entries=entries), sweep=("execute", e.id, targets, locks)))


def step_commit_first(cfg, s, i):
    db = s.db
    busy = held_items(s)
    for b in sorted(db.linked - db.erased):
        if ITEMS[b] in busy or db.isolated:
            continue
        n = spend(s, "unlink")
        if n:
            ghost = n.ghost._replace(acked=n.ghost.acked | {b}, journaled=n.ghost.journaled | ({b} if cfg.journaled_on_commit else frozenset()))
            after = db if cfg.ack_without_suppression else db._replace(linked=db.linked - {b}, unjournaled=db.unjournaled | {b})
            yield (f"unlink.{b}", n._replace(db=after, ghost=ghost))
        n = spend(s, "share")
        if n and not separation_lost(db, s.journal, b):
            yield (f"share.{b}", n._replace(db=db._replace(unproven=db.unproven | {b}, unproven_queue=db.unproven_queue | {b}),
                   ghost=n.ghost._replace(ever_unproven=n.ghost.ever_unproven | {b})))


def step_journal_flush(cfg, s, ok, jgen):
    db, j, g = s.db, s.journal, s.ghost
    if s.late:
        expected, entry = s.late
        if j.up and (expected == head(j) or cfg.no_expected_position):
            yield ("late_append.lands", s._replace(journal=j._replace(entries=j.entries + (entry,)), late=None))
        elif j.up:
            yield ("late_append.rejected", s._replace(late=None))
    if db.unjournaled and j.up and ok:
        b = sorted(db.unjournaled)[0]
        known = any(e.kind == "unlink" and e.binds == frozenset({b}) for e in j.entries)
        entries = j.entries if known and not cfg.non_idempotent_flush else j.entries + (Entry(f"unlink-{b}", "unlink", jgen, SUBJECT[b], frozenset({ITEMS[b]}), frozenset({b})),)
        yield (f"flush.{b}", s._replace(journal=j._replace(entries=entries), db=ack(db._replace(unjournaled=db.unjournaled - {b}), entries),
               ghost=g._replace(journaled=g.journaled | {b})))
    if db.unproven_queue and j.up and ok:
        b = sorted(db.unproven_queue)[0]
        known = any(e.kind == "unprove" and b in e.binds for e in j.entries)
        entries = j.entries if known else j.entries + (Entry(f"unprove-{b}", "unprove", jgen, SUBJECT[b], frozenset({ITEMS[b]}), frozenset({b})),)
        yield (f"flush_unprove.{b}", s._replace(journal=j._replace(entries=entries), db=ack(db._replace(unproven_queue=db.unproven_queue - {b}), entries)))
    if s.witness_up and not db.isolated:
        for b in sorted(g.acked - s.witness - db.linked):
            n = spend(s, "witness")
            if n:
                yield (f"witness.put.{b}", n._replace(witness=s.witness | {b}, ghost=n.ghost._replace(witnessed=n.ghost.witnessed | {b})))


def step_heartbeat(cfg, s, ok, jgen):
    db, j, g = s.db, s.journal, s.ghost
    n = spend(s, "heartbeat")
    if n and j.up and ok and s.hb_flight is None:
        beat = j.entries + (Entry(f"hb{s.next_id}", "heartbeat", jgen, prev=db.hb),)
        yield ("heartbeat.append", n._replace(journal=j._replace(entries=beat), next_id=s.next_id + 1, hb_flight=head(j) + 1))
    n = spend(s, "heartbeat")
    if n and j.up and db.hb:  # an abandoned heartbeat landing late; its transaction is gone, so it never commits
        yield ("heartbeat.append_delayed", n._replace(journal=j._replace(entries=j.entries + (Entry(f"hb{s.next_id}", "heartbeat", jgen, prev=0),)), next_id=s.next_id + 1))
    if db.isolated or db.gen != gen_of(j) or s.hb_flight is None:
        return
    position = s.hb_flight
    if cfg.heartbeat_no_cas or j.entries[position - 1].prev == db.hb:
        yield ("heartbeat.commit", s._replace(db=ack(db._replace(hb=position), j.entries[:position]), hb_flight=None,
               ghost=g._replace(covered_at=g.covered_at | frozenset((b, position) for b in g.acked - db.linked))))
    else:
        yield ("heartbeat.cas_failed", s._replace(hb_flight=None))


def step_restore(cfg, s, jgen):
    db, j, g = s.db, s.journal, s.ghost
    if s.old is not None and not s.old_stopped and s.restore:
        for b in sorted(s.old.linked - s.old.erased):
            n = spend(s, "old_write")
            if n:
                yield (f"old_primary.unlink.{b}", n._replace(old=s.old._replace(linked=s.old.linked - {b}, unjournaled=s.old.unjournaled | {b}),
                       ghost=n.ghost._replace(late_lost=n.ghost.late_lost | {b}, acked=n.ghost.acked | {b})))
    if s.restore is None:
        for snap in s.snaps:
            n = spend(s, "restore")
            if n:
                zombies = tuple(w._replace(zombie=True, holding=False) if w else None for w in n.workers)
                yield ("restore.load_isolated", n._replace(db=snap._replace(isolated=not cfg.no_isolation), old=db, old_stopped=False, restore="loaded", workers=zombies, sweep=None, hb_flight=None,
                       ghost=n.ghost._replace(restored=True)))
    elif s.restore == "loaded":
        if not s.old_stopped:
            yield ("restore.stop_old_primary", s._replace(old_stopped=True))
            if j.up:
                yield ("restore.cutover_old_unreachable", s._replace(journal=j._replace(entries=j.entries + (Entry(f"g{jgen + 1}", "gen", jgen + 1),)), restore="cut"))
        elif j.up:
            pending = sorted(s.old.unjournaled - frozenset(b for e in j.entries if e.kind == "unlink" for b in e.binds)) if s.old else []
            if pending and not cfg.no_salvage:
                b = pending[0]
                yield (f"restore.salvage.{b}", s._replace(journal=j._replace(entries=j.entries + (Entry(f"unlink-{b}", "unlink", jgen, SUBJECT[b], frozenset({ITEMS[b]}), frozenset({b})),)),
                       ghost=g._replace(journaled=g.journaled | {b}, late_lost=g.late_lost - {b})))
            else:
                yield ("restore.cutover", s._replace(journal=j._replace(entries=j.entries + (Entry(f"g{jgen + 1}", "gen", jgen + 1),)), restore="cut",
                       ghost=g._replace(salvage_skipped=bool(pending))))
    elif s.restore == "cut" and j.up:
        new, forwarded = replay(cfg, db, j, defer=not cfg.restore_forwards_before_window)
        ghost = g
        if forwarded and not cfg.activation_stays_open:
            ghost = fault(ghost, "unresolved intent executed before the window decision")
        new = new._replace(linked=new.linked - s.witness, unjournaled=new.unjournaled | (s.witness & db.linked))
        yield ("restore.apply", s._replace(db=ack(new._replace(gen=jgen, isolated=False, hb=head(j), timeline=timeline_of(j)), j.entries), restore=None,
               activation=cfg.activation_stays_open, ghost=ghost))
    if not s.activation and not s.restore and not db.isolated:
        yield ("controller.accept_window", s._replace(activation=True, ghost=g._replace(accepted_losses=g.acked)))


def step_faults(cfg, s, ok, jgen):
    db, j, g = s.db, s.journal, s.ghost
    if any(s.workers) or s.sweep or s.hb_flight:
        n = spend(s, "crash")
        if n:
            yield ("crash", n._replace(workers=(None,) * len(s.workers), sweep=None, hb_flight=None))
    n = spend(s, "snap")
    if n and not db.isolated:
        yield ("snapshot", n._replace(snaps=n.snaps + (db,)))
    n = spend(s, "outage")
    if n:
        yield ("journal.toggle", n._replace(journal=j._replace(up=not j.up)))
    n = spend(s, "witness_outage")
    if n:
        yield ("witness.toggle", n._replace(witness_up=not s.witness_up))
    n = spend(s, "failover_ok")
    if n and ok and j.up:
        entries = j.entries if cfg.lossless_failover_unrecorded else j.entries + (Entry(f"tl{s.next_id}", "timeline", jgen),)
        yield ("failover.lossless", n._replace(journal=j._replace(entries=entries), db=db._replace(timeline=db.timeline + 1), next_id=s.next_id + 1))
    for snap in s.snaps:
        n = spend(s, "failover_bad")
        if n:
            yield ("failover.lossy", n._replace(db=snap._replace(timeline=db.timeline + 1), workers=(None,) * len(s.workers), sweep=None, hb_flight=None,
                   ghost=n.ghost._replace(timeline_lost=True)))
        n = spend(s, "undeclared")
        if n:
            witness = frozenset() if cfg.witness_in_db_domain else s.witness
            flight = s.hb_flight if cfg.heartbeat_no_cas else None  # a rollback ends every open transaction
            yield ("undeclared_rollback", n._replace(db=snap, workers=(None,) * len(s.workers), sweep=None, hb_flight=flight, witness=witness, ghost=n.ghost._replace(undeclared=True)))
    n = spend(s, "advance")
    if n and ok and db.applied < head(j) and resolved(cfg, db, j, db.applied):
        yield ("sweeper.advance_watermark", n._replace(db=db._replace(applied=db.applied + 1)))
    n = spend(s, "prune")
    if n and j.up:
        pins = [db.applied] + [snap.applied for snap in s.snaps]
        for k, e in enumerate(j.entries):
            if not resolved(cfg, db, j, k) or (e.kind == "hold" and hold_end(cfg, j, e) is None):
                pins.append(k)
        horizon = min(pins)
        if horizon > j.base:
            yield ("journal.prune", n._replace(journal=j._replace(base=horizon)))


def successors(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    jgen = gen_of(j)
    ok = verified(cfg, s)
    busy = held_items(s)
    for i, w in enumerate(s.workers):
        if w is not None:
            yield from step_worker(cfg, s, i, w, jgen)
            continue
        if ok and j.up:
            if s.activation:
                yield from begin_erasures(cfg, s, i, jgen)
                yield from begin_resumes(cfg, s, i, busy)
            yield from begin_controls(cfg, s, i, busy)
            if "b1" not in db.linked and "b1" not in db.erased and 1 not in busy and 1 not in db.holds and not any(
                    e.kind == "intent" and e.subject == "B" and not intent_stopped(cfg, j, k, e) for k, e in enumerate(j.entries)):
                n = spend(s, "link")
                if n:
                    yield ("linkB1", n._replace(db=db._replace(linked=db.linked | {"b1"}), ghost=n.ghost._replace(acked=n.ghost.acked - {"b1"})))
        yield from step_commit_first(cfg, s, i)
    yield from step_sweeper(cfg, s, ok, busy, jgen)
    yield from step_journal_flush(cfg, s, ok, jgen)
    yield from step_heartbeat(cfg, s, ok, jgen)
    yield from step_faults(cfg, s, ok, jgen)
    yield from step_restore(cfg, s, jgen)


def violations(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    dispatch = can_dispatch(cfg, s)
    if g.fault:
        yield g.fault
    named = frozenset().union(*[e.binds for e in j.entries if e.kind == "intent"] or [frozenset()])
    if db.erased - named - g.committed:
        yield "erased a binding that no intent named"
    for b in sorted(db.erased):
        unlinked = any(e.kind == "unlink" and b in e.binds and any(x.kind == "intent" and b in x.binds and j.entries.index(x) < j.entries.index(e) for x in j.entries) for e in j.entries)
        if unlinked and b not in g.committed:
            yield "forward erased a binding unlinked after the intent"
    for b in sorted(g.committed - db.erased):
        if b not in db.linked or not dispatch:
            continue
        intents = [(k, e) for k, e in enumerate(j.entries) if e.kind == "intent" and b in e.binds]
        was_confirmed = any(b in confirmed_binds(j, e.id) for _, e in intents)
        overruled = any(oracle_hold_forbids(j, k, ITEMS[b]) or oracle_overruled(j, k, e) or separation_lost(db, j, b) for k, e in intents)
        if was_confirmed or not overruled or (cfg.check_hold_tradeoff and g.undeclared):
            yield "dispatch on a database that lost a committed erasure"
    if dispatch and g.journaled & (db.linked - db.unjournaled):
        yield "dispatch after losing a journaled write"
    if dispatch and g.witnessed & db.linked:
        yield "dispatch after losing a witnessed write"
    unjournaled_loss = (g.acked - g.journaled) & db.linked
    # A rollback is detectable once a later heartbeat in the log names the committed position that covered the write.
    named = frozenset(e.prev for e in j.entries if e.kind == "heartbeat")
    covered = frozenset(b for b, position in g.covered_at if position in named)
    if dispatch and unjournaled_loss - g.accepted_losses and (cfg.check_undetectable or not g.undeclared or unjournaled_loss & covered or g.timeline_lost):
        yield "dispatch after losing an acknowledged write without an accepted window"
    if g.acked & db.linked and not (g.undeclared or g.restored or g.timeline_lost):
        yield "acknowledged a write that is not in effect"
    if cfg.check_separation_rollback and db.erased & g.ever_unproven - g.committed:
        yield "erased a binding whose loss of separation a rollback had hidden"
    if g.salvage_skipped:
        yield "cutover abandoned acknowledged writes on a reachable old primary"
    ids = [(e.id, e.kind) for e in j.entries if e.kind != "forward"]
    unlinks = [e for e in j.entries if e.kind == "unlink"]
    if len(ids) != len(set(ids)) or len(unlinks) != len({b for e in unlinks for b in e.binds}):
        yield "duplicate journal entry"
    for k in range(j.base):
        e = j.entries[k]
        if e.kind == "intent" and not (position_of(j, "void", e.id) is not None or oracle_stopped(j, k, e)):
            for snap in s.snaps + (db,):
                if (confirmed_binds(j, e.id) - snap.erased) & snap.linked:
                    yield "retention deleted an entry a restorable backup still needs"
        if e.kind == "hold" and position_of(j, "release_confirm", e.id) is None:
            yield "retention deleted an unreleased hold"
    idle = not any(s.workers) and not s.sweep and not s.restore and not s.late and s.activation
    wedge_free = not (g.undeclared or g.restored or g.timeline_lost)
    if wedge_free and not db.isolated and not verified(cfg, s):
        yield "recovery check stopped a deployment with no rollback"
    if idle and j.up and verified(cfg, s) and not g.undeclared and not needs_reconcile(cfg, db, j):
        for i, e in enumerate(j.entries):
            if e.kind != "intent" or confirmed(j, e.id) or oracle_stopped(j, i, e):
                continue
            later = frozenset().union(*[x.binds for k, x in enumerate(j.entries) if x.kind == "unlink" and k > i] or [frozenset()])
            for b in sorted(e.binds - db.erased - later):
                if b in db.linked and not oracle_hold_forbids(j, i, ITEMS[b]) and not separation_lost(db, j, b):
                    yield "an independent subscope was left unerased by another subject's hold or refusal"


def explore(cfg, limit=2_500_000):
    start = State(Db(linked=cfg.initial, unproven=cfg.unproven), Journal(), frozenset(), True, (None, None), None, None, None, (), None, False, None, True, Ghost(), 1, cfg.budget)
    seen = {start}
    queue = deque([start])
    parents = {start: None}
    while queue:
        s = queue.popleft()
        for v in violations(cfg, s):
            path, cur = [], s
            while parents[cur] is not None:
                label, prev = parents[cur]
                path.append(label)
                cur = prev
            return len(seen), v, list(reversed(path))
        for label, n in successors(cfg, s):
            if n not in seen:
                if len(seen) >= limit:
                    raise LimitReached()
                seen.add(n)
                parents[n] = (label, s)
                queue.append(n)
    return len(seen), None, []


def budget(**kw):
    return tuple(kw.get(name, 0) for name in BUDGET_NAMES)


NO_B1 = frozenset({"a0", "a1", "b2"})
ALL = frozenset({"a0", "a1", "b1", "b2"})
CHAIN = ALL | {"c2", "c3"}
SHARED = frozenset({1})
OF_A = frozenset({0, 1})
SCENARIOS = {
    "concurrent intersecting erasures":   dict(b=budget(erase_a=1, erase_b=1, crash=2, abort=1)),
    "hold on the shared profile":         dict(b=budget(erase_a=1, erase_b=1, hold=1, crash=2)),
    "hold for A splits B's erasure":      dict(b=budget(erase_a=1, erase_b=1, hold=1, crash=2), hold=OF_A),
    "unproven separation stays protected": dict(b=budget(erase_b=1, hold=1, crash=1), hold=OF_A, unproven=frozenset({"b2"})),
    "separation lost after the intent":   dict(b=budget(erase_b=1, share=1, crash=1, snap=1, undeclared=1)),
    "cross-scope refusal":                dict(b=budget(erase_a=1, erase_b=1, refuse=1, crash=2)),
    "journal append races":               dict(b=budget(erase_a=1, erase_b=1, hold=1, crash=1, timeout=1)),
    "late hold against a roll-forward":   dict(b=budget(erase_b=1, hold=1, crash=1, timeout=1)),
    "unlink around erasure":              dict(b=budget(erase_a=1, erase_b=1, unlink=1, crash=2)),
    "link around erasure":                dict(b=budget(erase_a=1, erase_b=1, link=1, crash=2), initial=NO_B1),
    "three-subject chain":                dict(b=budget(erase_a=1, erase_b=1, erase_c=1, hold=1, crash=1), initial=CHAIN),
    "hold release does not resume":       dict(b=budget(erase_b=2, hold=1, release=1, resume=1, crash=1)),
    "release crash windows":              dict(b=budget(erase_b=1, hold=1, release=2, crash=2, timeout=1)),
    "resume needs a fresh scope":         dict(b=budget(erase_b=1, hold=1, release=1, resume=1, unlink=1, share=1)),
    "rollback with hold":                 dict(b=budget(erase_a=1, erase_b=1, hold=1, snap=1, undeclared=1, crash=1)),
    "rollback and heartbeat":             dict(b=budget(unlink=1, snap=1, undeclared=1, heartbeat=3)),
    "rollback of an in-doubt erasure":    dict(b=budget(erase_a=1, snap=1, undeclared=1, crash=1, heartbeat=1)),
    "heartbeat crash does not wedge":     dict(b=budget(heartbeat=3)),
    "restore across scopes":              dict(b=budget(erase_a=1, erase_b=1, hold=1, snap=1, restore=1, crash=1)),
    "restore with unlink":                dict(b=budget(erase_a=1, unlink=1, snap=1, restore=1, crash=1)),
    "restore window and pending intent":  dict(b=budget(erase_b=1, hold=1, snap=1, restore=1, crash=1), hold=SHARED),
    "old primary writes during restore":  dict(b=budget(erase_a=1, unlink=1, snap=1, restore=1, old_write=2)),
    "journal outage and witness":         dict(b=budget(unlink=1, outage=2, witness=1, witness_outage=1, snap=1, crash=0)),
    "rollback with witness":              dict(b=budget(unlink=1, witness=1, snap=1, undeclared=1, heartbeat=1)),
    "restore with witness":               dict(b=budget(unlink=1, witness=1, snap=1, restore=1, outage=1)),
    "lossless and lossy failover":        dict(b=budget(unlink=1, snap=1, failover_ok=1, failover_bad=1)),
    "retention watermark":                dict(b=budget(erase_a=1, erase_b=1, hold=1, snap=1, crash=1, advance=4, prune=1)),
}

MUTANTS = {
    "old writer re-reads the head": ("no_generation_check", ["restore across scopes"]),
    "holds matched by subject": ("match_hold_by_subject", ["hold on the shared profile"]),
    "only later holds block, without reconcile-first": ({"later_holds_only": True, "act_before_reconcile": True}, ["rollback with hold", "hold on the shared profile"]),
    "a hold blocks the whole intent": ("hold_blocks_whole_intent", ["hold on the shared profile", "hold for A splits B's erasure"]),
    "unproven separation treated as separable": ("treat_unproven_as_separable", ["unproven separation stays protected"]),
    "forward ignores lost separation": ("forward_ignores_separation", ["separation lost after the intent"]),
    "append without expected position": ("no_expected_position", ["journal append races"]),
    "no rescan on position conflict": ("no_rescan_on_conflict", ["journal append races"]),
    "forward ignores a later unlink": ("no_scope_shrink", ["unlink around erasure"]),
    "forward uses current links": ("forward_uses_current_links", ["link around erasure"]),
    "void an intent because it is absent": ("void_on_absence", ["rollback of an in-doubt erasure", "rollback with hold"]),
    "restore without isolation": ("no_isolation", ["restore with unlink", "restore across scopes"]),
    "activation open after restore": ("activation_stays_open", ["restore with unlink", "old primary writes during restore"]),
    "restore forwards before the window": ("restore_forwards_before_window", ["restore window and pending intent", "restore across scopes"]),
    "restore without salvage": ("no_salvage", ["old primary writes during restore"]),
    "hold without scope locks": ("hold_without_locks", ["hold on the shared profile"]),
    "act on an unreconciled scope": ("act_before_reconcile", ["rollback with hold", "concurrent intersecting erasures"]),
    "roll forward without a claim": ("no_forward_claim", ["late hold against a roll-forward"]),
    "replay under a partial lock set": ("partial_replay_locks", ["late hold against a roll-forward", "hold on the shared profile"]),
    "refusals ignored": ("ignore_refusals", ["cross-scope refusal"]),
    "refusal matched by locks": ("refusal_by_locks", ["cross-scope refusal"]),
    "newer intent matched by locks": ("newer_intent_by_locks", ["rollback with hold", "restore across scopes"]),
    "release resumes blocked intents": ("auto_resume_on_release", ["hold release does not resume"]),
    "release effective before confirmation": ("release_unconfirmed", ["release crash windows", "hold release does not resume"]),
    "resume without authorization": ("resume_without_authorization", ["hold release does not resume"]),
    "resume on the stale scope": ("resume_stale_scope", ["resume needs a fresh scope"]),
    "no heartbeat position check": ("no_heartbeat_check", ["rollback and heartbeat"]),
    "heartbeat commit without compare-and-swap": ("heartbeat_no_cas", ["rollback and heartbeat"]),
    "heartbeat check rejects any older reference": ("heartbeat_strict_prev", ["heartbeat crash does not wedge"]),
    "non-idempotent flush": ("non_idempotent_flush", ["rollback and heartbeat", "restore with unlink"]),
    "timeline reference kept in the database": ("timeline_reference_in_db", ["lossless and lossy failover"]),
    "lossless failover not recorded": ("lossless_failover_unrecorded", ["lossless and lossy failover"]),
    "watermark advanced on commit": ("watermark_on_commit", ["retention watermark"]),
    "acknowledged without suppression": ("ack_without_suppression", ["journal outage and witness"]),
    "journaled claimed from a commit": ("journaled_on_commit", ["rollback with witness", "rollback and heartbeat"]),
    "recovery check ignores the witness": ("recovery_ignores_witness", ["rollback with witness"]),
    "witness inside the database domain": ("witness_in_db_domain", ["rollback with witness"]),
}

KNOWN_LIMITS = (
    ("check_undetectable", "rollback and heartbeat", "undeclared rollback can lose a pending_journal write"),
    ("check_hold_tradeoff", "rollback with hold", "a later hold outranks an unconfirmed erasure lost to an undeclared rollback"),
    ("check_separation_rollback", "separation lost after the intent", "a rollback can hide an unflushed loss of separation"),
)


def run(scenario, **flags):
    spec = SCENARIOS[scenario]
    return explore(Config(spec["b"], hold_items=spec.get("hold", SHARED), initial=spec.get("initial", ALL), unproven=spec.get("unproven", frozenset()), **flags))


def main():
    failed = False
    total = 0
    for name in SCENARIOS:
        try:
            states, violation, path = run(name)
        except LimitReached:
            print(f"BASE   {name}: INCOMPLETE, state limit reached")
            failed = True
            continue
        total += states
        print(f"BASE   {name}: {states} states, {'OK' if violation is None else 'VIOLATION: ' + violation}")
        if violation:
            print("       trace: " + " -> ".join(path))
            failed = True
    print(f"TOTAL  {total} states (bounded verification, not a proof)")
    for name, (flag, scenarios) in MUTANTS.items():
        caught = None
        for scenario in scenarios:
            try:
                _, violation, path = run(scenario, **(flag if isinstance(flag, dict) else {flag: True}))
            except LimitReached:
                continue
            if violation:
                caught = (scenario, violation, path)
                break
        if caught:
            print(f"MUTANT {name}: caught in '{caught[0]}': {caught[1]}")
            print("       trace: " + " -> ".join(caught[2]))
        else:
            print(f"MUTANT {name}: SURVIVED")
            failed = True
    for flag, scenario, label in KNOWN_LIMITS:
        _, violation, path = run(scenario, **{flag: True})
        if violation:
            print(f"LIMIT  {label}: {' -> '.join(path)}")
        else:
            print(f"LIMIT  expected witness missing for: {label}")
            failed = True
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
