#!/usr/bin/env python3
"""Bounded placeholder-generation write-fence exploration; stdlib only."""
from collections import deque
from dataclasses import dataclass, replace


@dataclass(frozen=True, slots=True, order=True)
class Request:
    kind: str
    expected: int
    status: str = "pending"  # then received or lost; provider already decided


@dataclass(frozen=True, slots=True)
class State:
    obj: str = "absent"  # absent, empty placeholder, nonempty payload
    generation: int = 0
    next_generation: int = 1
    known_placeholder: int = 0
    requests: tuple = ()
    alive: bool = True
    drained: bool = False
    publisher: bool = True
    root: bool = True  # PeerPending durable-root attachment
    collector: int = 0  # 0 waiting; 1 read; 2 delete request; 3 provider; 4 done
    observed: int = 0


def canonical(requests):
    # Identical retries are interchangeable; retain multiplicity.
    return tuple(sorted(requests))


def transitions(s, mutant=None):
    producing = s.alive and not s.drained
    creates = sum(r.kind == "empty" for r in s.requests)
    payloads = sum(r.kind == "payload" for r in s.requests)
    if producing:
        if creates < 2:
            req = Request("empty", 0)
            yield "P dispatch EMPTY create-only PUT/retry", replace(s, requests=canonical(s.requests + (req,)))
        if s.known_placeholder and payloads < 2:
            expected = 0 if mutant == "payload-create-only" else s.known_placeholder
            req = Request("payload", expected)
            yield f"P dispatch PAYLOAD PUT/retry expecting g{expected}", replace(s, requests=canonical(s.requests + (req,)))
        if s.obj == "empty" and s.known_placeholder != s.generation:
            yield f"P read EMPTY metadata; remember g{s.generation}", replace(s, known_placeholder=s.generation)
        if all(r.status == "received" for r in s.requests):
            # Explicitly stop issuing before releasing. An abort may drain an
            # empty request set; successful work may drain after its responses.
            yield "P join all requests and stop issuing writes", replace(s, drained=True)
        yield "F authoritative producer process death", replace(s, alive=False)

    # A lost response does not mean the request never reached the provider.
    # Dispatch and provider commit are independent scheduler transitions.
    seen_requests = set()
    for index, req in enumerate(s.requests):
        if req.status != "pending" or req in seen_requests:
            continue
        seen_requests.add(req)
        matches = (s.generation == req.expected if req.expected else s.obj == "absent")
        outcomes = ("received", "lost") if s.alive else ("lost",)
        for outcome in outcomes:
            requests = list(s.requests)
            requests[index] = replace(req, status=outcome)
            nxt = replace(s, requests=canonical(requests))
            if matches:
                nxt = replace(nxt, obj=req.kind, generation=s.next_generation,
                              next_generation=s.next_generation + 1)
                if req.kind == "empty" and outcome == "received" and producing:
                    nxt = replace(nxt, known_placeholder=s.next_generation)
            result = f"commits g{s.next_generation}" if matches else "fails 412"
            yield f"G {req.kind} PUT expecting g{req.expected} {result}; response {outcome}", nxt

    if s.root:
        yield "T retire root attachment ONLY", replace(s, root=False)
    if s.publisher and (s.drained or not s.alive or (mutant == "retirement-releases-live-producer" and not s.root)):
        yield "F discharge publisher holder", replace(s, publisher=False)

    if s.collector == 0 and not s.publisher and not s.root:
        yield "C CAS last-holder Retired -> Deleting", replace(s, collector=1)
    elif s.collector == 1:
        observed = 0 if mutant == "skip-empty-placeholders" and s.obj == "empty" else s.generation
        yield f"C read current object generation g{s.generation}", replace(s, collector=2, observed=observed)
    elif s.collector == 2:
        if s.observed == 0:
            yield "C observed absence; mark collection complete", replace(s, collector=4, observed=0)
        else:
            yield f"C dispatch DELETE expecting g{s.observed}", replace(s, collector=3)
    elif s.collector == 3:
        if s.generation == s.observed:
            yield f"G DELETE g{s.observed} succeeds; collection complete", replace(
                s, obj="absent", generation=0, collector=4, observed=0)
        elif s.obj == "absent":
            yield "G DELETE observes absence; collection complete", replace(s, collector=4, observed=0)
        elif mutant == "delete-mismatch-is-complete":
            yield "G DELETE fails 412; MUTANT declares completion", replace(s, collector=4, observed=0)
        else:
            yield "G DELETE fails 412; collector must reread", replace(s, collector=1, observed=0)
        # A restart rereads object state and repeats generation-conditioned work.
        yield "C crash before DELETE; replacement resumes", replace(s, collector=1, observed=0)


def violation(s, check_live_holder=True):
    if check_live_holder and s.alive and not s.drained and not s.publisher:
        return "root retirement discharged a still-live producer"
    if s.collector and (s.publisher or s.root):
        return "collector entered beneath a retained holder"
    if s.collector == 4 and s.obj == "payload":
        return "nonempty payload exists after completed collection"
    return None


def explore(mutant=None, check_live_holder=True):
    initial = State()
    parents = {initial: None}
    queue = deque([initial])
    coverage = set()
    completed = empty_residue = 0
    while queue:
        s = queue.popleft()
        failure = violation(s, check_live_holder)
        if failure:
            trace = []
            while parents[s] is not None:
                previous, event = parents[s]
                trace.append(event)
                s = previous
            return len(parents), completed, empty_residue, coverage, failure, list(reversed(trace))
        if s.collector == 4:
            completed += 1
            if s.obj == "empty":
                empty_residue += 1
                coverage.add("late empty placeholder after collection")
        if not s.root and s.alive and not s.drained and s.publisher:
            coverage.add("root retirement retains live producer")
        if not s.alive and any(r.status == "pending" for r in s.requests):
            coverage.add("accepted requests remain pending after death")
        if sum(r.kind == "payload" for r in s.requests) == 2:
            coverage.add("two payload attempts with captured preconditions")
        for event, nxt in transitions(s, mutant):
            if "payload PUT" in event and "fails 412" in event and s.collector == 4:
                coverage.add("late payload rejected after collection")
            if "response lost" in event:
                coverage.add("provider commit with lost response")
            if "DELETE fails 412; collector must reread" in event:
                coverage.add("payload wins delete race; collector retries new generation")
            if nxt not in parents:
                parents[nxt] = (s, event)
                queue.append(nxt)
    return len(parents), completed, empty_residue, coverage, None, []


def main():
    states, completed, residue, coverage, failure, trace = explore()
    assert failure is None, (failure, trace)
    assert completed > 0 and residue > 0
    assert len(coverage) == 7, coverage
    print(f"PASS exact-placeholder fence: {states} states; {completed} completed-collection states")
    print(f"  empty-object residue occurs in {residue} completed-collection states; zero payload bytes")
    for item in sorted(coverage):
        print(f"  covered: {item}")
    cases = (
        ("payload-create-only", True),
        ("delete-mismatch-is-complete", True),
        ("skip-empty-placeholders", True),
        ("retirement-releases-live-producer", True),
        ("retirement-releases-live-producer", False),
    )
    for mutant, strict in cases:
        states, _, _, _, failure, trace = explore(mutant, strict)
        assert failure is not None
        suffix = "" if strict else " downstream effect"
        print(f"COUNTEREXAMPLE {mutant}{suffix}: {failure}; {states} states")
        for number, event in enumerate(trace, 1):
            print(f"  {number:02}. {event}")
    print("Bound: one key, one producer/root/collector, two EMPTY and two PAYLOAD attempts.")
    print("Each attempt dispatches separately from provider commit; response received or lost.")


if __name__ == "__main__":
    main()
