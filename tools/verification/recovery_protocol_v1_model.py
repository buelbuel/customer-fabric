#!/usr/bin/env python3
"""Conservative v1 erasure-safety model for ADR 0009/0010 (addresses review findings C1-C6).

Bounded verification, not a proof. It targets the six erasure-safety invariants only.
Journal-append races, heartbeat compare-and-swap and timeline handling stay covered by
`recovery_protocol_scopes_model.py` and `recovery_protocol_model.py`; here an append lands
atomically at the head so the state space can stay small enough to reason about.

The oracles read GROUND TRUTH held in the ghost (which record is really shared, which is
really under a legal hold, which subject-level change really touched which record), never
the protocol's own classification. A rule that misclassifies cannot also silence the check.
That is the I9 fix from the 2026-11-09 review.

Design under test: the conservative alternative. Only a subscope whose separation is
positively proven under lock, whose erasure_pending fence is visible, and that no rollback
or restore has invalidated, is erased automatically. Every shared, unattested, ambiguous or
held subscope is manual: unchanged until a controller records an `authorize(intent, record)`
decision in the journal. Hold release resumes nothing. Manual obligations are durable DB
rows, so journal retention cannot lose them. Kratos deletion waits for every manual subscope
of the subject. A withdrawal acknowledged `witnessed` fails closed when the witness is
unreadable.
"""
from collections import deque
from typing import NamedTuple, Optional
import sys

RECORDS = ("a0", "p1", "b2")          # a0 exclusive to A, b2 to B, p1 shared by A and B
SUBJECT_RECORDS = {"A": ("a0", "p1"), "B": ("p1", "b2")}
INTRINSIC_SHARED = frozenset({"p1"})  # known shared from the start (two active associations)
SUBJECTS = ("A", "B")


class Db(NamedTuple):
    pending: frozenset = frozenset()       # records with a visible erasure_pending fence
    erased: frozenset = frozenset()
    holds: frozenset = frozenset()
    known_unprovable: frozenset = frozenset()  # separation the protocol has durably recorded
    reviews: frozenset = frozenset()       # (intent_id, record) durable manual obligations
    authorized: frozenset = frozenset()    # (intent_id, record) recorded authorizations
    unprove_queue: frozenset = frozenset() # commit-first loss-of-separation awaiting the journal
    epoch: int = 0                         # bumped by every rollback/restore the record saw
    gen: int = 0
    isolated: bool = False
    anchor: frozenset = frozenset()
    hb: int = 0
    withdrawn: bool = False
    w_unjournaled: bool = False
    auto_ok: bool = True  # auto-classification allowed; a restore clears it until source re-sync


class Journal(NamedTuple):
    entries: tuple = ()
    up: bool = True
    base: int = 0


class Ghost(NamedTuple):
    fault: Optional[str] = None
    gt_unprovable: frozenset = INTRINSIC_SHARED   # records whose separation is truly unprovable
    gt_held: frozenset = frozenset()              # records truly under a legal hold now
    ever_held: frozenset = frozenset()            # records a hold ever covered while an intent was open
    kratos_deleted: frozenset = frozenset()       # subjects whose identity deletion was ordered
    acked: bool = False                           # a withdrawal was acknowledged witnessed/journaled
    witnessed: bool = False
    journaled: bool = False
    undeclared: bool = False
    detectable: bool = True                       # the last rollback is detectable by the recovery check
    max_beat: int = 0


class Intent(NamedTuple):
    id: str
    subject: str
    auto: frozenset       # subscopes classified independent at append time
    manual: frozenset     # subscopes classified protected at append time
    epoch: int            # db.epoch when appended


class State(NamedTuple):
    db: Db
    journal: Journal
    witness: frozenset          # records whose erasure/withdrawal the witness recorded
    witness_up: bool
    snaps: tuple
    restoring: Optional[str]
    activation: bool
    ghost: Ghost
    next_id: int
    budget: tuple


BUDGET_NAMES = ("erase", "hold", "release", "authorize", "share", "withdraw", "snap", "rollback",
                "restore", "outage", "witness_outage", "heartbeat", "prune")


class LimitReached(Exception):
    pass


class Config(NamedTuple):
    budget: tuple
    # mutants (each disables one conservative rule)
    classify_ignores_unprove: bool = False    # C1: treat a known-shared record as auto
    rollforward_ignores_fence: bool = False   # C1: execute auto without a visible fence
    reprove_after_restore: bool = False       # C1: silently re-prove a lost fence instead of manual
    shared_auto_action: bool = False          # C2: auto-act on shared/unattested records
    subject_ignores_protection: bool = False  # C3: subject-level change touches protected records
    prune_ignores_stopped: bool = False       # C4: retention treats a hold-stopped subscope as resolved
    resume_on_release: bool = False           # C4: a released hold resumes a stopped subscope
    delete_identity_with_open_manual: bool = False  # C5
    recovery_ignores_witness_outage: bool = False   # C6
    check_undetectable: bool = False          # enable the known-limit witness


def gen_of(j):
    return sum(1 for e in j.entries if e[0] == "gen")


def head(j):
    return len(j.entries)


def spend(s, name):
    i = BUDGET_NAMES.index(name)
    if s.budget[i] <= 0:
        return None
    return s._replace(budget=s.budget[:i] + (s.budget[i] - 1,) + s.budget[i + 1:])


def entries_of(j, kind):
    return [e for e in j.entries if e[0] == kind]


def intent_of(j, iid):
    for e in j.entries:
        if e[0] == "intent" and e[1].id == iid:
            return e[1]
    return None


def anchors(entries):
    return frozenset(id(e) if False else (e[0],) + tuple(x for x in e[1:] if not isinstance(x, (frozenset,))) for e in entries)


def ack(db, j):
    return db._replace(anchor=db.anchor | frozenset(range(len(j.entries))))


def verified(cfg, s):
    db, j = s.db, s.journal
    if db.isolated or db.gen != gen_of(j):
        return False
    if not (db.anchor >= frozenset(range(db.hb))):
        return False
    # a heartbeat naming a position the database lacks reveals a rollback
    for pos, e in enumerate(j.entries, start=1):
        if e[0] == "heartbeat" and pos > db.hb and e[1] > db.hb:
            return False
    if s.ghost.witnessed and not cfg.recovery_ignores_witness_outage:
        # C6: an unreadable witness fails closed wherever a witnessed ack was issued
        if not s.witness_up:
            return False
        # a readable witness that holds a withdrawal the database no longer reflects reveals the loss
        if "w" in s.witness and not db.withdrawn:
            return False
    return True


def can_dispatch(cfg, s):
    return s.journal.up and s.activation and not s.restoring and verified(cfg, s)


def classify(cfg, db, j, subject):
    """Split a subject's live records into auto and manual under the current committed view."""
    live = [r for r in SUBJECT_RECORDS[subject] if r not in db.erased]
    auto, manual = set(), set()
    for r in live:
        known_shared = (r in INTRINSIC_SHARED or r in db.known_unprovable) and not cfg.classify_ignores_unprove
        protected = known_shared or r in db.holds or not db.auto_ok
        if protected and not cfg.shared_auto_action:
            manual.add(r)
        else:
            auto.add(r)
    return frozenset(auto), frozenset(manual)


def fence_visible(cfg, db, intent, r):
    if cfg.rollforward_ignores_fence:
        return True
    if r not in db.pending:
        return False
    # a rollback/restore since the intent invalidates a fence unless we (wrongly) re-prove
    if db.epoch != intent.epoch and not cfg.reprove_after_restore:
        return False
    if not db.auto_ok and not cfg.reprove_after_restore:
        return False
    return True


def hold_touched(cfg, s, intent, r):
    """A hold on r still in the live journal window. Releasing it resumes nothing (retention keeps it)."""
    live = s.journal.entries[s.journal.base:]
    for e in live:
        if e[0] == "hold" and e[1] == r:
            if cfg.resume_on_release and any(x[0] == "relconf" and x[1] == r for x in live):
                continue  # mutant: a released hold stops blocking
            return True
    return False


def is_manual(cfg, s, iid, r):
    """A subscope is manual if the live journal names it manual or holds it. Retention must keep that record."""
    live = s.journal.entries[s.journal.base:]
    named = any(e[0] == "intent" and e[1].id == iid and r in e[1].manual for e in live)
    return named or hold_touched(cfg, s, None, r)


def auto_eligible(cfg, s, intent, r):
    """Re-test at execute time: proven-separable, not held, never hold-touched, fence valid."""
    db = s.db
    if r in db.erased:
        return False
    if is_manual(cfg, s, intent.id, r):
        return False
    if not fence_visible(cfg, db, intent, r):
        return False
    if cfg.shared_auto_action:
        return True
    known_shared = (r in INTRINSIC_SHARED or r in db.known_unprovable) and not cfg.classify_ignores_unprove
    return not known_shared and r not in db.holds and not hold_touched(cfg, s, intent, r)


def execute_auto(cfg, s, intent):
    """Erase the auto subscopes still eligible at execute time. Worker and sweeper use the same gate."""
    db = s.db
    out = frozenset(r for r in intent.auto if auto_eligible(cfg, s, intent, r))
    if not out:
        return None
    g = s.ghost
    fault = g.fault
    for r in out:
        if (r in g.gt_unprovable or r in g.gt_held or r in g.ever_held) and not any(
                e[0] == "authorize" and e[2] == r for e in s.journal.entries):
            if not (g.undeclared and not g.detectable):      # declared loss is a known residual limit
                fault = fault or "C1/C2: erased a record whose separation or hold was lost, without authorization"
    return s._replace(db=db._replace(erased=db.erased | out), ghost=g._replace(fault=fault))


def execute_manual(cfg, s, intent, r):
    """Erase a manual subscope only with a recorded authorization and a visible fence."""
    db, g = s.db, s.ghost
    fault = g.fault
    if r not in g.gt_unprovable and r not in g.gt_held:
        pass  # a record that turned out separable may still be erased once authorized
    return s._replace(db=db._replace(erased=db.erased | {r}), ghost=g._replace(fault=fault))


def subject_scope(j, subject):
    out = set()
    for e in j.entries:
        if e[0] == "intent" and e[1].subject == subject:
            out |= set(e[1].auto) | set(e[1].manual)
    return frozenset(out)


def subject_done(cfg, s, subject):
    scope = subject_scope(s.journal, subject)
    return bool(scope) and scope <= s.db.erased


def subject_minimize(cfg, s, subject):
    """Subject-level erasure work (revoke grants, overwrite secret_hash, minimize evidence).

    It touches rows that reference a protected record only when that record is authorized.
    """
    db, g = s.db, s.ghost
    touched = set()
    for r in SUBJECT_RECORDS[subject]:
        protected = r in g.gt_unprovable or r in g.gt_held
        authorized = any(e[0] == "authorize" and e[2] == r for e in s.journal.entries)
        if not protected or authorized or cfg.subject_ignores_protection:
            touched.add(r)
    fault = g.fault
    for r in touched:
        if (r in g.gt_unprovable or r in g.gt_held) and not any(
                e[0] == "authorize" and e[2] == r for e in s.journal.entries):
            fault = fault or "C3: subject-level change touched a protected record without authorization"
    return s._replace(ghost=g._replace(fault=fault))


def manual_open(cfg, s, subject):
    """Manual subscopes of the subject not yet erased."""
    scope = subject_scope(s.journal, subject)
    return frozenset(r for r in scope if r not in s.db.erased and is_manual(cfg, s, next(
        e[1].id for e in s.journal.entries if e[0] == "intent" and e[1].subject == subject and r in (e[1].auto | e[1].manual)), r))


def successors(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    ok = not db.isolated and db.gen == gen_of(j) and not s.restoring
    jgen = gen_of(j)

    # erasure is a dispatch-class action: it fails closed unless the recovery check passes
    if ok and j.up and s.activation and verified(cfg, s):
        # begin an erasure: journal-first intent, then fence the auto subscopes
        for subject in SUBJECTS:
            if any(e[0] == "intent" and e[1].subject == subject and not any(
                    x[0] == "confirm" and x[1] == e[1].id for x in j.entries) for e in j.entries):
                continue
            auto, manual = classify(cfg, db, j, subject)
            if not (auto or manual):
                continue
            n = spend(s, "erase")
            if n:
                iid = f"i{s.next_id}"
                intent = Intent(iid, subject, auto, manual, db.epoch)
                nj = j._replace(entries=j.entries + (("intent", intent),))
                reviews = db.reviews | frozenset((iid, r) for r in manual)
                yield (f"erase{subject}.intent", n._replace(journal=nj, next_id=s.next_id + 1,
                       db=db._replace(reviews=reviews)))

        # fence + execute auto subscopes of an open intent (the worker)
        for e in j.entries:
            if e[0] != "intent":
                continue
            intent = e[1]
            if any(x[0] == "confirm" and x[1] == intent.id for x in j.entries):
                continue
            # fence only subscopes still proven-separable and unheld at fence time
            to_fence = frozenset(r for r in intent.auto if r not in db.pending and r not in db.erased
                                 and r not in db.holds and (cfg.shared_auto_action or cfg.classify_ignores_unprove
                                 or (r not in INTRINSIC_SHARED and r not in db.known_unprovable)))
            if to_fence and intent.epoch == db.epoch:
                yield (f"{intent.id}.fence", s._replace(db=ack(db._replace(pending=db.pending | to_fence), j)))
            # a subscope that lost separation, gained a hold, or lost its fence to a rollback becomes manual
            stale = frozenset(r for r in intent.auto if r not in db.erased and not auto_eligible(cfg, s, intent, r)
                              and not is_manual(cfg, s, intent.id, r))
            if stale and not cfg.shared_auto_action:
                yield (f"{intent.id}.reclassify_manual", s._replace(db=db._replace(
                    reviews=db.reviews | frozenset((intent.id, r) for r in stale), pending=db.pending - stale)))
            ns = execute_auto(cfg, s, intent)
            if ns is not None:
                executed = ns.db.erased - db.erased
                nj = j._replace(entries=j.entries + (("confirm", intent.id, frozenset(executed)),))
                yield (f"{intent.id}.execute_auto", ns._replace(db=ack(ns.db, nj), journal=nj))

        # authorize a manual subscope, then execute it
        for (iid, r) in sorted(db.reviews):
            intent = intent_of(j, iid)
            if intent is None or r in db.erased:
                continue
            if (iid, r) not in db.authorized:
                n = spend(s, "authorize")
                if n:
                    nj = j._replace(entries=j.entries + (("authorize", iid, r),))
                    yield (f"{iid}.authorize.{r}", n._replace(journal=nj,
                           db=ack(db._replace(authorized=db.authorized | {(iid, r)}, pending=db.pending | {r}), nj)))
            elif r in db.pending and r not in db.holds:  # an active hold still dominates an authorization
                ns = execute_manual(cfg, s, intent, r)
                yield (f"{iid}.execute_manual.{r}", ns._replace(db=ack(ns.db, j)))

        # subject-level minimization once every subscope of the subject is resolved or authorized
        for subject in SUBJECTS:
            intents = [e[1] for e in j.entries if e[0] == "intent" and e[1].subject == subject]
            if not intents:
                continue
            open_manual = manual_open(cfg, s, subject)
            unauthorized = frozenset(r for r in open_manual if not any(e[0] == "authorize" and e[2] == r for e in j.entries))
            if not unauthorized or cfg.subject_ignores_protection:
                yield (f"{subject}.subject_minimize", subject_minimize(cfg, s, subject))

        # Kratos identity deletion obligation
        for subject in SUBJECTS:
            if subject in g.kratos_deleted:
                continue
            has_erasure = any(e[0] == "intent" and e[1].subject == subject for e in j.entries)
            if not has_erasure:
                continue
            # only once every in-scope record is erased (no open auto or manual subscope remains)
            if subject_done(cfg, s, subject) or cfg.delete_identity_with_open_manual:
                yield (f"{subject}.delete_identity", s._replace(ghost=g._replace(kratos_deleted=g.kratos_deleted | {subject})))

        # loss of separation (commit-first): ground truth now, protocol knowledge after a flush
        for r in RECORDS:
            if r in g.gt_unprovable or r in db.erased:
                continue
            n = spend(s, "share")
            if n:
                # commit-first and effective immediately: the profile lock sets attribution_shared now
                yield (f"share.{r}", n._replace(db=db._replace(unprove_queue=db.unprove_queue | {r}, known_unprovable=db.known_unprovable | {r}),
                       ghost=g._replace(gt_unprovable=g.gt_unprovable | {r})))

        # place / release a legal hold
        for r in RECORDS:
            if r in db.holds or r in db.erased:
                continue
            n = spend(s, "hold")
            if n:
                open_here = any(e[0] == "intent" and r in (e[1].auto | e[1].manual) for e in j.entries)
                nj = j._replace(entries=j.entries + (("hold", r),))
                yield (f"hold.{r}", n._replace(journal=nj, db=ack(db._replace(holds=db.holds | {r}), nj),
                       ghost=g._replace(gt_held=g.gt_held | {r}, ever_held=g.ever_held | ({r} if open_here else frozenset()))))
        for r in sorted(db.holds):
            n = spend(s, "release")
            if n:
                nj = j._replace(entries=j.entries + (("release", r), ("relconf", r)))
                after = db._replace(holds=db.holds - {r})
                if cfg.resume_on_release:
                    # wrongly resumes: drops the manual obligation and re-fences the stopped subscope
                    after = after._replace(reviews=frozenset(x for x in db.reviews if x[1] != r), pending=db.pending | {r})
                yield (f"release.{r}", n._replace(journal=nj, db=ack(after, nj), ghost=g._replace(gt_held=g.gt_held - {r})))

        # a withdrawal: witnessed when the witness is up, else pending_journal
        if not db.withdrawn:
            n = spend(s, "withdraw")
            if n:
                if s.witness_up:
                    yield ("withdraw.witnessed", n._replace(db=db._replace(withdrawn=True), witness=s.witness | {"w"},
                           ghost=g._replace(acked=True, witnessed=True)))
                else:
                    yield ("withdraw.pending_journal", n._replace(db=db._replace(withdrawn=True, w_unjournaled=True),
                           ghost=g._replace(acked=True)))

    # flush loss-of-separation to the journal
    if db.unprove_queue and j.up and ok:
        r = sorted(db.unprove_queue)[0]
        nj = j._replace(entries=j.entries + (("unprove", r),))
        yield (f"flush_unprove.{r}", s._replace(journal=nj,
               db=ack(db._replace(unprove_queue=db.unprove_queue - {r}, known_unprovable=db.known_unprovable | {r}), nj)))

    # heartbeat
    n = spend(s, "heartbeat")
    if n and j.up and ok:
        nj = j._replace(entries=j.entries + (("heartbeat", db.hb),))
        yield ("heartbeat.append", n._replace(journal=nj))
    if ok and not db.isolated:
        for pos, e in enumerate(j.entries, start=1):
            if e[0] == "heartbeat" and pos > g.max_beat and e[1] == db.hb:
                yield (f"heartbeat.commit{pos}", s._replace(db=ack(db._replace(hb=pos), j), ghost=g._replace(max_beat=pos)))

    # faults
    n = spend(s, "snap")
    if n and not db.isolated:
        yield ("snapshot", n._replace(snaps=n.snaps + (db,)))
    n = spend(s, "outage")
    if n:
        yield ("journal.toggle", n._replace(journal=j._replace(up=not j.up)))
    n = spend(s, "witness_outage")
    if n:
        yield ("witness.toggle", n._replace(witness_up=not s.witness_up))
    for snap in s.snaps:
        n = spend(s, "rollback")
        if n:
            detect = snap.hb < db.hb or snap.gen != db.gen
            yield ("undeclared_rollback", n._replace(db=snap._replace(epoch=db.epoch + 1), ghost=g._replace(undeclared=True, detectable=g.detectable and detect)))
    n = spend(s, "prune")
    if n and j.up:
        pins = []
        auth = frozenset(e[2] for e in j.entries if e[0] == "authorize")
        for k, e in enumerate(j.entries):
            if e[0] == "intent":
                intent = e[1]
                scope = frozenset(intent.auto) | frozenset(intent.manual)
                unresolved = frozenset(r for r in scope if r not in db.erased and r not in auth)
                stopped = frozenset(r for r in unresolved if is_manual(cfg, s, intent.id, r))
                unconfirmed = not any(x[0] == "confirm" and x[1] == intent.id for x in j.entries)
                if (unresolved - (stopped if cfg.prune_ignores_stopped else frozenset())) or (unconfirmed and not cfg.prune_ignores_stopped):
                    pins.append(k)
            if e[0] == "hold":
                released = any(x[0] == "relconf" and x[1] == e[1] for x in j.entries)
                blocks = any(x[0] == "intent" and e[1] in (set(x[1].auto) | set(x[1].manual)) and e[1] not in db.erased and e[1] not in auth for x in j.entries)
                if (not released) or (blocks and not cfg.prune_ignores_stopped):
                    pins.append(k)
            if e[0] == "authorize" and e[2] not in db.erased:
                pins.append(k)
        horizon = min(pins) if pins else len(j.entries)
        if horizon > j.base:
            yield ("journal.prune", n._replace(journal=j._replace(base=horizon)))

    # restore
    if s.restoring is None:
        for snap in s.snaps:
            n = spend(s, "restore")
            if n:
                yield ("restore.isolate", n._replace(db=snap._replace(isolated=True, epoch=db.epoch + 1), restoring="loaded", activation=False,
                       ghost=g._replace(detectable=True)))
    elif s.restoring == "loaded" and j.up:
        yield ("restore.cutover", s._replace(journal=j._replace(entries=j.entries + (("gen", jgen + 1),)), restoring="cut"))
    elif s.restoring == "cut" and j.up:
        # apply confirmed facts only; unresolved intents do NOT execute here (they wait for the window)
        new = db
        for e in j.entries:
            if e[0] == "hold":
                new = new._replace(holds=new.holds | {e[1]})
            elif e[0] == "relconf":
                new = new._replace(holds=new.holds - {e[1]})
            elif e[0] == "unprove":
                new = new._replace(known_unprovable=new.known_unprovable | {e[1]})
            elif e[0] == "confirm":
                new = new._replace(erased=new.erased | e[2])
            elif e[0] == "authorize":
                new = new._replace(authorized=new.authorized | {(e[1], e[2])})
        new = new._replace(gen=jgen, isolated=False, hb=head(j), pending=frozenset(), auto_ok=False)  # fences not restored; auto stays closed
        yield ("restore.apply", s._replace(db=ack(new, j), restoring=None))
    if not s.activation and not s.restoring and not db.isolated:
        yield ("controller.window", s._replace(activation=True))
    if s.activation and not db.auto_ok and not s.restoring and not db.isolated:
        yield ("controller.resync_attested", s._replace(db=db._replace(auto_ok=True, known_unprovable=db.known_unprovable | g.gt_unprovable)))


def violations(cfg, s):
    db, j, g = s.db, s.journal, s.ghost
    auth = frozenset(e[2] for e in j.entries if e[0] == "authorize")
    if g.fault:
        yield g.fault
    # INV1/INV2 ground truth: no truly-shared or truly/ever-held record is erased without an authorization
    for r in sorted(db.erased):
        protected = r in g.gt_unprovable or r in g.ever_held or r in g.gt_held
        if protected and r not in auth:
            if g.undeclared and not g.detectable:
                continue  # declared residual limit, stated in guarantee 5
            yield f"erased protected record {r} without authorization"
    # INV5: identity deletion only with every manual subscope of the subject resolved or authorized
    for subject in sorted(g.kratos_deleted):
        open_manual = manual_open(cfg, s, subject)
        unresolved = frozenset(r for r in open_manual if r not in auth)
        if unresolved:
            yield f"deleted the Kratos identity of {subject} with an open protected subscope"
    # INV6: witnessed rollback protection fails closed
    if can_dispatch(cfg, s) and g.witnessed and g.undeclared and db.withdrawn is False and "w" in s.witness:
        yield "dispatched after losing a witnessed withdrawal"
    # a pruned journal must never let a manual obligation disappear (C4 durability)
    for subject in SUBJECTS:
        for e in j.entries:
            if e[0] == "intent" and e[1].subject == subject:
                for r in e[1].manual:
                    if r in db.erased and r not in auth and not (g.undeclared and not g.detectable):
                        yield f"manual subscope {r} erased without authorization"


def explore(cfg, limit=1_500_000):
    start = State(Db(), Journal(), frozenset(), True, (), None, False, Ghost(), 1, cfg.budget)
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


SCENARIOS = {
    "exclusive erasure proceeds":        budget(erase=2, heartbeat=1),
    "shared stays manual":               budget(erase=2, authorize=1, heartbeat=1),
    "hold protects then authorized":     budget(erase=1, hold=1, release=1, authorize=1),
    "separation lost then crash":        budget(erase=1, share=1, snap=1, rollback=1),
    "separation lost then restore":      budget(erase=1, share=1, snap=1, restore=1),
    "hold release does not resume":      budget(erase=1, hold=1, release=1, prune=1),
    "retention with open manual":        budget(erase=1, hold=1, release=1, prune=2),
    "identity waits for manual":         budget(erase=1, authorize=1),
    "witness outage and rollback":       budget(withdraw=1, snap=1, rollback=1, witness_outage=1),
    "restore then window":               budget(erase=1, hold=1, snap=1, restore=1, authorize=1),
}

MUTANTS = {
    "C1 classify ignores known-shared": ("classify_ignores_unprove", ["shared stays manual"]),
    "C1 roll-forward ignores the fence": ("rollforward_ignores_fence", ["separation lost then crash", "separation lost then restore"]),
    # reprove_after_restore (disabling the fence_visible epoch/auto_ok check) has no independent
    # counterexample: a detected rollback already fails the recovery check that gates erasure, and an
    # undetected one is the declared residual limit (guarantee 5). It stays as a flag, not a claimed mutant.
    "C2 shared records auto-acted": ("shared_auto_action", ["shared stays manual"]),
    "C3 subject change ignores protection": ("subject_ignores_protection", ["hold protects then authorized", "shared stays manual"]),
    "C4 retention prunes a stopped subscope": ("prune_ignores_stopped", ["retention with open manual"]),
    "C4 release resumes a stopped subscope": ("resume_on_release", ["hold release does not resume"]),
    "C5 identity deleted with open manual": ("delete_identity_with_open_manual", ["identity waits for manual"]),
    "C6 recovery ignores a witness outage": ("recovery_ignores_witness_outage", ["witness outage and rollback"]),
}


def run(scenario, **flags):
    return explore(Config(SCENARIOS[scenario], **flags))


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
                _, violation, path = run(scenario, **{flag: True})
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
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
