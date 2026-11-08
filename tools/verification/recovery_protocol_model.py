#!/usr/bin/env python3
"""Exhaustive bounded model of the recovery journal protocol in ADR 0010.

This is a design check, not an implementation. It enumerates every interleaving of
the operations below within small budgets and checks the safety invariants at each
state. Each mutant removes one protocol rule and must be caught, which shows that
the invariants have force. Run with no arguments. Exit status is non-zero on any
base violation or any surviving mutant.
"""
from collections import deque
from typing import NamedTuple, Optional
import sys


class Db(NamedTuple):
    life: str = "active"  # active, pending, erased
    hold: bool = False
    withdrawn: bool = False
    unjournaled: Optional[int] = None  # commit-first event waiting for the journal
    intake: bool = False  # withdrawal accepted before verification
    seq: int = 0
    gen: int = 0
    quarantine: bool = False
    ack: int = 0  # journal length this database has acknowledged
    intents: frozenset = frozenset()


class Entry(NamedTuple):
    seq: int
    kind: str
    gen: int


class Journal(NamedTuple):
    gen: int = 0
    entries: tuple = ()
    up: bool = True


class Worker(NamedTuple):
    op: str
    pc: int
    gen: int
    seq: int = 0


class Ghost(NamedTuple):
    erased: bool = False  # an erasure transaction truly committed on some timeline
    held: bool = False  # a hold truly committed
    refused: bool = False  # a refusal truly committed
    withdrew: bool = False  # a withdrawal truly committed
    window_accepted: bool = False  # the controller reconciled the unjournaled window
    rolled_forward: bool = False
    undeclared: bool = False  # an undeclared rollback happened
    withdrew_journaled: bool = False


class State(NamedTuple):
    db: Db
    journal: Journal
    workers: tuple
    lock: Optional[int]
    snaps: tuple
    restore: Optional[str]
    restore_snap: Optional[Db]
    activation: bool
    ghost: Ghost
    budget: tuple


BUDGET_NAMES = ("accept", "erase", "hold", "refuse", "withdraw", "snap", "restore", "undeclared",
                "outage", "truncate", "heartbeat", "crash")


class Config(NamedTuple):
    budget: tuple
    void_on_absence: bool = False
    no_generation_fence: bool = False
    no_sequence_advance: bool = False
    non_idempotent_append: bool = False
    no_quarantine: bool = False
    activation_stays_open: bool = False
    hold_commit_first: bool = False
    no_journal_rollback_check: bool = False
    no_reconcile_before_dispatch: bool = False
    no_resequence_on_restore: bool = False
    act_before_reconcile: bool = False
    check_undetectable: bool = False


def spend(s, name):
    i = BUDGET_NAMES.index(name)
    if s.budget[i] <= 0:
        return None
    return s._replace(budget=s.budget[:i] + (s.budget[i] - 1,) + s.budget[i + 1:])


def set_worker(s, i, w):
    return s._replace(workers=s.workers[:i] + (w,) + s.workers[i + 1:])


def append(j, seq, kind, gen):
    return j._replace(entries=j.entries + (Entry(seq, kind, gen),))


def can_append(cfg, s, gen):
    return s.journal.up and (cfg.no_generation_fence or gen == s.journal.gen)


def can_commit(cfg, s, gen):
    return (cfg.no_quarantine or not s.db.quarantine) and (cfg.no_generation_fence or s.db.gen == gen)


def needs_reconcile(cfg, db, j):
    """True when replaying the journal would change this database."""
    new, _ = apply_journal(cfg, db, j, Ghost())
    return (new.life, new.hold, new.withdrawn) != (db.life, db.hold, db.withdrawn)


def can_dispatch(cfg, s):
    db, j = s.db, s.journal
    if db.quarantine or not j.up or not s.activation or db.gen != j.gen:
        return False
    if not cfg.no_journal_rollback_check and len(j.entries) < db.ack:
        return False
    floors = [e.seq for e in j.entries if e.kind in ("confirm", "withdraw", "heartbeat")]
    if floors and db.seq < max(floors):
        return False
    if not cfg.no_reconcile_before_dispatch and needs_reconcile(cfg, db, j):
        return False
    return True


def later_block(j, intent):
    """An intent stops rolling forward only when voided or superseded by a later attempt."""
    return any((e.kind == "intent" and e.seq > intent.seq)
               or (e.kind == "void" and e.seq == intent.seq) for e in j.entries)


def apply_journal(cfg, db, j, ghost):
    """Roll the database forward from the journal. Idempotent."""
    life, hold, withdrawn = db.life, db.hold, db.withdrawn
    forward = ghost.rolled_forward
    for e in sorted(j.entries, key=lambda x: x.seq):
        if e.kind == "accept" and life == "active":
            life = "pending"
        elif e.kind == "refuse" and life == "pending":
            life = "active"
        elif e.kind == "hold" and life != "erased":
            hold = True
        elif e.kind == "withdraw":
            withdrawn = True
        elif e.kind == "intent" and life != "erased":
            blocked = later_block(j, e)
            definite = any(x.kind == "confirm" and x.seq == e.seq for x in j.entries)
            if definite or not blocked:
                if not definite and not ghost.erased:
                    forward = True
                life = "erased"
    high = max([e.seq for e in j.entries] + [db.seq])
    seq = db.seq if cfg.no_sequence_advance else high
    unjournaled = db.unjournaled
    if unjournaled is not None and not cfg.no_resequence_on_restore and high >= unjournaled:
        seq = max(seq, high) + 1  # a surviving commit-first event must not reuse a sequence
        unjournaled = seq
    return db._replace(life=life, hold=hold, withdrawn=withdrawn or db.intake, intake=False, seq=seq,
                       gen=j.gen, quarantine=False, ack=len(j.entries), unjournaled=unjournaled), forward


def successors(cfg, s):
    db, j = s.db, s.journal
    idle_lock = s.lock is None
    verified = not db.quarantine and db.gen == j.gen
    current = j.up and db.seq >= max([e.seq for e in j.entries] + [0])
    for i, w in enumerate(s.workers):
        if w is None:
            ready = cfg.act_before_reconcile or not needs_reconcile(cfg, db, j)
            if idle_lock and verified and j.up and ready:
                for op, ok in (("accept", db.life == "active"),
                               ("erase", db.life == "pending" and not db.hold),
                               ("hold", db.life != "erased" and not db.hold),
                               ("refuse", db.life == "pending")):
                    if ok:
                        n = spend(s, op)
                        if n:
                            seq = db.seq + 1
                            yield (f"{op}.begin", n._replace(db=db._replace(seq=seq), lock=i,
                                   workers=n.workers[:i] + (Worker(op, 0, db.gen, seq),) + n.workers[i + 1:]))
            if idle_lock and db.life != "erased" and not db.withdrawn:
                n = spend(s, "withdraw")
                if n:
                    if verified and current:
                        seq = db.seq + 1
                        yield ("withdraw.commit", n._replace(db=db._replace(withdrawn=True, unjournaled=seq, seq=seq),
                               ghost=n.ghost._replace(withdrew=True)))
                    elif not db.intake:
                        yield ("withdraw.intake", n._replace(db=db._replace(intake=True),
                               ghost=n.ghost._replace(withdrew=True)))
            continue
        kind = {"accept": "accept", "erase": "intent", "hold": "hold", "refuse": "refuse"}[w.op]
        release = {"workers": s.workers[:i] + (None,) + s.workers[i + 1:], "lock": None if s.lock == i else s.lock}
        if w.pc == 0:  # journal first
            if w.op == "hold" and cfg.hold_commit_first:
                if can_commit(cfg, s, w.gen):
                    yield ("hold.commit_first", s._replace(db=db._replace(hold=True), ghost=s.ghost._replace(held=True),
                           **release))
                continue
            if can_append(cfg, s, w.gen):
                yield (f"{w.op}.journal", set_worker(s._replace(journal=append(j, w.seq, kind, w.gen)), i, w._replace(pc=1)))
            else:
                yield (f"{w.op}.abort", s._replace(**release))
        elif w.pc == 1:  # database commit
            if can_commit(cfg, s, w.gen):
                ack = max(db.ack, len(j.entries))
                if w.op == "accept" and db.life == "active":
                    n = s._replace(db=db._replace(life="pending", ack=ack), **release)
                elif w.op == "refuse" and db.life == "pending":
                    n = s._replace(db=db._replace(life="active", ack=ack), ghost=s.ghost._replace(refused=True), **release)
                elif w.op == "hold" and db.life != "erased":
                    n = s._replace(db=db._replace(hold=True, ack=ack), ghost=s.ghost._replace(held=True), **release)
                elif w.op == "erase" and db.life == "pending" and not db.hold:
                    n = set_worker(s._replace(db=db._replace(life="erased", ack=ack, intents=db.intents | {w.seq}),
                                              ghost=s.ghost._replace(erased=True), lock=None), i, w._replace(pc=2))
                else:
                    n = None
                if n:
                    yield (f"{w.op}.commit", n)
            if cfg.void_on_absence or not can_commit(cfg, s, w.gen):
                if can_append(cfg, s, w.gen) and w.op == "erase":  # the worker observed a definite abort
                    yield ("erase.void", s._replace(journal=append(j, w.seq, "void", w.gen), **release))
                elif not can_append(cfg, s, w.gen):
                    yield (f"{w.op}.stranded", s._replace(**release))
        elif w.pc == 2:  # confirmation after commit
            if can_append(cfg, s, w.gen):
                yield ("erase.confirm", s._replace(journal=append(j, w.seq, "confirm", w.gen),
                       db=db._replace(ack=max(db.ack, len(j.entries) + 1)),
                       workers=s.workers[:i] + (None,) + s.workers[i + 1:]))
            else:
                yield ("erase.confirm_lost", s._replace(workers=s.workers[:i] + (None,) + s.workers[i + 1:]))

    if db.unjournaled is not None and j.up and db.gen == j.gen and not db.quarantine:
        known = any(e.seq == db.unjournaled and e.kind == "withdraw" for e in j.entries)
        flushed = j if known and not cfg.non_idempotent_append else append(j, db.unjournaled, "withdraw", j.gen)
        yield ("withdraw.flush", s._replace(journal=flushed,
               db=db._replace(unjournaled=None, ack=len(flushed.entries)),
               ghost=s.ghost._replace(withdrew_journaled=True)))
    if any(s.workers) or s.lock is not None:
        n = spend(s, "crash")
        if n:
            yield ("crash", n._replace(workers=(None,) * len(s.workers), lock=None))
    n = spend(s, "snap")
    if n and not db.quarantine:
        yield ("snapshot", n._replace(snaps=n.snaps + (db,)))
    n = spend(s, "heartbeat")
    if n and j.up and verified:
        yield ("heartbeat", n._replace(journal=append(j, db.seq, "heartbeat", j.gen), db=db._replace(ack=len(j.entries) + 1)))
    n = spend(s, "outage")
    if n:
        yield ("journal.toggle", n._replace(journal=j._replace(up=not j.up)))
    n = spend(s, "truncate")
    if n and j.entries:
        yield ("journal.truncate", n._replace(journal=j._replace(entries=j.entries[:-1])))
    for snap in s.snaps:
        n = spend(s, "undeclared")
        if n:
            yield ("undeclared_rollback", n._replace(db=snap, workers=(None,) * len(s.workers), lock=None,
                   ghost=n.ghost._replace(undeclared=True)))
    # Same-generation reconciliation runs without manual steps.
    if verified and j.up and idle_lock and not any(s.workers) and needs_reconcile(cfg, db, j):
        closed = {e.seq for e in j.entries if e.kind == "void"}
        pending = [e for e in j.entries if e.kind == "intent" and e.seq not in db.intents and e.seq not in closed]
        if cfg.void_on_absence and pending:
            yield ("reconcile.void", s._replace(journal=append(j, pending[0].seq, "void", j.gen)))
        else:
            new, forward = apply_journal(cfg, db, j, s.ghost)
            yield ("reconcile.forward", s._replace(db=new._replace(ack=max(db.ack, len(j.entries))),
                   ghost=s.ghost._replace(rolled_forward=forward)))
    # Declared restore: load into quarantine, fence old writers, apply, then wait for the controller.
    if s.restore is None:
        for snap in s.snaps:
            n = spend(s, "restore")
            if n:
                loaded = snap._replace(quarantine=not cfg.no_quarantine)
                yield ("restore.load", n._replace(db=loaded, restore="loaded", restore_snap=snap, lock=None))
    elif s.restore == "loaded" and j.up:
        yield ("restore.cutover", s._replace(journal=j._replace(gen=j.gen + 1), restore="cut"))
    elif s.restore == "cut" and j.up:
        new, forward = apply_journal(cfg, db, j, s.ghost)
        yield ("restore.apply", s._replace(db=new, restore=None, restore_snap=None,
               activation=cfg.activation_stays_open, ghost=s.ghost._replace(rolled_forward=forward)))
    if not s.activation and not s.restore and not db.quarantine:
        yield ("controller.reconcile_window", s._replace(activation=True, ghost=s.ghost._replace(window_accepted=True)))


def violations(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    dispatch = can_dispatch(cfg, s)
    if dispatch and g.erased and db.life != "erased":
        yield "dispatch on a database that lost a committed erasure"
    if dispatch and len(j.entries) < db.ack:
        yield "dispatch on a journal missing acknowledged entries"
    lost = g.withdrew and not (db.withdrawn or db.intake) and not g.window_accepted
    # An undeclared rollback can lose a commit-first event before it reaches the journal.
    # Nothing can detect that, so it is asserted separately as a documented limit.
    if dispatch and lost and (cfg.check_undetectable or not g.undeclared or g.withdrew_journaled):
        yield "dispatch after losing a withdrawal without controller reconciliation"
    if db.life == "erased" and not g.erased and g.rolled_forward and (g.held or g.refused):
        yield "in-doubt intent overrode a committed hold or refusal"
    if db.life == "erased" and g.held and not g.erased:
        yield "erased a subject under a committed hold"
    keys = [(e.gen, e.seq, e.kind) for e in j.entries]
    if len(keys) != len(set(keys)):
        yield "duplicate journal entry"
    by_seq = {}
    for e in j.entries:
        if e.kind in ("intent", "hold", "refuse", "accept", "withdraw"):
            if e.seq in by_seq and by_seq[e.seq] != e.kind:
                yield "event sequence reused for a different event"
            by_seq[e.seq] = e.kind
    if any(e.kind == "void" and any(x.kind == "confirm" and x.seq == e.seq for x in j.entries) for e in j.entries):
        yield "a confirmed erasure was voided"
    if g.erased and any(e.kind == "void" for e in j.entries) and db.life != "erased" and not db.quarantine and not s.restore:
        yield "a committed erasure was voided while in doubt"


def explore(cfg, limit=2_500_000):
    start = State(Db(), Journal(), (None, None), None, (), None, None, True, Ghost(), cfg.budget)
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
                    raise SystemExit(f"state limit reached at {limit}; results would be incomplete")
                seen.add(n)
                parents[n] = (label, s)
                queue.append(n)
    return len(seen), None, []


# accept, erase, hold, refuse, withdraw, snap, restore, undeclared, outage, truncate, heartbeat, crash
SCENARIOS = {
    "erasure crash windows":              (1, 2, 0, 0, 0, 1, 1, 0, 1, 0, 0, 2),
    "concurrent erasure attempts":        (1, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3),
    "hold and refusal against erasure":   (1, 1, 1, 1, 0, 1, 1, 0, 0, 0, 0, 1),
    "withdrawal and journal outage":      (1, 1, 0, 0, 1, 1, 1, 0, 2, 0, 1, 1),
    "two restores":                       (1, 1, 0, 0, 1, 2, 2, 0, 0, 0, 0, 1),
    "undeclared rollback":                (1, 1, 0, 0, 1, 1, 0, 1, 0, 0, 1, 1),
    "undeclared rollback with hold":      (1, 1, 1, 0, 0, 1, 0, 1, 0, 0, 0, 1),
    "journal tail loss":                  (1, 1, 0, 0, 1, 0, 0, 0, 0, 1, 1, 1),
    "combined faults":                    (1, 1, 1, 0, 1, 1, 1, 0, 1, 0, 1, 1),
}

MUTANTS = {
    "void an in-doubt intent when absent": ("void_on_absence", ["undeclared rollback"]),
    "no generation fence": ("no_generation_fence", ["two restores", "erasure crash windows", "withdrawal and journal outage"]),
    "no sequence advance on restore": ("no_sequence_advance", ["two restores", "erasure crash windows", "withdrawal and journal outage"]),
    "no re-sequencing of surviving events": ("no_resequence_on_restore", ["two restores"]),
    "non-idempotent journal append": ("non_idempotent_append", ["undeclared rollback"]),
    "restore without quarantine": ("no_quarantine", ["two restores", "erasure crash windows"]),
    "activation open after restore": ("activation_stays_open", ["withdrawal and journal outage", "two restores"]),
    "hold commits before the journal": ("hold_commit_first", ["hold and refusal against erasure"]),
    "journal rollback not checked": ("no_journal_rollback_check", ["journal tail loss"]),
    "dispatch without reconciliation": ("no_reconcile_before_dispatch", ["erasure crash windows", "undeclared rollback"]),
    "act on an unreconciled database": ("act_before_reconcile", ["undeclared rollback with hold", "hold and refusal against erasure"]),
}

# Documented limit: this scenario must still show the loss, so the limit stays honest.
KNOWN_LIMIT = ("check_undetectable", "undeclared rollback")


def main():
    failed = False
    for name, budget in SCENARIOS.items():
        states, violation, path = explore(Config(budget))
        print(f"BASE   {name}: {states} states, {'OK' if violation is None else 'VIOLATION: ' + violation}")
        if violation:
            print("       trace: " + " -> ".join(path))
            failed = True
    for name, (flag, scenarios) in MUTANTS.items():
        caught = None
        for scenario in scenarios:
            _, violation, path = explore(Config(SCENARIOS[scenario], **{flag: True}))
            if violation:
                caught = (scenario, violation, path)
                break
        if caught:
            print(f"MUTANT {name}: caught in '{caught[0]}': {caught[1]}")
        else:
            print(f"MUTANT {name}: SURVIVED")
            failed = True
    flag, scenario = KNOWN_LIMIT
    _, violation, path = explore(Config(SCENARIOS[scenario], **{flag: True}))
    if violation:
        print(f"LIMIT  undeclared rollback can lose an unjournaled withdrawal: {' -> '.join(path)}")
    else:
        print("LIMIT  expected witness missing; update ADR 0010")
        failed = True
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
