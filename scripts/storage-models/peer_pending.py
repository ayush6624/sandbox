#!/usr/bin/env python3
"""PeerPending extension of the atomic-catalog bounded model."""
from collections import deque
from dataclasses import dataclass, replace

import model


@dataclass(frozen=True)
class PeerCatalog(model.Catalog):
    availability: str = "PeerPending"


@dataclass(frozen=True)
class PeerState(model.State):
    cat: PeerCatalog = PeerCatalog()
    p: int = 2  # root publication starts independently of backup
    b: int = 0
    b_seen: PeerCatalog | None = None
    peer: bool = True  # journal-pinned immutable source exists initially


def transitions(s, mutant=None):
    if s.p == 2:
        yield "P read catalog before exposing PeerPending", replace(s, p=3, p_seen=s.cat)
    elif s.p == 3:
        if s.p_seen == s.cat:
            cat = model.change_holders(s.cat, add="root", phase="Live")
            yield "P CAS attach root while PeerPending", replace(s, p=4, p_seen=None, cat=cat)
        else:
            yield "P attachment CAS conflict", replace(s, p=2, p_seen=None)
    elif s.p == 4:
        assert s.root == "Absent"
        yield "P publish PeerPending root", replace(s, p=6, root="Live")
        yield "P publish PeerPending root, response lost", replace(s, p=5, root="Live", lost=True)
    elif s.p == 5:
        yield "P resolve root PUT using exact root/tombstone", replace(s, p=6)
    elif s.p == 6 and (s.b == 8 or mutant == "early-publisher-release"):
        yield "P read catalog to release publisher", replace(s, p=7, p_seen=s.cat)
    elif s.p == 7:
        if s.p_seen == s.cat:
            cat = model.change_holders(s.cat, remove="publisher")
            yield "P CAS release publisher", replace(s, p=8, p_seen=None, cat=cat)
        else:
            yield "P release CAS conflict", replace(s, p=6, p_seen=None)

    # Backup starts only after offer visibility, including a lost offer response.
    # Request dispatch, remote commit, and ambiguous-response resolution differ.
    if s.root != "Absent":
        if s.b == 0:
            yield "B dispatch payload PUT", replace(s, b=1)
        elif s.b == 1:
            yield "B payload commits; response received", replace(s, b=3, data=True)
            yield "B payload commits; response lost", replace(s, b=2, data=True)
        elif s.b == 2:
            yield "B resolve ambiguous payload PUT", replace(s, b=3)
        elif s.b == 3:
            yield "B dispatch manifest PUT", replace(s, b=4)
        elif s.b == 4:
            yield "B manifest commits; response received", replace(s, b=6, manifest=True)
            yield "B manifest commits; response lost", replace(s, b=5, manifest=True)
        elif s.b == 5:
            yield "B resolve ambiguous manifest PUT", replace(s, b=6)
        elif s.b == 6:
            yield "B read catalog to commit CloudReady", replace(s, b=7, b_seen=s.cat)
        elif s.b == 7:
            if s.b_seen == s.cat:
                cat = replace(s.cat, availability="CloudReady", revision=s.cat.revision + 1)
                yield "B CAS CloudReady after all PUTs resolve", replace(s, b=8, b_seen=None, cat=cat)
            else:
                yield "B readiness CAS conflict", replace(s, b=6, b_seen=None)

    # Existing reader, retirement, and collector logic is reused without invoking
    # its CloudReady-only publication actor. Restore the actual publisher PC.
    for event, nxt in model.transitions(replace(s, p=8), mutant=False):
        if event.startswith("P "):
            continue
        nxt = replace(nxt, p=s.p)
        if event.startswith("T tombstone"):
            nxt = replace(nxt, retired_after_loss=s.retired_after_loss or s.p == 5)
        yield event, nxt

    # Abstract CacheReady || peer expiry as already satisfied. The remaining
    # Published && BackupComplete guards must hold even in that strongest case.
    if s.peer and s.p >= 6 and (s.b == 8 or mutant == "early-peer-cleanup"):
        yield "S remove retained peer source", replace(s, peer=False)


def violation(s, check_publisher=True):
    if len(s.cat.holders) > model.CAPACITY:
        return "holder capacity exceeded"
    if s.cat.phase in {"Retired", "Deleting", "Deleted"} and s.cat.holders:
        return "retired catalog retained holders"
    if s.cat.phase == "Deleted" and (s.data or s.manifest):
        return "collection marked Deleted while late-upload bytes remained"
    if check_publisher and s.b < 8 and "publisher" not in s.cat.holders:
        return "publisher released while backup uploads remain unresolved"
    if check_publisher and s.p < 8 and "publisher" not in s.cat.holders:
        return "publisher lost protection before publication completion"
    cloud_route = s.cat.availability == "CloudReady" and s.data and s.manifest
    route = s.peer or cloud_route
    if s.root == "Live" and ("root" not in s.cat.holders or not route):
        return "live PeerPending root lost its holder or last readable route"
    for i, pc in enumerate(s.readers):
        if pc in (3, 4, 5) and (f"reader{i}" not in s.cat.holders or not route):
            return "returned reader lost protection or last readable route"
    return None


def explore(mutant=None, check_publisher=True):
    initial = PeerState()
    parents = {initial: None}
    queue = deque([initial])
    terminal = 0
    coverage = set()
    while queue:
        s = queue.popleft()
        problem = violation(s, check_publisher)
        if problem:
            trace = []
            while parents[s] is not None:
                previous, event = parents[s]
                trace.append(event)
                s = previous
            return len(parents), terminal, coverage, problem, list(reversed(trace))
        if s.cat.phase == "Deleted" and s.p == 8 and s.b == 8 and s.t == 3 and s.readers == (6, 6) and not s.peer:
            terminal += 1
        active = [pc in (3, 4, 5) for pc in s.readers]
        if all(active) and s.b < 8:
            coverage.add("two returned readers before CloudReady")
        if any(active) and s.root == "Tombstone" and s.b in (1, 2, 4, 5):
            coverage.add("reader survives root retirement with unresolved backup PUT")
        if s.retired_after_loss and s.b < 8:
            coverage.add("lost root response then retirement before backup completion")
        if any(active) and not s.peer:
            coverage.add("reader continues from cloud after peer cleanup")
        for event, nxt in transitions(s, mutant):
            if event == "B payload commits; response lost":
                coverage.add("lost payload PUT response")
            if event == "B manifest commits; response lost":
                coverage.add("lost manifest PUT response")
            if "recover persisted" in event:
                coverage.add("collector resumes persisted Deleting")
            if nxt not in parents:
                parents[nxt] = (s, event)
                queue.append(nxt)
    return len(parents), terminal, coverage, None, []


def main():
    count, terminal, coverage, failure, trace = explore()
    assert failure is None, (failure, trace)
    assert terminal > 0
    assert len(coverage) == 7, coverage
    print(f"PASS PeerPending atomic catalog: {count} states; {terminal} reclaimed terminal states")
    for item in sorted(coverage):
        print(f"  covered: {item}")
    for mutant in ("early-publisher-release", "early-peer-cleanup"):
        count, _, _, failure, trace = explore(mutant)
        assert failure is not None
        print(f"COUNTEREXAMPLE {mutant}: {failure}; {count} states")
        for number, event in enumerate(trace, 1):
            print(f"  {number:02}. {event}")
    count, _, _, failure, trace = explore("early-publisher-release", check_publisher=False)
    assert failure == "collection marked Deleted while late-upload bytes remained", (failure, trace)
    print(f"COUNTEREXAMPLE early-release downstream effect: {failure}; {count} states")
    for number, event in enumerate(trace, 1):
        print(f"  {number:02}. {event}")
    print("Bound: one generation, publisher, backup worker, durable root, two readers,")
    print("one peer source, four holders, two independently ambiguous upload responses.")


if __name__ == "__main__":
    main()
