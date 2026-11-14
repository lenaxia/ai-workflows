#!/usr/bin/env bash
# reconcile-ledger.sh — rebuild the review-gate finding ledger from the full
# verdict history (docs/review-gate-v1-spec.md §3). This is the convergence
# oracle: /merge refuses unless its JSON says converged=true, so it must be
# deterministic, byte-stable, and independent of prompt goodwill.
#
# Inputs (gh api): the PR object (author identity + head), ALL bot review
# bodies, and ALL PR comments. A "verdict body" is a bot-authored body
# containing a REVIEW-LEDGER v1 block — reviews count by block alone; marker
# comments additionally require the check-verdict.sh verdict marker
# (<!-- ai-workflows-verdict: ... sha=... -->) so a stray stats comment can
# never smuggle a ledger in. Bodies with no block are skipped (delivery
# validity is check-verdict.sh's job, §4); a body WITH a block that does not
# parse is a hard error: JSON {"error": ...} on stdout, exit 1 — a corrupt
# ledger must stop the merge, never shrink it silently.
#
# Line-based block grammar (v1, see the spec): REVIEW-LEDGER v1 ... exactly
# one END-REVIEW-LEDGER per body; scalar lines round/head/lane/verdict
# (validated: int / 40-hex / full|docs / approve|request-changes); repeatable
# coverage:; finding: lines with id/class/anchor/pointer/status/risk
# (fix_cost accepted, unconsumed) against the closed class and anchor lists;
# waive: id=...; reason=... lines. Prose fields must not contain ';' (the
# field separator). Parsed with python3 stdlib only — PyYAML is NOT assumed.
#
# Semantics:
#   - Verdicts are applied chronologically by (round, source order); a
#     finding id's last status wins (new/still-open → open, fixed → closed),
#     so advisories carried across rounds keep their ids per the spec.
#   - Latest verdict (max round) echoes as round/head/last_verdict — exactly
#     what the merge gate feeds back into head-check.
#   - Class→blocking map (script-enforced, spec §"Class→blocking map"):
#     full lane blocking = {correctness, robustness, security,
#     test-integrity}; docs lane blocking = {docs-truth}. A docs-lane finding
#     whose class is outside the docs map (e.g. correctness) is treated as
#     BLOCKING — fail-closed: the docs lane may not silently drop a
#     real-code finding. Docs-lane pointer-path invalidation is a delivery-
#     time rule (§4 check-verdict.sh); reconcile does not re-validate paths.
#   - Waivers: lines `waive <finding-id>: <reason>` from PR comments by the
#     PR author are the authoritative record (admin authority is not
#     machine-derivable and is left to humans); ledger `waive:` lines also
#     close advisory findings per the spec, applied before comment waivers so
#     the author's reason wins on conflict. Waivers apply ONLY to
#     advisory-class ids; a blocking-class attempt (or an unknown id — the
#     class cannot be verified, so fail-closed) is recorded in
#     rejected_waivers and the finding stays open. A waived id stays closed
#     even if a later verdict re-raises it (the reviewer contract says waived
#     findings are not re-raised; the script enforces the closure).
#   - converged = no open blocking AND no open advisory left (every advisory
#     is fixed or waived). Waiver attempts by non-authors are ignored
#     entirely.
#
# Output (stdout, exact spec shape, sort_keys + id-sorted arrays + fixed
# formatting → re-running on the same input is byte-identical):
#   {"round": <int>, "head": "<40-hex|empty>", "lane": "full|docs",
#    "open_blocking":  [{"id","class","anchor","pointer","risk"}...],
#    "open_advisory":  [{"id","class","anchor","pointer","risk"}...],
#    "waived":         [{"id","reason"}...],
#    "rejected_waivers": [{"id","reason","class"}...],
#    "converged": <bool>, "last_verdict": "approve|request-changes|empty"}
#
# Fixture mode for golden tests (no gh, no token, no network):
#   --from-file <dir> (or env REVIEW_STATE_FROM_FILE=<dir>) reads each gh
#   call's JSON verbatim from the named file; missing file = empty result:
#     pull.json     — GET repos/R/pulls/N           (object; reads .user.login)
#     reviews.json  — GET repos/R/pulls/N/reviews   (ARRAY, all pages merged)
#     comments.json — GET repos/R/issues/N/comments (ARRAY, all pages merged)
#   (Nested arrays of pages are flattened, so a live-mode --paginate | jq -s
#   dump and a hand-written flat array are both valid fixtures.)
#   The bot actor is env-overridable: REVIEW_BOT_ACTOR
#   (default github-actions[bot], same actor check-verdict.sh pins).
#
# Exit codes: 0 = reconciled (any convergence state), 1 = malformed ledger
# ({"error": ...} JSON printed), 2 = usage error.
#
# Usage:
#   reconcile-ledger.sh <pr> [--repo R] [--lane full|docs] [--from-file D]
#   lane defaults to env GATE_LANE, then full.

set -euo pipefail

REPO="${REPOSITORY:-}"
PR=""
LANE="${GATE_LANE:-full}"
FIXTURE="${REVIEW_STATE_FROM_FILE:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --repo)        [ $# -ge 2 ] || { printf 'reconcile-ledger.sh: --repo requires an operand\n' >&2; exit 2; }; REPO="$2"; shift 2 ;;
    --repo=*)      REPO="${1#*=}"; shift ;;
    --lane)        [ $# -ge 2 ] || { printf 'reconcile-ledger.sh: --lane requires an operand\n' >&2; exit 2; }; LANE="$2"; shift 2 ;;
    --lane=*)      LANE="${1#*=}"; shift ;;
    --from-file)   [ $# -ge 2 ] || { printf 'reconcile-ledger.sh: --from-file requires an operand\n' >&2; exit 2; }; FIXTURE="$2"; shift 2 ;;
    --from-file=*) FIXTURE="${1#*=}"; shift ;;
    -h|--help)     printf 'usage: reconcile-ledger.sh <pr> [--repo R] [--lane full|docs] [--from-file D]\n'; exit 0 ;;
    *) if [ -n "$PR" ]; then printf 'reconcile-ledger.sh: unexpected argument: %s\n' "$1" >&2; exit 2; fi; PR="$1"; shift ;;
  esac
done

[ -n "$PR" ] || { printf 'reconcile-ledger.sh: a PR number is required\n' >&2; exit 2; }
case "$LANE" in full|docs) ;; *) printf 'reconcile-ledger.sh: --lane must be full or docs, got %s\n' "$LANE" >&2; exit 2 ;; esac

if [ -z "$FIXTURE" ]; then
  [ -n "$REPO" ] || { printf 'reconcile-ledger.sh: --repo or REPOSITORY is required\n' >&2; exit 2; }
fi

# fetch_raw <route> <fixture-name> — full body of a paginated ARRAY endpoint.
# Fixture mode: the file verbatim (already all pages merged; the python side
# also flattens nested page arrays). Live mode: gh --paginate streams one
# array per page; jq -s collects them into a single array-of-pages document.
# A gh outage propagates: set -e -o pipefail exits non-zero BEFORE python
# runs — an API failure must never reconcile to converged.
fetch_raw() {
  if [ -n "$FIXTURE" ]; then
    if [ -f "$FIXTURE/$2" ]; then cat "$FIXTURE/$2"; fi
    return 0
  fi
  gh api "$1" --paginate | jq -s '.'
}

fetch_one() { # non-paginated single-document route; fixture default {}
  if [ -n "$FIXTURE" ]; then
    if [ -f "$FIXTURE/$2" ]; then cat "$FIXTURE/$2"; else printf '{}\n'; fi
    return 0
  fi
  gh api "$1"
}

TMPDIR_GATE="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_GATE"' EXIT
fetch_one "repos/$REPO/pulls/$PR" "pull.json" > "$TMPDIR_GATE/pull.json"
fetch_raw "repos/$REPO/pulls/$PR/reviews?per_page=100" "reviews.json" > "$TMPDIR_GATE/reviews.json"
fetch_raw "repos/$REPO/issues/$PR/comments?per_page=100" "comments.json" > "$TMPDIR_GATE/comments.json"

python3 - "$LANE" "$TMPDIR_GATE/pull.json" "$TMPDIR_GATE/reviews.json" "$TMPDIR_GATE/comments.json" <<'PYEOF'
import json
import os
import re
import sys


class LedgerError(Exception):
    """A verdict body carries a REVIEW-LEDGER v1 block that does not parse."""


BOT = os.environ.get("REVIEW_BOT_ACTOR", "github-actions[bot]")
LANE = sys.argv[1]
BLOCKING = {"full": {"correctness", "robustness", "security", "test-integrity"},
            "docs": {"docs-truth"}}
ADVISORY = {"full": {"docs-truth", "style"}, "docs": {"style"}}
CLASSES = BLOCKING["full"] | ADVISORY["full"]
STATUSES = {"new", "fixed", "still-open"}
HEAD_RE = re.compile(r"^[0-9a-f]{40}$")
ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
POINTER_RE = re.compile(r"^.+:[0-9]+$")
ANCHOR_RE = re.compile(r'^(issue:\d+|rule:\d+|claim:"[^"]*")$')
MARKER_RE = re.compile(
    r"<!-- ai-workflows-verdict: (?:APPROVE|REQUEST_CHANGES) sha=[0-9a-f]{40} -->")
WAIVER_LINE_RE = re.compile(r"^waive ([A-Za-z0-9][A-Za-z0-9._-]*): (.+)$")


def die(msg):
    # A corrupt ledger is a HARD stop for the merge gate: JSON error on
    # stdout (the gate parses stdout), non-zero exit.
    print(json.dumps({"error": msg}, sort_keys=True))
    sys.exit(1)


def load(path, what):
    with open(path, "r", encoding="utf-8") as f:
        text = f.read().strip()
    if not text:
        return []
    try:
        return json.loads(text)
    except ValueError as exc:
        die("invalid JSON from %s (%s): %s" % (os.path.basename(path), what, exc))


def flatten_pages(doc):
    """Accept a flat array, nested page arrays, or a single object."""
    if isinstance(doc, dict):
        return [doc]
    if not isinstance(doc, list):
        return []
    out = []
    for item in doc:
        if isinstance(item, list):
            out.extend(flatten_pages(item))
        elif isinstance(item, dict):
            out.append(item)
    return out


def login_of(obj):
    user = obj.get("user")
    if not isinstance(user, dict):
        return ""
    return user.get("login") or ""


def parse_kv(val, kind):
    fields = {}
    for part in val.split(";"):
        part = part.strip()
        if not part:
            continue
        key, eq, v = part.partition("=")
        if not eq:
            raise LedgerError("%s line has a part without k=v: %r" % (kind, part))
        key = key.strip()
        if key in fields:
            raise LedgerError("duplicate %s field %r" % (kind, key))
        fields[key] = v.strip()
    if kind == "finding":
        for req in ("id", "class", "anchor", "pointer", "status", "risk"):
            if req not in fields:
                raise LedgerError("finding line missing %s= field" % req)
        if fields["class"] not in CLASSES:
            raise LedgerError("finding class not in the closed list: %r" % fields["class"])
        if fields["status"] not in STATUSES:
            raise LedgerError("finding status not in the closed list: %r" % fields["status"])
        if not ID_RE.match(fields["id"]):
            raise LedgerError("malformed finding id: %r" % fields["id"])
        if not ANCHOR_RE.match(fields["anchor"]):
            raise LedgerError("anchor not in the closed list: %r" % fields["anchor"])
        if not POINTER_RE.match(fields["pointer"]):
            raise LedgerError("pointer must be <path>:<line>: %r" % fields["pointer"])
        unknown = set(fields) - {"id", "class", "anchor", "pointer", "status", "risk", "fix_cost"}
        if unknown:
            raise LedgerError("unknown finding field(s): %s" % ",".join(sorted(unknown)))
    else:  # waive
        for req in ("id", "reason"):
            if req not in fields:
                raise LedgerError("waive line missing %s= field" % req)
        if not ID_RE.match(fields["id"]):
            raise LedgerError("malformed waive id: %r" % fields["id"])
        unknown = set(fields) - {"id", "reason"}
        if unknown:
            raise LedgerError("unknown waive field(s): %s" % ",".join(sorted(unknown)))
    return fields


def parse_block(body):
    """Parse THE one REVIEW-LEDGER v1 block; None when the body has none."""
    lines = body.splitlines()
    starts = [i for i, ln in enumerate(lines) if ln.strip() == "REVIEW-LEDGER v1"]
    if not starts:
        return None
    if len(starts) > 1:
        raise LedgerError("body carries more than one REVIEW-LEDGER v1 block")
    s = starts[0]
    ends = [i for i, ln in enumerate(lines) if ln.strip() == "END-REVIEW-LEDGER" and i > s]
    if not ends:
        raise LedgerError("REVIEW-LEDGER v1 block missing END-REVIEW-LEDGER")
    scalars, coverage, findings, waives = {}, [], [], []
    for ln in lines[s + 1:ends[0]]:
        stripped = ln.strip()
        if not stripped:
            continue
        key, colon, val = stripped.partition(":")
        if not colon:
            raise LedgerError("block line without key: separator: %r" % stripped)
        key, val = key.strip(), val.strip()
        if key in ("round", "head", "lane", "verdict"):
            if key in scalars:
                raise LedgerError("duplicate %s: line" % key)
            scalars[key] = val
        elif key == "coverage":
            coverage.extend(p for p in re.split(r"[,\s]+", val) if p)
        elif key == "finding":
            findings.append(parse_kv(val, "finding"))
        elif key == "waive":
            waives.append(parse_kv(val, "waive"))
        else:
            raise LedgerError("unknown ledger key: %r" % key)
    for k in ("round", "head", "lane", "verdict"):
        if k not in scalars:
            raise LedgerError("ledger block missing %s: line" % k)
    if not re.fullmatch(r"\d+", scalars["round"]):
        raise LedgerError("round: must be a non-negative integer, got %r" % scalars["round"])
    if not HEAD_RE.match(scalars["head"]):
        raise LedgerError("head: must be a 40-char sha, got %r" % scalars["head"])
    if scalars["lane"] not in ("full", "docs"):
        raise LedgerError("lane: must be full or docs, got %r" % scalars["lane"])
    if scalars["verdict"] not in ("approve", "request-changes"):
        raise LedgerError("verdict: must be approve or request-changes, got %r" % scalars["verdict"])
    return {"round": int(scalars["round"]), "head": scalars["head"],
            "lane": scalars["lane"], "verdict": scalars["verdict"],
            "coverage": coverage, "findings": findings, "waives": waives}


def main():
    pull = load(sys.argv[2], "pull")
    reviews = flatten_pages(load(sys.argv[3], "reviews"))
    comments = flatten_pages(load(sys.argv[4], "comments"))
    author = login_of(pull) if isinstance(pull, dict) else ""

    # Verdict bodies in source order: reviews first, then marker comments.
    # Reviews qualify by ledger block alone; comments additionally need the
    # verdict marker (a stray comment must not smuggle a ledger in).
    verdicts = []
    seq = 0
    for rev in reviews:
        body = rev.get("body")
        if login_of(rev) != BOT or not isinstance(body, str) or not body.strip():
            continue
        try:
            block = parse_block(body)
        except LedgerError as exc:
            die("malformed ledger in bot review: %s" % exc)
        if block is not None:
            verdicts.append((block, seq))
            seq += 1
    for com in comments:
        body = com.get("body")
        if login_of(com) != BOT or not isinstance(body, str) or not body.strip():
            continue
        if not MARKER_RE.search(body):
            continue
        try:
            block = parse_block(body)
        except LedgerError as exc:
            die("malformed ledger in bot verdict comment: %s" % exc)
        if block is not None:
            verdicts.append((block, seq))
            seq += 1

    # Chronological replay: (round, source order). A fixed id re-raised by a
    # later verdict legitimately reopens (new status = the reviewer saw it
    # again); last status per id wins.
    state = {}
    for block, s in sorted(verdicts, key=lambda v: (v[0]["round"], v[1])):
        for f in block["findings"]:
            state[f["id"]] = {"id": f["id"], "class": f["class"],
                              "anchor": f["anchor"], "pointer": f["pointer"],
                              "risk": f["risk"], "open": f["status"] != "fixed"}

    blocking = BLOCKING[LANE]
    advisory = ADVISORY[LANE]

    # Waiver attempts: ledger waive: lines first (chronological), then the
    # AUTHOR's comment waivers (the authoritative record; their reason wins
    # on id conflicts). Blocking-class and unknown-id attempts fail closed
    # into rejected_waivers; non-author comments are ignored entirely.
    attempts = []
    for block, s in sorted(verdicts, key=lambda v: (v[0]["round"], v[1])):
        for w in block["waives"]:
            attempts.append((w["id"], w["reason"]))
    for com in comments:
        login = login_of(com)
        if login == BOT or not isinstance(com.get("body"), str):
            continue
        if author:
            if login != author:
                continue
        elif not login:
            continue
        for line in com["body"].splitlines():
            m = WAIVER_LINE_RE.match(line.strip())
            if m:
                attempts.append((m.group(1), m.group(2).strip()))

    waived, rejected = {}, {}
    for wid, reason in attempts:
        ent = state.get(wid)
        if ent is None:
            rejected[wid] = {"id": wid, "reason": reason, "class": "unknown"}
        elif ent["class"] in blocking:
            rejected[wid] = {"id": wid, "reason": reason, "class": ent["class"]}
        else:
            waived[wid] = reason

    def finding_view(ent):
        return {"id": ent["id"], "class": ent["class"], "anchor": ent["anchor"],
                "pointer": ent["pointer"], "risk": ent["risk"]}

    open_all = [e for e in state.values() if e["open"] and e["id"] not in waived]
    # Fail-closed map: a class outside the lane's advisory set blocks, so the
    # docs lane can never silently drop a real-code finding.
    open_blocking = sorted((finding_view(e) for e in open_all
                            if e["class"] not in advisory), key=lambda f: f["id"])
    open_advisory = sorted((finding_view(e) for e in open_all
                            if e["class"] in advisory), key=lambda f: f["id"])
    converged = not open_blocking and not open_advisory

    if verdicts:
        latest = max(verdicts, key=lambda v: (v[0]["round"], v[1]))[0]
        round_out, head_out, last_verdict = latest["round"], latest["head"], latest["verdict"]
    else:
        round_out, head_out, last_verdict = 0, "", ""

    out = {
        "round": round_out,
        "head": head_out,
        "lane": LANE,
        "open_blocking": open_blocking,
        "open_advisory": open_advisory,
        "waived": [{"id": k, "reason": waived[k]} for k in sorted(waived)],
        "rejected_waivers": [rejected[k] for k in sorted(rejected)],
        "converged": converged,
        "last_verdict": last_verdict,
    }
    print(json.dumps(out, indent=2, sort_keys=True, ensure_ascii=False))


main()
PYEOF
