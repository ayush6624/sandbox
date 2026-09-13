#!/usr/bin/env python3
"""Bounded executable state exploration; Python stdlib only."""
from collections import deque
from dataclasses import dataclass, replace

CAPACITY = 4  # publisher, durable root, two readers

@dataclass(frozen=True)
class Catalog:
    phase: str = "Publishing"
    holders: frozenset = frozenset({"publisher"})
    revision: int = 0

@dataclass(frozen=True)
class State:
    cat: Catalog = Catalog()
    root: str = "Absent"
    data: bool = False
    manifest: bool = False
    p: int = 0
    p_seen: Catalog | None = None
    lost: bool = False
    retired_after_loss: bool = False
    t: int = 0
    t_seen: Catalog | None = None
    readers: tuple = (0, 0)
    r_seen: tuple = (None, None)
    c: int = 0
    c_seen: Catalog | None = None


def change_holders(cat, *, add=None, remove=None, phase=None):
    holders = set(cat.holders)
    if add is not None:
        holders.add(add)
    if remove is not None:
        holders.discard(remove)
    if len(holders) > CAPACITY:
        return None
    return replace(cat, phase="Retired" if not holders else (phase or cat.phase),
                   holders=frozenset(holders), revision=cat.revision + 1)


def reader_state(s, i, pc, seen=None, cat=None):
    readers, observations = list(s.readers), list(s.r_seen)
    readers[i], observations[i] = pc, seen
    return replace(s, readers=tuple(readers), r_seen=tuple(observations),
                   cat=s.cat if cat is None else cat)


def transitions(s, mutant=False):
    # Root attachment is in the same CAS as the publication-ready transition.
    if s.p == 0:
        yield "P write payload", replace(s, p=1, data=True)
    elif s.p == 1:
        yield "P write manifest last", replace(s, p=2, manifest=True)
    elif s.p == 2:
        yield "P read catalog for durable attachment", replace(s, p=3, p_seen=s.cat)
    elif s.p == 3:
        if s.p_seen == s.cat:
            cat = change_holders(s.cat, add="root", phase="Live")
            yield "P CAS attach durable holder and become Live", replace(s, p=4, p_seen=None, cat=cat)
        else:
            yield "P attachment CAS conflict", replace(s, p=2, p_seen=None)
    elif s.p == 4:
        assert s.root == "Absent"
        yield "P create-only root PUT succeeds", replace(s, p=6, root="Live")
        yield "P root PUT succeeds, response lost", replace(s, p=5, root="Live", lost=True)
    elif s.p == 5:
        # A create-only retry cannot replace Tombstone. No second publication.
        yield "P resolve ambiguous PUT by exact root/tombstone read", replace(s, p=6)
    elif s.p == 6:
        yield "P read catalog to release publisher", replace(s, p=7, p_seen=s.cat)
    elif s.p == 7:
        if s.p_seen == s.cat:
            cat = change_holders(s.cat, remove="publisher")
            yield "P CAS release after all writes complete", replace(s, p=8, p_seen=None, cat=cat)
        else:
            yield "P release CAS conflict", replace(s, p=6, p_seen=None)

    if s.t == 0 and s.root == "Live":
        yield "T tombstone root before detaching", replace(
            s, root="Tombstone", t=1,
            retired_after_loss=s.retired_after_loss or s.p == 5)
    elif s.t == 1:
        yield "T read catalog", replace(s, t=2, t_seen=s.cat)
    elif s.t == 2:
        if s.t_seen == s.cat:
            cat = change_holders(s.cat, remove="root")
            yield "T CAS detach root; retire atomically if last", replace(s, t=3, t_seen=None, cat=cat)
        else:
            yield "T detach CAS conflict", replace(s, t=1, t_seen=None)

    for i, pc in enumerate(s.readers):
        name = f"reader{i}"
        if pc == 0:
            yield f"R{i} read root", reader_state(s, i, 1 if s.root == "Live" else 6)
        elif pc == 1:
            yield f"R{i} read catalog", reader_state(s, i, 2, s.cat)
        elif pc == 2:
            seen = s.r_seen[i]
            if "root" not in seen.holders or seen.phase != "Live":
                yield f"R{i} acquisition rejected", reader_state(s, i, 6)
            elif seen != s.cat and not mutant:
                yield f"R{i} acquisition CAS conflict", reader_state(s, i, 1)
            else:
                # Mutant trusts a prior observation and overwrites newer catalog.
                cat = change_holders(seen if mutant else s.cat, add=name)
                if cat is not None:
                    yield f"R{i} {'UNCONDITIONAL PUT' if mutant else 'CAS'} acquire; return handle", reader_state(s, i, 3, cat=cat)
        elif pc == 3:
            yield f"R{i} use data and finish work", reader_state(s, i, 4)
        elif pc == 4:
            yield f"R{i} read catalog to release", reader_state(s, i, 5, s.cat)
        elif pc == 5:
            if s.r_seen[i] == s.cat:
                cat = change_holders(s.cat, remove=name)
                yield f"R{i} CAS release; retire atomically if last", reader_state(s, i, 6, cat=cat)
            else:
                yield f"R{i} release CAS conflict", reader_state(s, i, 4)

    if s.c == 0:
        yield "C read catalog", replace(s, c=1, c_seen=s.cat)
    elif s.c == 1:
        if s.c_seen != s.cat:
            yield "C stale observation; reread", replace(s, c=0, c_seen=None)
        elif s.cat.phase == "Retired":
            cat = replace(s.cat, phase="Deleting", revision=s.cat.revision + 1)
            yield "C CAS Retired -> Deleting", replace(s, c=2, c_seen=None, cat=cat)
        elif s.cat.phase == "Deleting":
            yield "C recover persisted Deleting", replace(s, c=2, c_seen=None)
        elif s.cat.phase == "Deleted":
            yield "C observe deletion complete", replace(s, c=5, c_seen=None)
        else:
            yield "C referenced generation; retry later", replace(s, c=0, c_seen=None)
    elif s.c == 2:
        yield "C delete generation payload and manifest", replace(s, c=3, data=False, manifest=False)
        yield "C crash before DELETE; replacement restarts", replace(s, c=0, c_seen=None)
    elif s.c == 3:
        yield "C read deletion completion catalog", replace(s, c=4, c_seen=s.cat)
        yield "C DELETE response lost; replacement restarts", replace(s, c=0, c_seen=None)
    elif s.c == 4:
        if s.c_seen == s.cat and s.cat.phase == "Deleting":
            cat = replace(s.cat, phase="Deleted", revision=s.cat.revision + 1)
            yield "C CAS Deleting -> Deleted", replace(s, c=5, c_seen=None, cat=cat)
        else:
            yield "C completion CAS conflict", replace(s, c=0, c_seen=None)


def violation(s):
    if len(s.cat.holders) > CAPACITY:
        return "holder capacity exceeded"
    if s.cat.phase in {"Retired", "Deleting", "Deleted"} and s.cat.holders:
        return "retired catalog retained holders"
    if s.root == "Live" and (not s.data or not s.manifest or "root" not in s.cat.holders):
        return "live durable root lost its bytes or holder"
    for i, pc in enumerate(s.readers):
        if pc in (3, 4, 5) and (not s.data or not s.manifest or f"reader{i}" not in s.cat.holders):
            return "returned reader lost bytes or protection before release"
    if s.p < 8 and "publisher" not in s.cat.holders:
        return "publisher lost protection before completion"
    return None


def explore(mutant=False):
    initial = State()
    parents = {initial: None}
    queue = deque([initial])
    terminal = 0
    coverage = set()
    while queue:
        s = queue.popleft()
        problem = violation(s)
        if problem:
            trace = []
            while parents[s] is not None:
                previous, event = parents[s]
                trace.append(event)
                s = previous
            return len(parents), terminal, coverage, problem, list(reversed(trace))
        if s.cat.phase == "Deleted" and s.p == 8 and s.t == 3 and s.readers == (6, 6):
            terminal += 1
        if all(pc in (3, 4, 5) for pc in s.readers):
            coverage.add("two simultaneous returned readers")
        if s.retired_after_loss:
            coverage.add("lost root response followed by cross-host retirement")
        for event, nxt in transitions(s, mutant):
            if "recover persisted" in event:
                coverage.add("collector restart with persisted Deleting")
            if "CAS conflict" in event:
                coverage.add("stale observations rejected")
            if nxt not in parents:
                parents[nxt] = (s, event)
                queue.append(nxt)
    return len(parents), terminal, coverage, None, []


def main():
    count, terminal, coverage, failure, trace = explore()
    assert failure is None, (failure, trace)
    assert terminal > 0
    assert len(coverage) == 4, coverage
    print(f"PASS atomic catalog: {count} states; {terminal} reclaimed terminal states")
    for item in sorted(coverage):
        print(f"  covered: {item}")
    count, _, _, failure, trace = explore(mutant=True)
    assert failure is not None
    print(f"COUNTEREXAMPLE unconditional-acquisition mutant: {failure}; {count} states")
    for number, event in enumerate(trace, 1):
        print(f"  {number:02}. {event}")
    print("Bound: one generation, one publisher/root, two readers, four holders.")


if __name__ == "__main__":
    main()
