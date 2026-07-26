#!/usr/bin/env python3
"""Render a pull request body while omitting unused optional sections."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

# Internal references that usually should not appear in a PR body. This is a soft
# warning (not a hard failure) because naming a document is legitimate when the
# PR actually adds or edits it. See the "Self-Contained History" section of
# SKILL.md.
INTERNAL_REF_PATTERNS = (
    ("task/phase id", re.compile(r"\b[ABCDT][0-9]{1,2}\b")),
    ("decision/requirement id", re.compile(r"\b(?:ADR|AD|RFC|TDD|FR|NFR|AC|Gap|CORE|G)-[A-Za-z0-9]")),
    ("cycle label", re.compile(r"\bcycle\s+[a-z0-9]{1,2}\b", re.IGNORECASE)),
    ("phase label", re.compile(r"\bphase\s+\d+", re.IGNORECASE)),
    ("internal spec path", re.compile(r"\.specs/")),
)

# AI/tooling attribution is never legitimate in a PR body, so it is a hard
# failure rather than a warning. See "Commit And PR Hygiene" in SKILL.md.
AI_ATTRIBUTION_PATTERNS = (
    ("co-authored-by trailer", re.compile(r"co-authored-by\s*:", re.IGNORECASE)),
    ("generated-with line", re.compile(r"generated\s+with", re.IGNORECASE)),
    ("assistant/model name", re.compile(r"\b(?:claude|anthropic|copilot|chatgpt|openai)\b", re.IGNORECASE)),
    ("robot emoji", re.compile(r"\U0001F916")),
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--summary-file", required=True, type=Path)
    parser.add_argument("--changes-file", required=True, type=Path)
    parser.add_argument("--verification-file", required=True, type=Path)
    parser.add_argument("--screenshots-file", type=Path)
    parser.add_argument("--related-issues-file", type=Path)
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def read_required(path: Path, label: str) -> str:
    content = path.read_text(encoding="utf-8").strip()

    if not content:
        raise ValueError(f"{label} file must not be empty: {path}")

    return content


def read_optional(path: Path | None) -> str | None:
    if path is None:
        return None

    content = path.read_text(encoding="utf-8").strip()
    return content or None


def render_section(title: str, content: str) -> str:
    return f"## {title}\n\n{content}"


def warn_internal_refs(body: str) -> None:
    hits = [
        (name, match.group(0))
        for name, pattern in INTERNAL_REF_PATTERNS
        for match in pattern.finditer(body)
    ]
    if not hits:
        return

    print("WARN: PR body may contain internal references (keep it self-contained):", file=sys.stderr)
    for name, token in sorted(set(hits)):
        print(f"  - {name}: {token!r}", file=sys.stderr)
    print(
        "  Rephrase in plain terms; name a doc only if this PR adds or edits it.",
        file=sys.stderr,
    )


def check_ai_attribution(body: str) -> bool:
    hits = [
        (name, match.group(0))
        for name, pattern in AI_ATTRIBUTION_PATTERNS
        for match in pattern.finditer(body)
    ]
    if not hits:
        return True

    print("FAIL: PR body contains AI/tooling attribution:", file=sys.stderr)
    for name, token in sorted(set(hits)):
        print(f"  - {name}: {token!r}", file=sys.stderr)
    print(
        "  Remove all attribution; PR bodies carry no assistant or tool identification.",
        file=sys.stderr,
    )
    return False


def main() -> int:
    args = parse_args()
    sections = [
        render_section("Summary", read_required(args.summary_file, "Summary")),
        render_section("Changes", read_required(args.changes_file, "Changes")),
        render_section("Verification", read_required(args.verification_file, "Verification")),
    ]

    screenshots = read_optional(args.screenshots_file)
    if screenshots is not None:
        sections.append(render_section("Screenshots", screenshots))

    related_issues = read_optional(args.related_issues_file)
    if related_issues is not None:
        sections.append(render_section("Related Issues", related_issues))

    body = "\n\n".join(sections) + "\n"
    warn_internal_refs(body)

    if not check_ai_attribution(body):
        return 1

    if args.output is None:
        print(body, end="")
    else:
        args.output.write_text(body, encoding="utf-8")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
