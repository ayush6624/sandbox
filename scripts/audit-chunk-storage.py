#!/usr/bin/env python3
"""Read GCS object metadata on a worker and report storage and manifest sharing.

Uses the attached GCE service account. Never writes or deletes cloud objects.
The report is an observation during concurrent operation, not a GC root set.
"""

import argparse
import hashlib
import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request


class StorageReader:
    def __init__(self, bucket):
        request = urllib.request.Request(
            "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
            headers={"Metadata-Flavor": "Google"},
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            self.token = json.load(response)["access_token"]
        self.base = "https://storage.googleapis.com/storage/v1/b/" + urllib.parse.quote(bucket, safe="")

    def get(self, path, query):
        request = urllib.request.Request(
            self.base + path + "?" + urllib.parse.urlencode(query),
            headers={"Authorization": "Bearer " + self.token},
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    def page(self, token, count):
        return self.get("/o", {"maxResults": count, "pageToken": token,
            "fields": "nextPageToken,items(name,size,generation)"})

    def object_json(self, item):
        return self.get("/o/" + urllib.parse.quote(item["name"], safe=""),
            {"alt": "media", "ifGenerationMatch": item["generation"]})


def manifest_object(name):
    parts = name.split("/")
    return (len(parts) == 3 and parts[0] == "hib" and parts[2] == "manifest.json"
        or len(parts) == 4 and parts[0] == "handoff" and parts[3] == "record.json"
        or len(parts) == 3 and parts[0] == "handoff" and parts[2] == "control.json")


def manifest_set(item, body):
    parts = item["name"].split("/")
    if parts[0] == "hib":
        return "/".join(parts[:2]), body
    if parts[-1] == "record.json":
        return "/".join(parts[:3]), body["manifest"]
    descriptor = body.get("descriptor")
    if descriptor is None:
        return None
    generation = descriptor["ref"]["generation"]
    return "/".join(parts[:2]) + "/" + generation, descriptor["manifest"]


def measure(reader, max_objects, max_manifests, seconds):
    start = time.monotonic()
    report = {"started_at_unix": time.time(), "listing_complete": False,
        "manifest_scan_complete": False, "pages": 0, "objects": 0,
        "live_object_bytes": 0, "prefixes": {}, "errors": [],
        "scope": "Current object versions only; excludes soft-deleted/noncurrent versions, local journals and active reader closures. Not a deletion plan."}
    chunks, manifests = {}, []
    token = ""
    seen_tokens = set()
    while report["objects"] < max_objects and time.monotonic() - start < seconds:
        page = reader.page(token, min(1000, max_objects - report["objects"]))
        report["pages"] += 1
        for item in page.get("items", []):
            name, size = item["name"], int(item["size"])
            if size < 0:
                raise ValueError("negative object size")
            prefix = name.split("/", 1)[0]
            totals = report["prefixes"].setdefault(prefix, {"objects": 0, "bytes": 0})
            totals["objects"] += 1
            totals["bytes"] += size
            report["objects"] += 1
            report["live_object_bytes"] += size
            if re.fullmatch(r"chunks/[0-9a-f]{64}", name):
                chunks[name[7:]] = size
            if manifest_object(name):
                manifests.append(item)
        token = page.get("nextPageToken", "")
        if not token:
            report["listing_complete"] = True
            break
        if token in seen_tokens:
            raise ValueError("object listing repeated a page token")
        seen_tokens.add(token)

    sets = {}
    for item in manifests[:max_manifests]:
        if time.monotonic() - start >= seconds:
            break
        try:
            result = manifest_set(item, reader.object_json(item))
            if result is None:
                continue
            identity, manifest = result
            hashes = {entry["hash"] for entry in manifest["chunks"] if entry["hash"] != "zero"}
            if any(re.fullmatch(r"[0-9a-f]{64}", h) is None for h in hashes):
                raise ValueError("invalid manifest hash")
            digest = hashlib.sha256(json.dumps(manifest, sort_keys=True).encode()).hexdigest()
            if identity in sets and sets[identity][0] != digest:
                raise ValueError("same generation has conflicting observed manifests")
            sets[identity] = (digest, hashes)
        except (urllib.error.URLError, ValueError, KeyError, TypeError) as error:
            report["errors"].append({"object": item["name"], "error": str(error)})
    report["manifest_objects_found"] = len(manifests)
    report["manifest_scan_complete"] = (report["listing_complete"] and len(manifests) <= max_manifests
        and time.monotonic() - start < seconds and not report["errors"])
    unique = set().union(*(hashes for _, hashes in sets.values()))
    shared = sum(chunks.get(h, 0) for h in unique)
    separate = sum(sum(chunks.get(h, 0) for h in hashes) for _, hashes in sets.values())
    report["observed_manifests"] = {"sets": len(sets), "unique_nonzero_hashes": len(unique),
        "hashes_missing_from_listing": len(unique - chunks.keys()),
        "listed_shared_compressed_bytes": shared,
        "listed_compressed_bytes_without_cross_set_dedup": separate,
        "cross_set_multiplier": separate / shared if shared else None,
        "interpretation": "Includes historical descriptors; not proof of live retention or future namespace cost."}
    report["elapsed_seconds"] = time.monotonic() - start
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bucket", required=True)
    parser.add_argument("--max-objects", type=int, default=250000)
    parser.add_argument("--max-manifests", type=int, default=1000)
    parser.add_argument("--seconds", type=int, default=300)
    parser.add_argument("--bucket-settings-only", action="store_true",
        help="Read retention, versioning, and lifecycle settings without listing objects")
    args = parser.parse_args()
    if min(args.max_objects, args.max_manifests, args.seconds) <= 0:
        parser.error("limits must be positive")
    try:
        reader = StorageReader(args.bucket)
        if args.bucket_settings_only:
            settings = reader.get("", {"fields": "name,location,storageClass,versioning,softDeletePolicy,retentionPolicy,lifecycle"})
            print(json.dumps({"bucket": args.bucket, "bucket_settings": settings}, indent=2))
            return
        report = measure(reader, args.max_objects, args.max_manifests, args.seconds)
        report["bucket"] = args.bucket
        print(json.dumps(report, indent=2))
    except (urllib.error.URLError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({"bucket": args.bucket, "listing_complete": False, "error": str(error)}, indent=2))
        raise SystemExit(1)


if __name__ == "__main__":
    main()
