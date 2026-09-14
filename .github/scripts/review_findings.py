"""Pure transforms between the model's findings.json and the GitHub
Reviews API payload.

No network, no filesystem, no GitHub calls — everything here is a function
of its arguments so it can be unit-tested off-CI. post_review.py does the
I/O.
"""

import json
import re

VERDICTS = ("blocker", "should-fix", "nit", "clean")
SEVERITIES = ("blocker", "should-fix", "nit")
BLOCKING = ("blocker", "should-fix")


class ValidationError(ValueError):
    """The model produced a document we refuse to act on."""


def _require(cond, msg):
    if not cond:
        raise ValidationError(msg)


def validate(doc):
    """Raise ValidationError unless doc matches the findings.json contract."""
    _require(isinstance(doc, dict), "top level must be an object")
    _require(doc.get("verdict") in VERDICTS,
             f"verdict must be one of {VERDICTS}, got {doc.get('verdict')!r}")
    _require(isinstance(doc.get("summary"), str) and doc["summary"].strip(),
             "summary must be a non-empty string")
    # doc["summary"] is the first line of the rendered review body, ahead
    # of the trailing <!-- ghost-review:<sha> --> marker build_review
    # appends — a forged '<!--' here is an even better decoy position
    # than a finding field for a consumer's re.search (first match).
    _require("<!--" not in doc["summary"],
             "summary must not contain an HTML comment marker")
    findings = doc.get("findings")
    _require(isinstance(findings, list), "findings must be a list")

    for i, f in enumerate(findings):
        where = f"findings[{i}]"
        _require(isinstance(f, dict), f"{where} must be an object")
        for key in ("file", "title", "body"):
            _require(isinstance(f.get(key), str) and f[key].strip(),
                     f"{where}.{key} must be a non-empty string")
        # A finding's text is interpolated into the review body alongside
        # the trailing <!-- ghost-review:<sha> --> marker build_review
        # appends. Without this check, a forged '<!--' in title/body/
        # suggestion could plant a decoy marker ahead of the real one, and
        # a consumer using re.search (first match) would read the
        # attacker's sha instead. A legitimate code review never needs to
        # emit a raw HTML comment opener.
        for key in ("title", "body"):
            _require("<!--" not in f[key],
                     f"{where}.{key} must not contain an HTML comment marker")
        path = f["file"]
        _require(not path.startswith("/") and ".." not in path.split("/"),
                 f"{where}.file must be a repo-relative path, got {path!r}")
        # A filename containing '<!--' is legal on disk but never
        # legitimate here, and build_review interpolates f['file'] into
        # the nits and dropped-findings lines of the review body.
        _require("<!--" not in path,
                 f"{where}.file must not contain an HTML comment marker")
        _require(f.get("severity") in SEVERITIES,
                 f"{where}.severity must be one of {SEVERITIES}")
        # bool is a subclass of int; reject it explicitly.
        line = f.get("line")
        _require(isinstance(line, int) and not isinstance(line, bool) and line > 0,
                 f"{where}.line must be a positive integer")
        end = f.get("end_line")
        if end is not None:
            _require(isinstance(end, int) and not isinstance(end, bool),
                     f"{where}.end_line must be an integer")
            _require(end >= line, f"{where}.end_line must be >= line")
        sug = f.get("suggestion")
        if sug is not None:
            _require(isinstance(sug, str), f"{where}.suggestion must be a string")
            _require("<!--" not in sug,
                     f"{where}.suggestion must not contain an HTML comment marker")
    return doc


_HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def _unquote_git_path(target):
    """Undo git's core.quotePath=true C-style quoting of a diff path.

    A non-ASCII (or otherwise "unusual") filename is rendered by git as a
    double-quoted string with UTF-8 bytes octal-escaped, e.g.
    "b/caf\\303\\251.txt" for café.txt. Left as-is, the quotes/backslashes/
    octal escapes stay in the key and findings against that file can never
    anchor.
    """
    if target.startswith('"') and target.endswith('"'):
        target = (target[1:-1]
                  .encode("latin-1", "backslashreplace")
                  .decode("unicode_escape")
                  .encode("latin-1")
                  .decode("utf-8", "replace"))
    return target


def parse_hunks(diff_text):
    """Map repo-relative path -> set of RIGHT-side line numbers in the diff.

    GitHub only accepts review comments anchored to a line that appears in
    the diff, which means added ('+') or context (' ') lines. Deleted files
    (+++ /dev/null) contribute nothing.
    """
    hunks = {}
    path = None
    new_line = 0
    saw_dash_header = False

    for raw in diff_text.splitlines():
        if raw.startswith("diff --git "):
            path, new_line = None, 0
            saw_dash_header = False
            continue
        if raw.startswith("--- "):
            saw_dash_header = True
            continue
        if saw_dash_header and raw.startswith("+++ "):
            # Only treat a line as the '+++' file header when the line
            # immediately before it was the '--- ' header. Otherwise an
            # added line whose own text starts with '++ ' becomes
            # '+++ ...' once the diff's leading '+' marker is prepended,
            # and would be mistaken for a bogus new file header.
            saw_dash_header = False
            target = _unquote_git_path(raw[4:].strip())
            path = None if target == "/dev/null" else re.sub(r"^b/", "", target)
            continue
        saw_dash_header = False
        m = _HUNK_RE.match(raw)
        if m:
            new_line = int(m.group(1))
            continue
        if path is None or new_line == 0:
            continue
        if raw.startswith("+") or raw.startswith(" ") or raw == "":
            hunks.setdefault(path, set()).add(new_line)
            new_line += 1
        # '-' lines consume no RIGHT-side number; '\' (no newline) is inert.
    return hunks


MARKER = "<!-- ghost-review:{sha} -->"

_LABEL = {"blocker": "🔴 blocker", "should-fix": "🟠 should-fix", "nit": "🔵 nit"}


def partition(findings):
    """Split findings into (blocking, nits) per the severity policy."""
    blocking = [f for f in findings if f.get("severity") in BLOCKING]
    nits = [f for f in findings if f.get("severity") == "nit"]
    return blocking, nits


def _longest_backtick_run(text):
    return max((len(m) for m in re.findall(r"`+", text)), default=0)


def _render_comment(f):
    parts = [f"**{_LABEL[f['severity']]} — {f['title']}**", "", f["body"]]
    sug = f.get("suggestion")
    if sug is not None:
        sug = sug.rstrip("\n")
        # A suggestion containing its own triple backticks would otherwise
        # close the ```suggestion fence early and inject arbitrary
        # Markdown into the rendered review. Open/close with a fence
        # longer than the longest backtick run inside the suggestion, the
        # standard Markdown technique for fencing code that itself
        # contains fences.
        fence = "`" * max(3, _longest_backtick_run(sug) + 1)
        parts += ["", f"{fence}suggestion", sug, fence]
    return "\n".join(parts)


def _anchor(f, hunks):
    """Return a Reviews-API comment dict, or None if it is not in the diff."""
    lines = hunks.get(f["file"])
    if not lines:
        return None
    start = f["line"]
    end = f.get("end_line") or start
    if start not in lines or end not in lines:
        return None
    comment = {
        "path": f["file"],
        "line": end,
        "side": "RIGHT",
        "body": _render_comment(f),
    }
    if end != start:
        comment["start_line"] = start
        comment["start_side"] = "RIGHT"
    return comment


def build_review(doc, commit_id, hunks):
    """Return (review_payload, dropped_findings).

    Blocking findings become inline comments; nits and un-anchorable
    findings go in the review body so nothing is silently lost.
    """
    blocking, nits = partition(doc["findings"])

    comments, dropped = [], []
    for f in blocking:
        anchored = _anchor(f, hunks)
        if anchored is None:
            dropped.append(f)
        else:
            comments.append(anchored)

    body = [doc["summary"], ""]
    body.append(f"**Verdict:** `{doc['verdict']}` — "
                f"{len(comments)} inline finding(s), {len(nits)} nit(s).")

    if nits:
        body += ["", "<details><summary>Nits "
                 f"({len(nits)}) — non-blocking</summary>", ""]
        for f in nits:
            loc = f"`{f['file']}:{f['line']}`"
            body.append(f"- {loc} **{f['title']}** — {f['body']}")
        body += ["", "</details>"]

    if dropped:
        body += ["", f"**{len(dropped)} finding(s) could not be anchored** "
                 "to a line in this diff and are reported here instead:", ""]
        for f in dropped:
            loc = f"`{f['file']}:{f['line']}`"
            body.append(f"- {_LABEL[f['severity']]} {loc} "
                        f"**{f['title']}** — {f['body']}")

    body += ["", MARKER.format(sha=commit_id)]

    payload = {
        "commit_id": commit_id,
        "body": "\n".join(body),
        "event": "COMMENT",
        "comments": comments,
    }
    return payload, dropped


def extract_findings(text):
    """Pull the findings document out of a model's free-form reply.

    The model has no write tools by design — its only output channel is
    stdout — so the JSON has to be recovered from whatever prose, fenced
    blocks, or event wrappers surround it. Tries a standard-library JSON
    decode anchored at every '{' in the reply, independently of every
    other position, and returns the LAST such decode that produced a
    dict with a 'verdict' key — which survives a model that reasons in
    prose before answering, wraps the answer in ```json, or restates a
    partial object mid-explanation. Because each attempt is local (no
    shared string/escape state across the whole reply), one unbalanced
    quote earlier in the model's prose cannot discard a valid object
    that follows it.

    Raises ValidationError when nothing usable is present, so a garbled
    reply fails the job loudly instead of posting a degraded review.

    Deviation from a literal "try every '{' independently" scan: once a
    position decodes successfully, the scan resumes after that object
    instead of also probing the positions inside it — otherwise a
    document with N nested braces costs O(N) decode attempts each
    doing O(N) work. And a RecursionError (the C JSON decoder's stack
    guard, hit by adversarially deep nesting) aborts the whole scan
    immediately rather than being retried one character over: nearly
    every remaining position in such a reply is just as deeply nested,
    so retrying them one by one is a multi-minute stall for the same
    verdict (ValidationError), not a chance at a different answer.
    """
    decoder = json.JSONDecoder()
    candidates = []
    tried = 0
    i = 0
    n = len(text)

    while i < n:
        if text[i] != "{":
            i += 1
            continue
        tried += 1
        try:
            doc, end = decoder.raw_decode(text, i)
        except RecursionError:
            raise ValidationError(
                "model reply contains JSON nested too deeply to parse "
                f"safely ({tried} '{{' position(s) tried, {n} chars)")
        except ValueError:
            i += 1
            continue
        if isinstance(doc, dict) and "verdict" in doc:
            candidates.append(doc)
        i = end

    if candidates:
        return candidates[-1]

    raise ValidationError(
        "no JSON object with a 'verdict' key found in the model reply "
        f"({tried} '{{' position(s) tried, {n} chars)")


def _parse_event_stream(text):
    """Parse an `opencode run --format json` reply into its event objects.

    Despite the flag's name, `--format json` does not emit one JSON
    document: it emits JSONL, one complete top-level event object per
    line, e.g.

        {"type":"step_start",...,"part":{...,"type":"step-start"}}
        {"type":"tool_use",...,"part":{"type":"tool",...,"state":{...}}}
        {"type":"text",...,"part":{"type":"text","text":"..."}}

    Returns the list of parsed dict events carrying a "type" key, or None
    when the reply is not an event stream at all (a plain-prose or bare
    JSON reply) so callers can pass such a reply through untouched. Lines
    that do not parse are skipped: a truncated or interleaved line must
    not cost us the rest of the stream.
    """
    events = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if isinstance(obj, dict) and "type" in obj:
            events.append(obj)
    return events or None


# opencode's message-part union, read out of the v1.18.30 binary's own
# schema strings: text, reasoning, tool, step-start, step-finish, file,
# patch, agent, snapshot. Only "text" is the model addressing us.
MODEL_TEXT_PART = "text"


def _model_text_parts(events):
    """Yield the text of the model's OWN message parts, in stream order.

    SECURITY — this filter is the whole point of the unwrapper, and it is
    an allowlist on purpose. A tool-result part carries
    part.state.output: the verbatim content of a file the model read. On
    a pull request that content is attacker-controlled (pr.diff is the
    PR's own diff), and it also echoes back the reviewer prompt, which
    embeds a literal {"verdict": "blocker|should-fix|nit|clean", ...}
    template. Recovering the findings document from anywhere in the
    stream would let a PR plant {"verdict":"clean"} in a file, have the
    model read it, and have that become the posted verdict.

    A denylist of the part types that are known to carry foreign content
    would leave file/patch/agent/snapshot parts — and anything a future
    opencode adds — admitted by default. So only a part explicitly typed
    "text" contributes, and the top-level event type must not mention a
    tool either. An opencode that renames the text part fails closed with
    an empty unwrap, and extract_findings_from_reply's inventory names
    the types it actually saw, which is a one-run diagnosis rather than a
    silent wrong verdict.
    """
    for event in events:
        top_type = event.get("type")
        if isinstance(top_type, str) and "tool" in top_type:
            continue
        part = event.get("part")
        if not isinstance(part, dict):
            continue
        if part.get("type") != MODEL_TEXT_PART:
            continue
        chunk = part.get("text")
        if isinstance(chunk, str):
            yield chunk


def _event_inventory(events):
    """'step_start/step-start x2, text/text x3' — what the stream held."""
    counts = {}
    for event in events:
        part = event.get("part")
        part_type = part.get("type") if isinstance(part, dict) else None
        key = (str(event.get("type")),
               str(part_type) if part_type is not None else "-")
        counts[key] = counts.get(key, 0) + 1
    return ", ".join(f"{top}/{part} x{n}"
                     for (top, part), n in sorted(counts.items()))


def unwrap_event_stream(text):
    """Reduce an opencode JSON event stream to the model's own prose.

    Returns `text` unchanged when it is not an event stream, which keeps
    a plain-text or bare-JSON reply working exactly as before. Otherwise
    returns the newline-joined text of the model's own message parts,
    with every tool result excluded (see _model_text_parts). The join is
    a newline, not an empty string, so a truncated streamed draft cannot
    run into the complete document that follows it.
    """
    events = _parse_event_stream(text)
    if events is None:
        return text
    return "\n".join(_model_text_parts(events))


def extract_findings_from_reply(text):
    """extract_findings, with the event-stream unwrap in front of it.

    This is the entry point post_review.py uses on a raw model reply.
    extract_findings itself is left exactly as it was — it is still the
    right thing to run on recovered prose, and it keeps its
    last-candidate-wins rule, which is what picks the final answer out of
    cumulative or restated text parts.
    """
    events = _parse_event_stream(text)
    if events is None:
        return extract_findings(text)

    unwrapped = "\n".join(_model_text_parts(events))
    try:
        return extract_findings(unwrapped)
    except ValidationError as exc:
        # Each CI iteration is expensive: say what the stream actually
        # contained, not just that nothing was found in it.
        raise ValidationError(
            f"{exc}; the reply was an opencode JSON event stream "
            f"({len(events)} event(s); {len(unwrapped)} chars of model text "
            f"remained after tool results were excluded) — "
            f"observed events: {_event_inventory(events)}") from exc
