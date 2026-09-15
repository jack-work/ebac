# ebac

Snapshot GitHub pull requests into figaro forms, compute the delta, and wake a
seat only when the delta earns it.

```
GitHub ──poll──▶ snapshot (on disk, full)  ──diff──▶ delta ──▶ role form ──▶ figaro seat
                                                                  ▲
                                                       systemd timer, one per critic
```

## The three decisions everything else follows from

**1. The form is a mailbox, not a database.** Measured 2026-08-26, against
docs that claim otherwise:

- A studied form is **not** re-sent whole on each change. A `study:@x` block
  carries only the keys that moved. (Changing a 28-char key cost +110
  cache-write tokens while an untouched 20 KB key sat in the same form;
  changing that 20 KB key cost +2139.)
- Each value **is** truncated, at **2046 characters**, with a trailing `…`.
  Boundary measured exactly: 2046 visible, 2047 elided.

So bodies stay in `snapshots/<critic>.json` — but because of **data loss**, not
token cost. That is acceptable because it is detectable: a cut string ends in
`…`, and a cut object is unbalanced and unparseable. Every seat's `harness`
contract says to re-read with `figaro form <id> -j` on either sign.

**2. The critic form *is* the role.** One form per critic, carrying both the
state and `target-aria`. Succession is `ebac cast --critic W --aria <new>`;
because `target-aria` resolves late and per call, the next heartbeat lands on
the successor and the timer never learns anything changed.

**3. Silence is the default.** An event must reach tier 1 to cost a turn.
Bots, your own comments, resolutions by others and pushes to PRs with no
outstanding feedback are recorded and never delivered.

## Quick start

```sh
go install ./...
ebac init                                    # systemd template units

ebac add --name aether --repo acme/widget --author @me \
    --mode develop --mint --outfit sonnet --issue-repo acme/widget

ebac poll --critic aether --dry-run           # reconcile, persist, alert nobody
systemctl --user enable --now ebac@aether.timer
```

Dry-run first, always. It performs the full round and persists observed state
but never invokes figaro, so the **first armed round is quiet instead of an
avalanche**.

## Grouping

A critic holds one PR or many; a role covers exactly one critic. Group PRs that
one seat should reason about together:

```sh
ebac add --name aether-core   --pr .../pull/6622 --pr .../pull/6640 --mint
ebac add --name aether-churn  --repo acme/widget --author @me --mint
```

`--repo` makes the set live: any new matching PR joins on its next round, and
one that leaves the filter is retained until it closes — a PR does not stop
mattering because a label moved.

## The fleet view

`ebac roster --sync` rebuilds a second form, `ebac-roster`, holding every
active critic with its seat, health and counts. Cast an overseer into it and
that aria critics the whole fleet:

```sh
ebac roster --sync --mint
```

The roster is rebuilt wholesale by one writer, never patched per-critic:
concurrent pollers appending rows to a shared form is a lost-update bug waiting
for a busy afternoon.

## Modes

| Mode | The seat's charge |
|---|---|
| `review` | Read the delta, tell the operator only what matters, ack, stay quiet. |
| `develop` | Treat every delta as a claim to be falsified: hunt false positives, **false absences**, and mistiering; run `ebac selfcheck`; make one focused improvement with a test; file harness defects with `ebac issue`. |

Both modes are **read-only on the pull requests by default**. The brief says so in
its own section, because an agent with GitHub credentials and a vague charge will
eventually decide that commenting would be helpful.

## Write

A critic can be granted authority to act on *its own* PRs:

```
ebac add --name N --pr URL --write        # at creation
ebac set-write --critic N --on | --off    # afterwards, takes effect next round
```

This is **per-critic and opt-in**, never a fleet default. A global flip would
authorize every critic on every PR it watches, which is exactly the failure the
read-only brief prevents; granting it one critic at a time keeps the decision
named and reviewable (`ebac ls` renders the mode as `review+w`).

Write covers **reply to threads, comment, and push to the head branch**. It does
not cover **approve, merge or close** — those stay unreachable in both modes,
because they are irreversible and a seat that can merge its own work has no
reviewer. PRs outside the critic remain read-only regardless.

Use it when the seat is the author and is expected to answer its own review
feedback. Leave it off for observation.

A write-enabled brief carries an extra `HOW TO ACT` section: converge rather
than churn, validate every finding at the head before accepting it, reject
tangential asks with a reason, cite individual threads, ask reviewers for a
line budget, keep rationale out of the source, and never claim an unrun test.
A seat with write and a vague charge answers every finding with a commit,
which draws another review; the section exists to stop that loop.

It also carries `SET THE PROTOCOL EARLY`: state a line budget with its reason,
list what is out of scope, number the open questions, freeze the code while
they are open, and keep one linked thread. Reviewers calibrate to what you give
them, so a PR that opens with a wall of rationale gets walls back. When a
reviewer ignores the terms, the brief tells the seat to say so once, warmly,
leading with what the review got right. The goal is the same eyes and fewer
words, never fewer findings.

## Stopping and archival

```
--ttl 7d              wall-clock deadline
--max-rounds 500      hard round cap
--archive-grace 6h    keep polling after the last PR closes
--no-archive          never retire automatically
ebac stop --critic W --reason "..."
```

When every PR is closed or merged and the grace elapses, ebac sends the seat
one closing notice, disables the timer, and moves the critic plus its final
snapshot into `archive/<name>-<stamp>/`. **The evidence outlives the critic**: a
reaped monitor that leaves no corpse cannot be audited afterwards.

## Robustness properties, and the incident behind each

- **A partial scan never infers a deletion.** One failed page marks the
  snapshot incomplete and the differ suppresses every removal, reporting the
  suppression as a distinct result. "The instrument could not see" and "nothing
  was there" must never render identically.
- **Observation and delivery are separate ledgers.** The snapshot advances even
  when a send fails; the undelivered events accumulate as a debt, deduplicated
  and capped. Conflating them re-reports every event on every failed round.
- **The debt is persisted before figaro is invoked.** A crash mid-alert is
  recoverable; the batch clears only on a confirmed send. Five retries, 30s
  doubling to a 30m cap.
- **Binaries are pinned absolute at `add` time.** A systemd unit has no
  interactive `PATH`. Measured here: the first armed round died with
  `env: 'bash': No such file or directory`.
- **Edits are caught by body digest, not timestamp.** Some edits do not move
  `updatedAt`.
- **Form keys are top-level and written whole.** `Figaro.SetJSON` refuses a
  dotted key. See below.
- **Adoption does not detonate.** A PR joining a critic with thirty standing
  threads produces one wake event, not thirty.
- **`--all` polls sequentially** and returns the first error while continuing;
  one broken critic does not stop the others.

### The figaro form hazard this tool is built around

Measured on this box, 2026-08-25, and not documented anywhere:

```sh
figaro set --id $F k '{"a":1}'   # k = {"a":1}
figaro set --id $F k.a 2         # → {"k":{"a":1}, "k.a":2}   ← a FLAT decoy
figaro unset --id $F k.a         # reports success, removes the DECOY
```

A dotted path nests correctly only when no prefix of it already holds a value.
Write a subtree as a JSON literal and every later dotted write into it lands as
a flat sibling that shadows nothing; overwrite a dotted-built subtree with a
literal and the old leaves are orphaned into flat garbage the same way.

ebac therefore writes **top-level keys only, always to a complete value**,
and anything a seat writes back is a **flat scalar** (`ack_round`, `ack_note`)
reached through `ebac ack`. `selfcheck` check 6 detects the decoy signature
in case it ever happens anyway.

## Selfcheck

Seven checks, ≤20 lines of output, each printing what it *measured*:

```
ebac selfcheck --strict
```

`UNTESTABLE` is a distinct verdict from `WARN` and carries the number it would
have warned about — a check that cannot tell "not evaluable" from "failing"
trains an operator to ignore warnings, and that costs more than any defect.

## Layout

| Path | What |
|---|---|
| `critics/<name>.json` | definition, stop conditions, runtime, delivery debt |
| `snapshots/<name>.json` | full canonical PR state — the diff input |
| `archive/<name>-<stamp>/` | retired critic plus its final snapshot |
| `roster.json` | id of the fleet-view form |

Root is `$EBAC_STATE`, else `$XDG_STATE_HOME/ebac`.

## Prior art

`prangl` (the Windows fleet, documented under `skills/angl/prangl/`) solved
this once already. Its reconciliation lifecycle, its retry policy, its
never-infer-deletions-from-a-partial-scan rule, and its scope discipline
(review comments and thread resolution only — not CI, not merge state, not
staleness) are all reused deliberately.
