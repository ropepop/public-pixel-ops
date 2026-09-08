#!/usr/bin/env python3
"""Read-only, reproducible Ticket implementation inventory across both checkouts.

The baseline is an immutable list of files and revisions, not a moving glob.
New source in the Ticket roots counts; increases elsewhere also count so moving
code to a shared helper cannot make it disappear from the release budget.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

OPS = Path(__file__).resolve().parents[1]
ROOTS = {"ops": OPS, "pixel": OPS.parent / "pixel-phone"}
SOURCE = {".go", ".rs", ".js", ".ts", ".mjs", ".kt", ".java", ".c", ".cpp", ".h", ".html", ".tmpl", ".css", ".sh", ".py", ".svg", ".Dockerfile"}


def git(root, *args):
    return subprocess.check_output(["git", *args], cwd=root).decode()


def source_file(name):
    path = Path(name)
    return (path.suffix in SOURCE
            and not any(part in path.parts for part in ("generated", "module_bindings", "node_modules", "target", "build", "archive", "test", "tests", "androidTest", "runtime-tests", ".ai"))
            and not path.name.startswith("test_")
            and path.name != "source_footprint.py"
            and not path.name.endswith(("_test.go", ".test.mjs", ".test.ts", ".test.js"))
            and not ("/internal/web/static/" in name and path.suffix == ".js")
            and not name.endswith("/diagnostic/hdr-diagnostic.js"))


def ticket_file(repo, name):
    if repo == "ops":
        return (name.startswith(("workloads/ticket-remote/", "tools/ticket/"))
                or name.startswith("infra/arbuzas/docker/images/") and Path(name).name.startswith("ticket"))
    return ("/src/main/" in name and ("/ticket/" in name or "ticket" in Path(name).name.lower())
            or name.startswith(("orchestrator/scripts/android/", "ops/scripts/", "tools/observability/", "tools/pixel/"))
            and "ticket" in Path(name).name.lower())


def lines(text, name):
    rows = text.splitlines()
    # These Rust modules place their test-only module at the end. Fail rather
    # than silently omit production code if that convention changes.
    if name.endswith(".rs") and "#[cfg(test)]" in rows:
        at = rows.index("#[cfg(test)]")
        tail = "\n".join(rows[at + 1:]).strip()
        if not tail.startswith("mod tests {"):
            raise ValueError(f"Review embedded test boundary: {name}")
        depth = 0
        # Count production conservatively if test code is not a trailing module.
        # The frozen inventory records the excluded boundary for review.
        if tail.endswith("}"):
            rows = rows[:at]
    return {"physical": len(rows), "nonblank": sum(bool(row.strip()) for row in rows)}


def inventory():
    result = {"revisions": {}, "files": {}}
    for repo, root in ROOTS.items():
        result["revisions"][repo] = git(root, "rev-parse", "HEAD").strip()
        for name in git(root, "ls-files", "--cached", "--others", "--exclude-standard").splitlines():
            if not source_file(name) or not ticket_file(repo, name):
                continue
            path = root / name
            if not path.is_file():
                continue
            text = path.read_text()
            result["files"][f"{repo}:{name}"] = {
                **lines(text, name), "sha256": hashlib.sha256(text.encode()).hexdigest()
            }
    result["total"] = {kind: sum(file[kind] for file in result["files"].values()) for kind in ("physical", "nonblank")}
    return result


def compare(baseline):
    current = inventory()
    # The initial inventory omitted dedicated helpers outside the workload roots.
    # Recover their original sizes from the SAME frozen commits, without editing
    # the frozen artifact or increasing its release ceiling. Current helpers are
    # fully charged; the audited totals expose the correction for the final report.
    omissions = {}
    for repo, root in ROOTS.items():
        revision = baseline["revisions"][repo]
        for name in git(root, "ls-tree", "-r", "--name-only", revision).splitlines():
            key = f"{repo}:{name}"
            if key not in baseline["files"] and source_file(name) and ticket_file(repo, name):
                original = git(root, "show", f"{revision}:{name}")
                omissions[key] = lines(original, name)
    # Full additions outside the workload are charged unless clearly test/build
    # tooling; existing shared files are charged for their positive net growth.
    outside = {}
    for repo, root in ROOTS.items():
        revision = baseline["revisions"][repo]
        changed = set(git(root, "diff", "--name-only", revision).splitlines())
        changed.update(git(root, "ls-files", "--others", "--exclude-standard").splitlines())
        for name in sorted(changed):
            if not source_file(name) or ticket_file(repo, name) or name == "tools/ticket-code-size.py":
                continue
            path = root / name
            if not path.is_file():
                continue
            before = subprocess.run(["git", "show", f"{revision}:{name}"], cwd=root, capture_output=True)
            old = lines(before.stdout.decode() if before.returncode == 0 else "", name)
            new = lines(path.read_text(), name)
            growth = {kind: max(0, new[kind] - old[kind]) for kind in old}
            if any(growth.values()):
                outside[f"{repo}:{name}"] = growth
    total = {kind: current["total"][kind] + sum(row[kind] for row in outside.values()) for kind in current["total"]}
    audited_baseline = {kind: baseline["total"][kind] + sum(row[kind] for row in omissions.values()) for kind in total}
    return {"baseline": baseline["total"], "current": total, "shared_growth": outside,
            "baseline_omissions": omissions, "audited_baseline": audited_baseline,
            "audited_reduction_percent": {kind: round(100 * (1 - total[kind] / audited_baseline[kind]), 2) for kind in total},
            "reduction_percent": {kind: round(100 * (1 - total[kind] / baseline["total"][kind]), 2) for kind in total},
            "passes": all(total[kind] * 2 <= baseline["total"][kind] for kind in total)}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path)
    args = parser.parse_args()
    output = compare(json.loads(args.baseline.read_text())) if args.baseline else inventory()
    print(json.dumps(output, indent=2, sort_keys=True))
    if args.baseline and not output["passes"]:
        raise SystemExit(1)
