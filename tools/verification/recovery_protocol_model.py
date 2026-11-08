#!/usr/bin/env python3
"""Exhaustive bounded model of the recovery journal protocol in ADR 0010.

This is a design check, not an implementation and not a proof. It enumerates the
interleavings of the operations below within small budgets, for one scope and two
workers, and checks safety invariants at each state. Each mutant removes one protocol
rule and must be caught. Exit status is non-zero on a base violation, a surviving
mutant, or a missing witness for a documented limit.
"""
from collections import deque
from typing import NamedTuple, Optional
import sys


class Db(NamedTuple):
    life: str = "active"  # active, pending, erased
    hold: bool = False
    withdrawn: bool = False
    unjournaled: bool = False  # a commit-first withdrawal is waiting for the journal
    intake: bool = False  # a withdrawal accepted before verification
    tx: int = 0  # transactional counter, rewinds with the database
    gen: int = 0
    isolated: bool = False
    anchor: frozenset = frozenset()  # entries acknowledged; models the stored head hash of the chain


class Entry(NamedTuple):
    id: int
    kind: str
    gen: int
    tx: int = 0


class Journal(NamedTuple):
    entries: tuple = ()
    up: bool = True


class Worker(NamedTuple):
    op: str
    pc: str  # append, unknown, commit, confirm, void
    gen: int
    id: int
    expected: int


class Ghost(NamedTuple):
    erased: bool = False
    held: bool = False
    refused: bool = False
    aborted: bool = False
    withdrew: bool = False
    withdrew_journaled: bool = False
    withdrew_covered: bool = False  # a heartbeat recorded the transaction containing it
    window_accepted: bool = False
    undeclared: bool = False


class State(NamedTuple):
    db: Db
    journal: Journal
    workers: tuple
    lock: Optional[int]
    late: Optional[tuple]  # (expected head, entry) for an append that timed out
    snaps: tuple
    old: Optional[Db]
    restore: Optional[str]
    activation: bool
    ghost: Ghost
    next_id: int
    budget: tuple


BUDGET_NAMES = ("accept", "erase", "hold", "refuse", "withdraw", "snap", "restore", "undeclared",
                "outage", "truncate", "heartbeat", "crash", "timeout", "abort")
WITHDRAW_ID = 9


class Config(NamedTuple):
    budget: tuple
    void_on_absence: bool = False
    no_expected_position: bool = False
    forward_ignores_holds: bool = False
    no_isolation: bool = False
    activation_stays_open: bool = False
    hold_commit_first: bool = False
    no_tail_check: bool = False
    no_reconcile_before_dispatch: bool = False
    act_before_reconcile: bool = False
    non_idempotent_append: bool = False
    void_after_release: bool = False
    no_heartbeat_floor: bool = False
    intake_merge_drops: bool = False
    check_undetectable: bool = False
    check_hold_tradeoff: bool = False


def gen_of(j):
    return sum(1 for e in j.entries if e.kind == "gen")


def head(j):
    return len(j.entries)


def spend(s, name):
    i = BUDGET_NAMES.index(name)
    if s.budget[i] <= 0:
        return None
    return s._replace(budget=s.budget[:i] + (s.budget[i] - 1,) + s.budget[i + 1:])


def put(s, i, w):
    return s._replace(workers=s.workers[:i] + (w,) + s.workers[i + 1:])


def later_block(cfg, j, i, intent):
    for k, e in enumerate(j.entries):
        if e.kind == "void" and e.id == intent.id:
            return True
        if k > i and e.kind == "intent":
            return True
        if k > i and e.kind in ("hold", "refuse") and not cfg.forward_ignores_holds:
            return True
    return False


def replay(cfg, db, j):
    """Roll the database forward from the journal. Idempotent."""
    life, hold, withdrawn = db.life, db.hold, db.withdrawn
    for i, e in enumerate(j.entries):
        if e.kind == "accept" and life == "active":
            life = "pending"
        elif e.kind == "refuse" and life == "pending":
            life = "active"
        elif e.kind == "hold" and life != "erased":
            hold = True
        elif e.kind == "withdraw":
            withdrawn = True
        elif e.kind == "intent" and life != "erased":
            confirmed = any(x.kind == "confirm" and x.id == e.id for x in j.entries)
            if confirmed or not later_block(cfg, j, i, e):
                life = "erased"
    return db._replace(life=life, hold=hold, withdrawn=withdrawn)


def needs_reconcile(cfg, db, j):
    new = replay(cfg, db, j)
    return (new.life, new.hold, new.withdrawn) != (db.life, db.hold, db.withdrawn) or db.intake


def keys_of(entries):
    return frozenset((e.id, e.kind, e.tx) for e in entries)


def tail_ok(db, j):
    """The hash chain shows every acknowledged entry is still in the journal."""
    return db.anchor <= keys_of(j.entries)


def ack(db, entries):
    return db._replace(anchor=db.anchor | keys_of(entries))


def floor(j):
    return max([e.tx for e in j.entries if e.kind == "heartbeat"] + [0])


def can_dispatch(cfg, s):
    db, j = s.db, s.journal
    if db.isolated or not j.up or not s.activation or db.gen != gen_of(j):
        return False
    if not cfg.no_tail_check and not tail_ok(db, j):
        return False
    if not cfg.no_heartbeat_floor and db.tx < floor(j):
        return False
    if not cfg.no_reconcile_before_dispatch and needs_reconcile(cfg, db, j):
        return False
    return True


def committed(s, i, db, ghost=None, keep=False):
    s = s._replace(db=ack(db._replace(tx=db.tx + 1), s.journal.entries))
    if ghost:
        s = s._replace(ghost=ghost)
    if keep:
        return s._replace(lock=None)
    return s._replace(workers=s.workers[:i] + (None,) + s.workers[i + 1:], lock=None if s.lock == i else s.lock)


def successors(cfg, s):
    db, j = s.db, s.journal
    g = s.ghost
    jgen = gen_of(j)
    verified = (not db.isolated and db.gen == jgen and (cfg.no_heartbeat_floor or db.tx >= floor(j))
                and (cfg.no_tail_check or tail_ok(db, j)))
    free = s.lock is None
    for i, w in enumerate(s.workers):
        release = {"workers": s.workers[:i] + (None,) + s.workers[i + 1:], "lock": None if s.lock == i else s.lock}
        if w is None:
            ready = cfg.act_before_reconcile or not needs_reconcile(cfg, db, j)
            if free and verified and j.up:
                for op, ok, gate in (("accept", db.life == "active", ready),
                                     ("erase", db.life == "pending" and not db.hold, ready),
                                     ("hold", db.life != "erased" and not db.hold, True),
                                     ("refuse", db.life == "pending", True)):
                    if ok and gate:
                        n = spend(s, op)
                        if n:
                            if op == "erase":
                                n = n._replace(ghost=n.ghost._replace(aborted=False))
                            yield (f"{op}.begin", n._replace(lock=i, next_id=s.next_id + 1, workers=n.workers[:i] +
                                   (Worker(op, "append", db.gen, s.next_id, head(j)),) + n.workers[i + 1:]))
            if db.life != "erased" and not db.withdrawn and not db.isolated:
                n = spend(s, "withdraw")
                if n:
                    win = n.ghost._replace(withdrew=True, window_accepted=False)
                    if verified and free:
                        yield ("withdraw.commit", n._replace(db=db._replace(withdrawn=True, unjournaled=True, tx=db.tx + 1), ghost=win))
                    if not verified or not j.up:
                        yield ("withdraw.intake", n._replace(db=db._replace(intake=True), ghost=win))
            continue
        kind = {"accept": "accept", "erase": "intent", "hold": "hold", "refuse": "refuse"}[w.op]
        entry = Entry(w.id, kind, w.gen)
        if w.pc == "append":
            if w.op == "hold" and cfg.hold_commit_first:
                if not db.isolated and db.gen == w.gen:
                    yield ("hold.commit_first", committed(s, i, db._replace(hold=True), g._replace(held=True)))
                continue
            if j.up and (w.expected == head(j) or cfg.no_expected_position):
                yield (f"{w.op}.append", put(s._replace(journal=j._replace(entries=j.entries + (entry,))), i,
                       w._replace(pc="commit", expected=head(j) + 1)))
            elif j.up and w.gen == jgen:
                yield (f"{w.op}.reread", put(s, i, w._replace(expected=head(j))))
            else:
                yield (f"{w.op}.not_applied", s._replace(**release))
            if j.up and s.late is None:
                n = spend(s, "timeout")
                if n:
                    yield (f"{w.op}.timeout", put(n._replace(late=(w.expected, entry)), i, w._replace(pc="unknown")))
        elif w.pc == "unknown":  # read back by event ID before reporting
            if j.up and any(e.id == w.id and e.kind == kind for e in j.entries):
                yield (f"{w.op}.found", put(s, i, w._replace(pc="commit")))
            elif j.up:
                yield (f"{w.op}.reported_unknown", s._replace(**release))
        elif w.pc == "commit":
            if not db.isolated and db.gen == w.gen:
                if w.op == "accept" and db.life == "active":
                    yield ("accept.commit", committed(s, i, db._replace(life="pending"), g._replace(refused=False)))
                elif w.op == "refuse" and db.life == "pending":
                    yield ("refuse.commit", committed(s, i, db._replace(life="active"), g._replace(refused=True)))
                elif w.op == "hold" and db.life != "erased":
                    yield ("hold.commit", committed(s, i, db._replace(hold=True), g._replace(held=True)))
                elif w.op == "erase" and db.life == "pending" and not db.hold:
                    done = committed(s, i, db._replace(life="erased"), g._replace(erased=True), keep=True)
                    yield ("erase.commit", put(done, i, w._replace(pc="confirm")))
                if w.op == "erase":  # the savepoint rolled back while the outer locks are held
                    n = spend(s, "abort")
                    if n and j.up and db.gen == jgen:
                        voided = j._replace(entries=j.entries + (Entry(w.id, "void", w.gen),))
                        ghost = n.ghost._replace(aborted=True)
                        if cfg.void_after_release:
                            yield ("erase.abort_released", put(n._replace(lock=None, ghost=ghost), i, w._replace(pc="void")))
                        else:
                            yield ("erase.abort_voided", n._replace(journal=voided, ghost=ghost, **release))
            else:
                yield (f"{w.op}.fenced", s._replace(**release))
        elif w.pc == "void":
            if j.up:
                yield ("erase.void_late", s._replace(journal=j._replace(entries=j.entries + (Entry(w.id, "void", w.gen),)), **release))
        elif w.pc == "confirm":
            if j.up and w.gen == jgen:
                yield ("erase.confirm", s._replace(journal=j._replace(entries=j.entries + (Entry(w.id, "confirm", w.gen),)),
                       workers=s.workers[:i] + (None,) + s.workers[i + 1:]))
            else:
                yield ("erase.confirm_lost", s._replace(workers=s.workers[:i] + (None,) + s.workers[i + 1:]))

    if s.late:
        expected, entry = s.late
        if j.up and (expected == head(j) or cfg.no_expected_position):
            yield ("late_append.lands", s._replace(journal=j._replace(entries=j.entries + (entry,)), late=None))
        elif j.up:
            yield ("late_append.rejected", s._replace(late=None))
    if db.unjournaled and j.up and verified:
        known = any(e.id == WITHDRAW_ID and e.kind == "withdraw" for e in j.entries)
        entries = j.entries if known and not cfg.non_idempotent_append else j.entries + (Entry(WITHDRAW_ID, "withdraw", jgen),)
        yield ("withdraw.flush", s._replace(journal=j._replace(entries=entries),
               db=ack(db._replace(unjournaled=False), entries), ghost=g._replace(withdrew_journaled=True)))
    if any(s.workers) or s.lock is not None:
        n = spend(s, "crash")
        if n:
            yield ("crash", n._replace(workers=(None,) * len(s.workers), lock=None))
    n = spend(s, "snap")
    if n and not db.isolated:
        yield ("snapshot", n._replace(snaps=n.snaps + (db,)))
    n = spend(s, "heartbeat")
    if n and j.up and verified:
        covered = g.withdrew and db.withdrawn
        beat = j.entries + (Entry(0, "heartbeat", jgen, db.tx),)
        yield ("heartbeat", n._replace(journal=j._replace(entries=beat), db=ack(db, beat), ghost=n.ghost._replace(withdrew_covered=g.withdrew_covered or covered)))
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
    # The bounded sweeper. Holds and refusals do not wait for it; they block by position.
    if verified and j.up and free and needs_reconcile(cfg, db, j):
        pending = [e for e in j.entries if e.kind == "intent" and not any(x.kind == "void" and x.id == e.id for x in j.entries)]
        if cfg.void_on_absence and pending and db.life != "erased":
            yield ("sweep.void", s._replace(journal=j._replace(entries=j.entries + (Entry(pending[0].id, "void", jgen),))))
        else:
            new = replay(cfg, db, j)
            merged = db.intake and not cfg.intake_merge_drops
            yield ("sweep.forward", s._replace(db=ack(new._replace(intake=False, withdrawn=new.withdrawn or db.intake,
                   unjournaled=new.unjournaled or merged), j.entries)))
    if s.restore is None:
        for snap in s.snaps:
            n = spend(s, "restore")
            if n:
                target = snap._replace(isolated=not cfg.no_isolation)
                yield ("restore.isolate_and_load", n._replace(db=target, old=db, restore="loaded", lock=None))
    elif s.restore == "loaded" and j.up:
        if s.old and (s.old.unjournaled or s.old.intake) and not any(e.id == WITHDRAW_ID for e in j.entries):
            yield ("restore.salvage", s._replace(journal=j._replace(entries=j.entries + (Entry(WITHDRAW_ID, "withdraw", jgen),)),
                   old=None, ghost=g._replace(withdrew_journaled=True)))
        yield ("restore.cutover", s._replace(journal=j._replace(entries=j.entries + (Entry(0, "gen", jgen + 1),)), restore="cut"))
    elif s.restore == "cut" and j.up:
        new = replay(cfg, db, j)
        merged = db.intake and not cfg.intake_merge_drops
        yield ("restore.apply", s._replace(db=ack(new._replace(gen=jgen, isolated=False, intake=False, tx=max(new.tx, floor(j)),
               withdrawn=new.withdrawn or db.intake, unjournaled=new.unjournaled or merged), j.entries),
               restore=None, old=None, activation=cfg.activation_stays_open))
    if not s.activation and not s.restore and not db.isolated:
        yield ("controller.accept_window", s._replace(activation=True, ghost=g._replace(window_accepted=True)))


def violations(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    dispatch = can_dispatch(cfg, s)
    # A hold placed after an undeclared rollback outranks an unconfirmed intent. See ADR 0010.
    hold_won = any(e.kind in ("hold", "refuse") for e in j.entries) and not any(e.kind == "confirm" for e in j.entries)
    if dispatch and g.erased and db.life != "erased" and (cfg.check_hold_tradeoff or not (g.undeclared and hold_won)):
        yield "dispatch on a database that lost a committed erasure"
    if dispatch and not tail_ok(db, j):
        yield "dispatch on a journal missing applied entries"
    lost = g.withdrew and not (db.withdrawn or db.intake) and not g.window_accepted
    undetectable = g.undeclared and not g.withdrew_journaled and not g.withdrew_covered
    if dispatch and lost and (cfg.check_undetectable or not undetectable):
        yield "dispatch after losing a withdrawal without an accepted window"
    if db.life == "erased" and not g.erased and (g.held or g.refused or g.aborted):
        yield "erasure executed against a hold, refusal or observed rollback"
    if db.withdrawn and not db.unjournaled and not db.intake and not any(e.kind == "withdraw" for e in j.entries) \
            and j.up and not s.restore and tail_ok(db, j):
        yield "a withdrawal can never reach the journal"
    keys = [(e.id, e.kind) for e in j.entries if e.kind not in ("heartbeat", "gen")]
    if len(keys) != len(set(keys)):
        yield "duplicate journal entry"
    if any(e.kind == "void" and any(x.kind == "confirm" and x.id == e.id for x in j.entries) for e in j.entries):
        yield "a confirmed erasure was voided"
    if g.erased and any(e.kind == "void" for e in j.entries) and db.life != "erased" and not db.isolated and not s.restore and not g.aborted:
        yield "a committed erasure was voided while in doubt"


def explore(cfg, limit=3_000_000):
    start = State(Db(), Journal(), (None, None), None, None, (), None, None, True, Ghost(), 1, cfg.budget)
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


# accept, erase, hold, refuse, withdraw, snap, restore, undeclared, outage, truncate, heartbeat, crash, timeout, abort
SCENARIOS = {
    "erasure crash windows":            (1, 2, 0, 0, 0, 1, 1, 0, 1, 0, 0, 2, 0, 1),
    "concurrent erasure attempts":      (1, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 1),
    "late append against a hold":       (1, 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0),
    "hold and refusal against erasure": (1, 1, 1, 1, 0, 1, 1, 0, 0, 0, 0, 1, 0, 1),
    "refusal then new acceptance":      (2, 2, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0),
    "observed rollback":                (1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1),
    "withdrawal and journal outage":    (1, 1, 0, 0, 1, 1, 1, 0, 2, 0, 1, 1, 0, 0),
    "two restores":                     (1, 1, 0, 0, 1, 2, 2, 0, 0, 0, 0, 1, 0, 0),
    "undeclared rollback":              (1, 1, 0, 1, 1, 1, 0, 1, 0, 0, 1, 1, 0, 0),
    "undeclared rollback with hold":    (1, 1, 1, 0, 0, 1, 0, 1, 0, 0, 0, 1, 0, 0),
    "journal tail loss":                (1, 1, 0, 0, 1, 0, 0, 0, 0, 1, 1, 1, 0, 0),
    "combined faults":                  (1, 1, 1, 0, 1, 1, 1, 0, 1, 0, 1, 1, 0, 0),
}

MUTANTS = {
    "void an in-doubt intent when absent": ("void_on_absence", ["undeclared rollback"]),
    "append without expected position": ("no_expected_position", ["late append against a hold", "two restores"]),
    "roll forward past a later hold": ("forward_ignores_holds", ["hold and refusal against erasure", "late append against a hold"]),
    "restore without isolation": ("no_isolation", ["two restores", "erasure crash windows"]),
    "activation open after restore": ("activation_stays_open", ["withdrawal and journal outage", "two restores"]),
    "hold commits before the journal": ("hold_commit_first", ["hold and refusal against erasure"]),
    "journal tail loss not checked": ("no_tail_check", ["journal tail loss"]),
    "dispatch without reconciliation": ("no_reconcile_before_dispatch", ["erasure crash windows", "undeclared rollback"]),
    "act on an unreconciled database": ("act_before_reconcile", ["undeclared rollback with hold", "erasure crash windows"]),
    "non-idempotent journal append": ("non_idempotent_append", ["undeclared rollback"]),
    "void after releasing locks": ("void_after_release", ["observed rollback"]),
    "no heartbeat floor": ("no_heartbeat_floor", ["undeclared rollback"]),
    "intake merged without journaling": ("intake_merge_drops", ["withdrawal and journal outage"]),
}

KNOWN_LIMITS = (("check_undetectable", "undeclared rollback", "undeclared rollback can lose an unjournaled withdrawal"),
                ("check_hold_tradeoff", "undeclared rollback with hold", "a later hold outranks an unconfirmed erasure lost to an undeclared rollback"))


def main():
    failed = False
    total = 0
    for name, budget in SCENARIOS.items():
        states, violation, path = explore(Config(budget))
        total += states
        print(f"BASE   {name}: {states} states, {'OK' if violation is None else 'VIOLATION: ' + violation}")
        if violation:
            print("       trace: " + " -> ".join(path))
            failed = True
    print(f"TOTAL  {total} states")
    for name, (flag, scenarios) in MUTANTS.items():
        caught = None
        for scenario in scenarios:
            _, violation, path = explore(Config(SCENARIOS[scenario], **{flag: True}))
            if violation:
                caught = (scenario, violation)
                break
        if caught:
            print(f"MUTANT {name}: caught in '{caught[0]}': {caught[1]}")
        else:
            print(f"MUTANT {name}: SURVIVED")
            failed = True
    for flag, scenario, label in KNOWN_LIMITS:
        _, violation, path = explore(Config(SCENARIOS[scenario], **{flag: True}))
        if violation:
            print(f"LIMIT  {label}: {' -> '.join(path)}")
        else:
            print(f"LIMIT  expected witness missing for: {label}; update ADR 0010")
            failed = True
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
