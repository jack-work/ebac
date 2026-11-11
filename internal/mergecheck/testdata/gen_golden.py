#!/usr/bin/env python3
"""Regenerate golden.json from minister-control's states.py: the Go port must give the same checklist and state.

    python3 gen_golden.py ~/dev/reports/minister-control ~/notes/decisions-minister/RULE-chiqun-merge-criteria.md

Case inputs are the previous golden's (a check-ni PASS added where band jobs were not the point) plus the cases
below; steps and state are whatever states.py says. Commit the states.py sha it was made from in the message.
"""
import copy, json, pathlib, sys

mc, rule = pathlib.Path(sys.argv[1]).expanduser(), pathlib.Path(sys.argv[2]).expanduser()
sys.path.insert(0, str(mc))
import states  # noqa: E402

states.POLICY = rule
here = pathlib.Path(__file__).parent
old = json.loads((here / "golden.json").read_text())
CHECK_NI = {"id": 13, "queue": "aether.check-ni", "verdict": "PASS", "state": "done"}
BAND = {"band job failed", "band job running", "zero tests", "no fast job"}

cases = []
for c in old["cases"]:
    if c["name"].startswith("+"):
        continue  # added below
    c = {k: c[k] for k in ("name", "facts", "jobs", "row", "item", "tests", "arch")} | {"files": c.get("files")}
    if c["name"] not in BAND and not any(j["queue"] == "aether.check-ni" for j in c["jobs"]):
        c["jobs"] = [CHECK_NI] + c["jobs"]
    if c["name"] == "ready":
        c["facts"] = c["facts"] | {"merge_state": "CLEAN", "review": "APPROVED"}
    cases.append(c)

base = next(c for c in cases if c["name"] == "ready")


def case(name, files=None, jobs=None, tests=None, **facts):
    c = copy.deepcopy(base)
    c["name"], c["files"] = "+" + name, files
    if jobs is not None:
        c["jobs"] = jobs
    if tests is not None:
        c["tests"] = tests
    c["facts"] = c["facts"] | facts
    cases.append(c)


ni = lambda v="PASS", st="done", i=21: {"id": i, "queue": "aether.check-ni", "verdict": v, "state": st}
job = lambda q, v, i, t="": {"id": i, "queue": "aether." + q, "verdict": v, "state": "done" if v else "leased"} | ({"tests": t} if t else {})
case("108334 tests-only, check-ni PASS", files=["aether_rust/crates/aether_grpc/src/effort_stamp_tests.rs"], jobs=[ni()])
case("108717 code, check-ni PASS, opt-in gate-kit FAIL", files=["aether_rust/crates/x/src/lib.rs", "aether_rust/crates/x/tests/a.rs"],
     jobs=[job("gate-kit", "FAIL", 30), ni()])
case("108459 code+docs, check-ni PASS, clippy-pr FAIL", files=["docs/x.md", "aether_rust/crates/x/src/a.rs"],
     jobs=[job("clippy-pr", "FAIL", 31), ni()])
case("docs-only, no job", files=["README.md", "aether_rust/docs/design.md"], jobs=[])
case("tests-only, no job", files=["aether_rust/crates/x/tests/t.rs", "py/test_x.py"], jobs=[])
case("check-ni running", files=["a.rs"], jobs=[ni(None, "leased")])
case("check-ni FAIL, older PASS", files=["a.rs"], jobs=[ni("FAIL", "failed", 41), ni("PASS", "done", 40)])
case("opt-in with tests", files=["a.rs"], jobs=[job("gate-ext", "PASS", 50, "decision_judge"), ni()], tests={"50": 412})
case("files not read", files=None, jobs=[ni()])
case("empty file list is code", files=[], jobs=[])
case("review required", files=["a.rs"], jobs=[ni()], merge_state="CLEAN", review="REVIEW_REQUIRED")
case("merge state unknown", files=["a.rs"], jobs=[ni()], merge_state="UNKNOWN", review="")
nf = copy.deepcopy(base)
nf["name"], nf["files"], nf["row"] = "+fyi evidence", ["a.rs"], nf["row"] | {"chiqun_notified": {"at": "2026-11-11T03:00Z", "chat": "dm"}}
cases.append(nf)

pol = states.policy()
out = {"rule": rule.read_text(), "self_check": states.self_check(pol), "cases": []}
for c in cases:
    f = c["facts"]
    steps = (states.checklist(pol, f, c["jobs"], c["row"], c["item"], {int(k): v for k, v in c["tests"].items()}, c["arch"], c["files"])
             if f.get("heavy") and f.get("state") == "OPEN" else None)
    out["cases"].append(c | {"steps": steps, "state": states.state_of(f, steps)})
(here / "golden.json").write_text(json.dumps(out, indent=1) + "\n")
print(len(out["cases"]), "cases;", {c["name"]: c["state"] for c in out["cases"]})
