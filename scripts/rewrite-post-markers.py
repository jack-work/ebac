#!/usr/bin/env python3
"""One-time rewrite of the old hidden post marker on GHE (Boss order 3, 2026-11-16).

Every post ebac made before the change ends in <!-- ebac:post:<critic>:<id> -->, which names the tool and the
critic. This rewrites each one in place to the neutral marker model.PostMarker computes,
<!-- post:<first 8 bytes of sha256("<critic>:<id>") as hex> -->, so the dedupe keeps finding old posts and ebac
never needs to read the old form.

Run it AFTER the new binary is live (the old binary searches the old marker for a post in flight).
Dry run by default; --apply edits. Needs gh logged in to the host as the posting account.

  rewrite-post-markers.py [--since 2026-10-20] [--org bic] [--apply]
"""
import argparse, datetime, hashlib, json, re, subprocess, sys

HOST = "microsoft.ghe.com"
LOGIN = "John-Kelliher"  # the search login of the posting account
OLD = re.compile(r"<!-- ebac:post:([^:\s]+):([A-Za-z0-9_-]+) -->")


def new_marker(critic, pid):
    return "<!-- post:%s -->" % hashlib.sha256(f"{critic}:{pid}".encode()).hexdigest()[:16]


def gh(args, data=None):
    r = subprocess.run(["gh", "api", "--hostname", HOST] + args, input=json.dumps(data) if data else None,
                       capture_output=True, text=True)
    if r.returncode:
        raise SystemExit(f"gh api {' '.join(args)}: {r.stderr.strip()}")
    return r.stdout


def pages(path, **params):
    args = ["-X", "GET", "--paginate", path] + sum((["-f", f"{k}={v}"] for k, v in params.items()), [])
    out = gh(args)
    dec, i, items = json.JSONDecoder(), 0, []
    while i < len(out):  # --paginate concatenates one JSON document per page
        while i < len(out) and out[i].isspace():
            i += 1
        if i >= len(out):
            break
        doc, i = dec.raw_decode(out, i)
        items += doc.get("items", []) if isinstance(doc, dict) else doc
    return items


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-10-20")
    ap.add_argument("--org", default="bic")
    ap.add_argument("--apply", action="store_true")
    a = ap.parse_args()

    # search returns at most 1000 hits per query: walk weekly windows of last-update date.
    prs, day, end = {}, datetime.date.fromisoformat(a.since), datetime.date.today()
    while day <= end:
        nxt = day + datetime.timedelta(days=7)
        q = f"commenter:{LOGIN} org:{a.org} is:pr updated:{day}..{nxt}"
        for it in pages("search/issues", q=q, per_page=100):
            repo = it["repository_url"].split("/repos/")[1]
            prs[(repo, it["number"])] = 1
        day = nxt + datetime.timedelta(days=1)
    print(f"{len(prs)} PRs with a comment by {LOGIN} since {a.since}", file=sys.stderr)

    todo = []  # (kind, repo, n, id, body)
    for repo, n in sorted(prs):
        for c in pages(f"repos/{repo}/issues/{n}/comments", per_page=100):
            todo.append(("issue", repo, n, c["id"], c.get("body") or "", c["user"]["login"]))
        for c in pages(f"repos/{repo}/pulls/{n}/comments", per_page=100):
            todo.append(("review_comment", repo, n, c["id"], c.get("body") or "", c["user"]["login"]))
        for c in pages(f"repos/{repo}/pulls/{n}/reviews", per_page=100):
            todo.append(("review", repo, n, c["id"], c.get("body") or "", c["user"]["login"]))

    hits = [t for t in todo if t[5].lower() == LOGIN.lower() and OLD.search(t[4])]
    print(f"{len(todo)} texts read, {len(hits)} carry the old marker", file=sys.stderr)
    done = 0
    for kind, repo, n, cid, body, _ in hits:
        nb = OLD.sub(lambda m: new_marker(m.group(1), m.group(2)), body)
        print(f"{kind}\t{repo}#{n}\t{cid}\t{OLD.search(body).group(0)} -> {new_marker(*OLD.search(body).groups())}")
        if not a.apply:
            continue
        if kind == "issue":
            gh(["-X", "PATCH", f"repos/{repo}/issues/comments/{cid}", "--input", "-"], {"body": nb})
        elif kind == "review_comment":
            gh(["-X", "PATCH", f"repos/{repo}/pulls/comments/{cid}", "--input", "-"], {"body": nb})
        else:
            gh(["-X", "PUT", f"repos/{repo}/pulls/{n}/reviews/{cid}", "--input", "-"], {"body": nb})
        done += 1
    print(f"{'rewrote' if a.apply else 'would rewrite'} {done if a.apply else len(hits)}", file=sys.stderr)


if __name__ == "__main__":
    main()
