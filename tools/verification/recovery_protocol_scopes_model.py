#!/usr/bin/env python3
"""Bounded model of ADR 0010 across two intersecting erasure scopes.

Bounded verification, not a proof. It enumerates interleavings within small budgets
and says nothing about behaviour outside them, or about real PostgreSQL, networks or
storage. Lock acquisition is modelled as atomic for a whole lock set, so lock-order
deadlocks are out of scope.

Subjects A and B each hold bindings on profiles. They intersect on profile 1. Each
mutant removes one rule, except one that removes a named pair, and must produce a
counterexample, printed as a full trace.
"""
from collections import deque
from typing import NamedTuple, Optional
import sys

ITEMS = {"a0": 0, "a1": 1, "b1": 1, "b2": 2}
SUBJECT = {"a0": "A", "a1": "A", "b1": "B", "b2": "B"}
SUBJECTS = ("A", "B")


def bindings_of(subject, linked):
    return frozenset(b for b in linked if SUBJECT[b] == subject)


def items_of(binds):
    return frozenset(ITEMS[b] for b in binds)


class Db(NamedTuple):
    linked: frozenset = frozenset({"a0", "a1", "b2"})
    erased: frozenset = frozenset()
    holds: frozenset = frozenset()
    unjournaled: frozenset = frozenset()  # commit-first unlinks awaiting the journal
    gen: int = 0
    isolated: bool = False
    anchor: frozenset = frozenset()
    beats: int = 0  # heartbeats committed to this database


class Entry(NamedTuple):
    id: str
    kind: str
    gen: int
    subject: Optional[str] = None
    locks: frozenset = frozenset()
    binds: frozenset = frozenset()


class Journal(NamedTuple):
    entries: tuple = ()
    up: bool = True


class Worker(NamedTuple):
    op: str
    pc: str
    gen: int
    id: str
    expected: int
    subject: Optional[str]
    locks: frozenset
    binds: frozenset
    holding: bool
    zombie: bool
    verified_upto: int


class Ghost(NamedTuple):
    committed: frozenset = frozenset()  # subjects whose erasure truly committed
    held: frozenset = frozenset()  # items under a committed hold
    aborted: frozenset = frozenset()  # subjects whose attempt observably rolled back
    acked_unlinks: frozenset = frozenset()
    journaled_unlinks: frozenset = frozenset()
    late_lost: frozenset = frozenset()  # acknowledged on an old primary after cutover
    accepted_losses: frozenset = frozenset()  # writes the controller recorded as lost
    undeclared: bool = False
    over_erasure: bool = False
    covered_once: frozenset = frozenset()  # writes included in one committed heartbeat
    covered: frozenset = frozenset()  # writes included in two, beyond the one-in-flight allowance
    salvage_skipped: bool = False
    max_beat: int = 0  # highest heartbeat ever committed; a heartbeat commits only its own position, once


class State(NamedTuple):
    db: Db
    journal: Journal
    workers: tuple
    late: Optional[tuple]
    snaps: tuple
    old: Optional[Db]
    old_stopped: bool
    restore: Optional[str]
    activation: bool
    ghost: Ghost
    next_id: int
    budget: tuple


BUDGET_NAMES = ("erase_a", "erase_b", "hold", "unlink", "link", "snap", "restore", "undeclared",
                "outage", "heartbeat", "crash", "timeout", "abort", "old_write")


class LimitReached(Exception):
    pass


class Config(NamedTuple):
    budget: tuple
    hold_item: int = 1
    initial: frozenset = frozenset({"a0", "a1", "b1", "b2"})
    no_generation_check: bool = False
    match_hold_by_subject: bool = False
    later_holds_only: bool = False
    no_expected_position: bool = False
    no_scope_shrink: bool = False
    forward_uses_current_links: bool = False
    void_on_absence: bool = False
    no_isolation: bool = False
    activation_stays_open: bool = False
    no_salvage: bool = False
    hold_without_locks: bool = False
    act_before_reconcile: bool = False
    no_heartbeat_check: bool = False
    non_idempotent_flush: bool = False
    no_rescan_on_conflict: bool = False
    check_undetectable: bool = False


def gen_of(j, upto=None):
    entries = j.entries if upto is None else j.entries[:upto]
    return sum(1 for e in entries if e.kind == "gen")


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


def held_items(s):
    out = frozenset()
    for w in s.workers:
        if w and w.holding:
            out |= w.locks
    return out


def blocked(cfg, j, i, intent):
    if any(e.kind == "void" and e.id == intent.id for e in j.entries):
        return True
    if any(k > i and e.kind == "intent" and e.subject == intent.subject for k, e in enumerate(j.entries)):
        return True
    if not cfg.no_generation_check and intent.gen < gen_of(j, i):
        return True
    for k, e in enumerate(j.entries):
        if e.kind != "hold":
            continue
        if cfg.later_holds_only and k < i:
            continue
        match = e.subject == intent.subject if cfg.match_hold_by_subject else bool(e.locks & intent.locks)
        if match:
            return True
    return False


def replay(cfg, db, j):
    linked, erased, holds = db.linked, db.erased, db.holds
    for i, e in enumerate(j.entries):
        if e.kind == "hold":
            holds |= e.locks
        elif e.kind == "unlink":
            linked -= e.binds
    forwarded = frozenset()
    for i, e in enumerate(j.entries):
        if e.kind != "intent":
            continue
        confirmed = any(x.kind == "confirm" and x.id == e.id for x in j.entries)
        if not confirmed and blocked(cfg, j, i, e):
            continue
        if cfg.forward_uses_current_links:
            target = bindings_of(e.subject, linked)
        elif cfg.no_scope_shrink:
            target = e.binds
        else:
            later = frozenset().union(*[x.binds for k, x in enumerate(j.entries) if x.kind == "unlink" and k > i] or [frozenset()])
            target = e.binds - later - (e.binds - linked - erased)
        if target - erased:
            if not confirmed:
                forwarded |= frozenset({(e.subject, e.locks)})
            erased |= target
    return db._replace(linked=linked, erased=erased, holds=holds), forwarded


def needs_reconcile(cfg, db, j, locks=None):
    new, _ = replay(cfg, db, j)
    if locks is None:
        return new.erased != db.erased or new.holds != db.holds or new.linked != db.linked
    return bool(items_of(new.erased ^ db.erased) & locks or (new.holds ^ db.holds) & locks or items_of(new.linked ^ db.linked) & locks)


def anchors(entries):
    return frozenset((e.id, e.kind) for e in entries)


def ack(db, entries):
    return db._replace(anchor=db.anchor | anchors(entries))


def beat_count(j):
    return sum(1 for e in j.entries if e.kind == "heartbeat")


def verified(cfg, s):
    db, j = s.db, s.journal
    if db.isolated or db.gen != gen_of(j) or not db.anchor <= anchors(j.entries):
        return False
    return cfg.no_heartbeat_check or beat_count(j) - db.beats <= 1


def can_dispatch(cfg, s):
    return s.journal.up and s.activation and verified(cfg, s) and not needs_reconcile(cfg, s.db, s.journal)


def forward_ghost(ghost, forwarded, db_before, cfg):
    for subject, locks in forwarded:
        if subject not in ghost.committed and (ghost.held & locks or subject in ghost.aborted):
            ghost = ghost._replace(over_erasure=True)
    return ghost


def begin(cfg, s, i, op, subject, locks, binds, budget_name):
    n = spend(s, budget_name)
    if not n:
        return None
    w = Worker(op, "append", s.db.gen, f"{op}{s.next_id}", head(s.journal), subject, locks, binds, True, False, head(s.journal))
    return put(n._replace(next_id=s.next_id + 1), i, w)


def successors(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    jgen = gen_of(j)
    ok = verified(cfg, s)
    busy = held_items(s)
    for i, w in enumerate(s.workers):
        if w is None:
            if ok and j.up:
                for subject, name in (("A", "erase_a"), ("B", "erase_b")):
                    binds = bindings_of(subject, db.linked) - db.erased
                    locks = items_of(bindings_of(subject, db.linked))
                    gate = cfg.act_before_reconcile or not needs_reconcile(cfg, db, j, locks)
                    if binds and not (locks & busy) and gate and not (locks & db.holds):
                        n = begin(cfg, s, i, "erase", subject, locks, binds, name)
                        if n:
                            yield (f"erase{subject}.begin", n._replace(ghost=n.ghost._replace(aborted=n.ghost.aborted - {subject})))
                item = frozenset({cfg.hold_item})
                if cfg.hold_item not in db.holds and (cfg.hold_without_locks or not (item & busy)):
                    owner = next(s2 for b, s2 in SUBJECT.items() if ITEMS[b] == cfg.hold_item and s2 == "B") if cfg.hold_item in (1, 2) else "A"
                    n = begin(cfg, s, i, "hold", owner, item, frozenset(), "hold")
                    if n:
                        yield ("hold.begin", put(n, i, n.workers[i]._replace(holding=not cfg.hold_without_locks)))
                if "b1" not in db.linked and "b1" not in db.erased and 1 not in busy and not any(
                        e.kind == "intent" and e.subject == "B" and not blocked(cfg, j, k, e) for k, e in enumerate(j.entries)):
                    n = spend(s, "link")
                    if n:
                        yield ("linkB1", n._replace(db=db._replace(linked=db.linked | {"b1"})))
            for b in sorted(db.linked - db.erased):
                if ITEMS[b] in busy or db.isolated:
                    continue
                n = spend(s, "unlink")
                if n:
                    yield (f"unlink.{b}", n._replace(db=db._replace(linked=db.linked - {b}, unjournaled=db.unjournaled | {b}),
                           ghost=n.ghost._replace(acked_unlinks=n.ghost.acked_unlinks | {b})))
            continue
        kind = {"erase": "intent", "hold": "hold"}[w.op]
        entry = Entry(w.id, kind, w.gen, w.subject, w.locks, w.binds)
        if w.pc == "append":
            if j.up:
                moved = gen_of(j) != gen_of(j, min(w.verified_upto, head(j)))
                if moved and not cfg.no_generation_check:
                    yield (f"{w.id}.abort_generation", drop(s, i))
                elif w.expected == head(j) or cfg.no_expected_position:
                    yield (f"{w.id}.append", put(s._replace(journal=j._replace(entries=j.entries + (entry,))), i,
                           w._replace(pc="commit", expected=head(j) + 1)))
                elif w.op == "erase" and not cfg.no_rescan_on_conflict and any(
                        e.kind == "hold" and e.locks & w.locks for e in j.entries[w.expected:]):
                    yield (f"{w.id}.blocked_by_hold", drop(s, i))
                else:
                    yield (f"{w.id}.reread", put(s, i, w._replace(expected=head(j))))
                if s.late is None:
                    n = spend(s, "timeout")
                    if n:
                        yield (f"{w.id}.timeout", put(n._replace(late=(w.expected, entry)), i, w._replace(pc="unknown")))
            else:
                yield (f"{w.id}.not_applied", drop(s, i))
        elif w.pc == "unknown":
            if j.up and any(e.id == w.id for e in j.entries):
                yield (f"{w.id}.found", put(s, i, w._replace(pc="commit")))
            elif j.up:
                yield (f"{w.id}.reported_unknown", drop(s, i))
        elif w.pc == "commit":
            target_old = w.zombie
            if target_old:
                if s.old is None or s.old_stopped:
                    yield (f"{w.id}.fenced", drop(s, i))
                    continue
                if w.op == "erase":
                    yield (f"{w.id}.commit_on_old", put(s._replace(old=s.old._replace(erased=s.old.erased | w.binds),
                           ghost=g._replace(committed=g.committed | {w.subject})), i, w._replace(pc="confirm", holding=False)))
                else:
                    yield (f"{w.id}.commit_on_old", drop(s._replace(old=s.old._replace(holds=s.old.holds | w.locks),
                           ghost=g._replace(held=g.held | w.locks)), i))
                continue
            if db.isolated or db.gen != w.gen:
                yield (f"{w.id}.fenced", drop(s, i))
                continue
            acked = ack(db, j.entries)
            if w.op == "hold":
                yield (f"{w.id}.commit", drop(s._replace(db=acked._replace(holds=db.holds | w.locks), ghost=g._replace(held=g.held | w.locks)), i))
            else:
                position = next((k for k, e in enumerate(j.entries) if e.id == w.id and e.kind == "intent"), head(j))
                earlier_hold = any(e.kind == "hold" and e.locks & w.locks for e in j.entries[:position])
                bad = g.over_erasure or bool(g.held & w.locks) or earlier_hold
                target = w.binds & (db.linked | db.erased) if not cfg.no_scope_shrink else w.binds
                yield (f"{w.id}.commit", put(s._replace(db=acked._replace(erased=db.erased | target),
                       ghost=g._replace(committed=g.committed | {w.subject}, over_erasure=bad)), i, w._replace(pc="confirm", holding=False)))
                n = spend(s, "abort")
                if n and j.up:
                    yield (f"{w.id}.abort_voided", drop(n._replace(journal=j._replace(entries=j.entries + (Entry(w.id, "void", w.gen, w.subject, w.locks, w.binds),)),
                           ghost=n.ghost._replace(aborted=n.ghost.aborted | {w.subject})), i))
        elif w.pc == "confirm":
            if j.up and w.gen == jgen and not w.zombie:
                yield (f"{w.id}.confirm", drop(s._replace(journal=j._replace(entries=j.entries + (Entry(w.id, "confirm", w.gen, w.subject),))), i))
            else:
                yield (f"{w.id}.confirm_lost", drop(s, i))

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
               ghost=g._replace(journaled_unlinks=g.journaled_unlinks | {b})))
    if any(s.workers):
        n = spend(s, "crash")
        if n:
            yield ("crash", n._replace(workers=(None,) * len(s.workers)))
    n = spend(s, "snap")
    if n and not db.isolated:
        yield ("snapshot", n._replace(snaps=n.snaps + (db,)))
    n = spend(s, "heartbeat")
    if n and j.up and ok:
        beat = j.entries + (Entry(f"hb{s.next_id}", "heartbeat", jgen),)
        yield ("heartbeat.append", n._replace(journal=j._replace(entries=beat), next_id=s.next_id + 1))
    if ok and beat_count(j) > g.max_beat:
        yield ("heartbeat.commit", s._replace(db=ack(db._replace(beats=beat_count(j)), j.entries),
               ghost=g._replace(max_beat=beat_count(j), covered=g.covered | g.covered_once, covered_once=g.covered_once | (g.acked_unlinks - db.linked))))
    n = spend(s, "outage")
    if n:
        yield ("journal.toggle", n._replace(journal=j._replace(up=not j.up)))
    for snap in s.snaps:
        n = spend(s, "undeclared")
        if n:
            yield ("undeclared_rollback", n._replace(db=snap, workers=(None,) * len(s.workers), ghost=n.ghost._replace(undeclared=True)))
    if ok and j.up and not busy and needs_reconcile(cfg, db, j):
        pending = [e for e in j.entries if e.kind == "intent" and e.subject not in g.committed and not any(x.kind == "void" and x.id == e.id for x in j.entries)]
        unresolved = [e for e in j.entries if e.kind == "intent" and not any(x.kind in ("void", "confirm") and x.id == e.id for x in j.entries)]
        if cfg.void_on_absence and unresolved:
            yield ("sweep.void", s._replace(journal=j._replace(entries=j.entries + (Entry(unresolved[0].id, "void", jgen, unresolved[0].subject),))))
        else:
            new, forwarded = replay(cfg, db, j)
            yield ("sweep.forward", s._replace(db=ack(new, j.entries), ghost=forward_ghost(g, forwarded, db, cfg)))
    if s.old is not None and not s.old_stopped and s.restore:
        for b in sorted(s.old.linked - s.old.erased):
            n = spend(s, "old_write")
            if n:
                yield (f"old_primary.unlink.{b}", n._replace(old=s.old._replace(linked=s.old.linked - {b}, unjournaled=s.old.unjournaled | {b}),
                       ghost=n.ghost._replace(late_lost=n.ghost.late_lost | {b}, acked_unlinks=n.ghost.acked_unlinks | {b})))
    if s.restore is None:
        for snap in s.snaps:
            n = spend(s, "restore")
            if n:
                zombies = tuple(w._replace(zombie=True, holding=False) if w else None for w in n.workers)
                yield ("restore.load_isolated", n._replace(db=snap._replace(isolated=not cfg.no_isolation), old=db, old_stopped=False, restore="loaded", workers=zombies))
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
                       ghost=g._replace(journaled_unlinks=g.journaled_unlinks | {b}, late_lost=g.late_lost - {b})))
            else:
                yield ("restore.cutover", s._replace(journal=j._replace(entries=j.entries + (Entry(f"g{jgen + 1}", "gen", jgen + 1),)), restore="cut",
                       ghost=g._replace(salvage_skipped=bool(pending))))
    elif s.restore == "cut" and j.up:
        new, forwarded = replay(cfg, db, j)
        ghost = forward_ghost(g, forwarded, db, cfg)
        yield ("restore.apply", s._replace(db=ack(new._replace(gen=jgen, isolated=False, beats=beat_count(j)), j.entries), restore=None,
               activation=cfg.activation_stays_open, ghost=ghost))
    if not s.activation and not s.restore and not db.isolated:
        yield ("controller.accept_window", s._replace(activation=True, ghost=g._replace(accepted_losses=g.acked_unlinks)))


def violations(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    dispatch = can_dispatch(cfg, s)
    if g.over_erasure:
        yield "erasure executed against a committed hold or an observed rollback"
    for subject in SUBJECTS:
        binds = frozenset(b for b in ITEMS if SUBJECT[b] == subject)
        intents = [e for e in j.entries if e.kind == "intent" and e.subject == subject]
        allowed = frozenset().union(*[e.binds for e in intents] or [frozenset()])
        if (db.erased & binds) - allowed - (binds if subject in g.committed else frozenset()):
            yield f"erased a binding of {subject} that no intent named"
    if "b1" in db.erased and not any(e.kind == "intent" and "b1" in e.binds for e in j.entries):
        yield "erased a binding linked after the intent"
    for b in sorted(db.erased):
        unlinked = any(e.kind == "unlink" and b in e.binds and any(x.kind == "intent" and b in x.binds and j.entries.index(x) < j.entries.index(e) for x in j.entries) for e in j.entries)
        if unlinked and SUBJECT[b] not in g.committed:
            yield "forward erased a binding unlinked after the intent"
    for subject in sorted(g.committed):
        binds = frozenset(e_b for e in j.entries if e.kind == "intent" and e.subject == subject for e_b in e.binds)
        missing = binds - db.erased - (binds - db.linked)
        hold_won = bool(g.held & items_of(binds)) or any(e.kind == "hold" and e.locks & items_of(binds) for e in j.entries)
        confirmed = any(x.kind == "confirm" and x.subject == subject for x in j.entries)
        if dispatch and missing and (confirmed or not hold_won):
            yield "dispatch on a database that lost a committed erasure"
    lost = g.journaled_unlinks & (db.linked - db.unjournaled)
    if dispatch and lost:
        yield "dispatch after losing a journaled unlink"
    unjournaled_loss = (g.acked_unlinks - g.journaled_unlinks) & db.linked
    if dispatch and unjournaled_loss - g.accepted_losses and (cfg.check_undetectable or not g.undeclared or unjournaled_loss & g.covered):
        yield "dispatch after losing an acknowledged write without an accepted window"
    if g.salvage_skipped:
        yield "cutover abandoned acknowledged writes on a reachable old primary"
    ids = [(e.id, e.kind) for e in j.entries]
    if len(ids) != len(set(ids)) or len([1 for e in j.entries if e.kind == "unlink"]) != len({b for e in j.entries if e.kind == "unlink" for b in e.binds}):
        yield "duplicate journal entry"


def explore(cfg, limit=2_500_000):
    start = State(Db(linked=cfg.initial), Journal(), (None, None), None, (), None, False, None, True, Ghost(), 1, cfg.budget)
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


NO_B1 = frozenset({"a0", "a1", "b2"})
ALL = frozenset({"a0", "a1", "b1", "b2"})
# erase_a, erase_b, hold, unlink, link, snap, restore, undeclared, outage, heartbeat, crash, timeout, abort, old_write
SCENARIOS = {
    "concurrent intersecting erasures":  ((1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 1, 0), 1, ALL),
    "hold on the shared profile":        ((1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0), 1, ALL),
    "hold on one scope only":            ((1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0), 2, ALL),
    "journal append races":              ((1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 1, 1, 0, 0), 1, ALL),
    "unlink around erasure":             ((1, 1, 0, 1, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0), 1, ALL),
    "link around erasure":               ((1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 2, 0, 0, 0), 1, NO_B1),
    "rollback with hold":                ((1, 1, 1, 0, 0, 1, 0, 1, 0, 0, 1, 0, 0, 0), 1, ALL),
    "rollback and heartbeat":            ((1, 0, 0, 1, 0, 1, 0, 1, 0, 2, 1, 0, 0, 0), 1, ALL),
    "restore across scopes":             ((1, 1, 1, 0, 0, 1, 1, 0, 0, 0, 1, 0, 0, 0), 1, ALL),
    "restore with unlink":               ((1, 0, 0, 1, 0, 1, 1, 0, 0, 0, 1, 0, 0, 0), 1, ALL),
    "old primary writes during restore": ((1, 0, 0, 1, 0, 1, 1, 0, 0, 0, 0, 0, 0, 2), 1, ALL),
}

MUTANTS = {
    "old writer re-reads the head": ("no_generation_check", ["restore across scopes"]),
    "holds matched by subject": ("match_hold_by_subject", ["hold on the shared profile"]),
    "only later holds block, without reconcile-first": ({"later_holds_only": True, "act_before_reconcile": True}, ["rollback with hold", "hold on the shared profile"]),
    "append without expected position": ("no_expected_position", ["journal append races"]),
    "no rescan on position conflict": ("no_rescan_on_conflict", ["journal append races"]),
    "forward ignores a later unlink": ("no_scope_shrink", ["unlink around erasure", "journal append races"]),
    "forward uses current links": ("forward_uses_current_links", ["link around erasure"]),
    "void an intent because it is absent": ("void_on_absence", ["rollback and heartbeat", "rollback with hold"]),
    "restore without isolation": ("no_isolation", ["restore with unlink", "restore across scopes"]),
    "activation open after restore": ("activation_stays_open", ["restore with unlink", "old primary writes during restore"]),
    "restore without salvage": ("no_salvage", ["old primary writes during restore"]),
    "hold without scope locks": ("hold_without_locks", ["hold on the shared profile"]),
    "act on an unreconciled scope": ("act_before_reconcile", ["rollback with hold", "concurrent intersecting erasures"]),
    "no heartbeat position check": ("no_heartbeat_check", ["rollback and heartbeat"]),
    "non-idempotent flush": ("non_idempotent_flush", ["rollback and heartbeat", "restore with unlink"]),
}

KNOWN_LIMIT = ("check_undetectable", "rollback and heartbeat", "undeclared rollback can lose an unjournaled acknowledged write")


def run(scenario, **flags):
    budget, hold_item, initial = SCENARIOS[scenario]
    return explore(Config(budget, hold_item=hold_item, initial=initial, **flags))


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
    flag, scenario, label = KNOWN_LIMIT
    _, violation, path = run(scenario, **{flag: True})
    if violation:
        print(f"LIMIT  {label}: {' -> '.join(path)}")
    else:
        print(f"LIMIT  expected witness missing for: {label}")
        failed = True
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
