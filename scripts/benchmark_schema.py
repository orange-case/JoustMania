"""The benchmark telemetry schema (#2570, FR-254): docs/specs/telemetry.md appendix B.

    python3 scripts/benchmark_schema.py --check   # drift between the code and the schema

The appendix is the only source. `scripts/gen_specs.py` renders it into
docs/specs/generated/benchmark-schema.md and .json (`make specs`); this module
parses it and checks it against the code:

- the collector's `orangecase.schema_version` equals the schema version;
- every row with a source names a file that contains its key as a literal
  (listed but not emitted);
- every string literal in an owned namespace in the Python, Go and Rust code
  outside tests is a listed key (emitted but not listed). Flag keys are skipped.

Standard library only.
"""

from __future__ import annotations

import argparse
import hashlib
import itertools
import json
import re
import sys
from dataclasses import dataclass
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
TELEMETRY_REL = "docs/specs/telemetry.md"
COLLECTOR_REL = "services/otel-collector/config.yaml"
SECTION = "## Appendix B. Benchmark schema"
SHAPE_HEADING = "### B.1 Trace shape"
COLUMNS = ["Key", "Kind", "On", "Type", "Challenges", "Status", "Source"]
KINDS = ("resource", "span", "span_event", "attribute", "metric", "label")
NO_SOURCE = "-"

_VERSION_RE = re.compile(r"^\*\*Schema version:\*\* `(\d+\.\d+\.\d+)`\s*$")
_NAMESPACES_RE = re.compile(r"^\*\*Owned namespaces:\*\* (.+)$")
_STATUS_RE = re.compile(r"^(stable|planned #\d+)$")
_COLLECTOR_VERSION_RE = re.compile(
    r"key: orangecase\.schema_version\r?\n[ \t]*value: \"?([0-9.]+)"
)
_CODE_SUFFIXES = (".py", ".go", ".rs")
_SKIP_PARTS = frozenset({"tests", "test", "testdata", "gen", "node_modules", "target"})


class SchemaError(Exception):
    """Appendix B is missing or malformed."""


@dataclass(frozen=True)
class Row:
    key: str
    kind: str
    on: str
    type: str
    challenges: str
    status: str
    source: str


@dataclass(frozen=True)
class Schema:
    version: str
    namespaces: tuple[str, ...]
    shape: str
    rows: tuple[Row, ...]

    def entries(self) -> list[dict]:
        return [_entry(r) for r in self.rows]

    def digest(self) -> str:
        blob = json.dumps(self.entries(), sort_keys=True)
        return hashlib.sha256(blob.encode()).hexdigest()


def _entry(r: Row) -> dict:
    status, _, issue = r.status.partition(" #")
    entry = {
        "key": r.key,
        "kind": r.kind,
        "on": r.on.replace("`", ""),
        "type": r.type.replace("`", "") or None,
        "challenges": [c.strip() for c in r.challenges.split(",")],
        "status": status,
    }
    if issue:
        entry["issue"] = int(issue)
    return entry


def _unquote(cell: str) -> str:
    cell = cell.strip()
    return cell[1:-1] if len(cell) > 1 and cell[0] == cell[-1] == "`" else cell


def _cells(line: str) -> list[str]:
    line = line.strip()
    if not line.startswith("|"):
        return []
    return [c.strip() for c in line[1:].removesuffix("|").split("|")]


def _section(text: str) -> list[str]:
    lines = text.replace("\r\n", "\n").split("\n")
    try:
        start = lines.index(SECTION)
    except ValueError as err:
        raise SchemaError(f"{TELEMETRY_REL}: no '{SECTION}' heading") from err
    end = next(
        (i for i in range(start + 1, len(lines)) if lines[i].startswith("## ")),
        len(lines),
    )
    return lines[start + 1 : end]


def _meta(lines: list[str]) -> tuple[str, tuple[str, ...]]:
    version = next((m[1] for l in lines if (m := _VERSION_RE.match(l))), None)
    if not version:
        raise SchemaError(
            f"{TELEMETRY_REL} appendix B: no '**Schema version:** `X.Y.Z`' line"
        )
    ns_line = next((m[1] for l in lines if (m := _NAMESPACES_RE.match(l))), None)
    if not ns_line:
        raise SchemaError(
            f"{TELEMETRY_REL} appendix B: no '**Owned namespaces:**' line"
        )
    namespaces = tuple(re.findall(r"`([^`]+\.\*)`", ns_line))
    if not namespaces:
        raise SchemaError(
            f"{TELEMETRY_REL} appendix B: owned namespaces must be written `ns.*`"
        )
    return version, namespaces


def _shape(lines: list[str]) -> str:
    try:
        s = lines.index(SHAPE_HEADING)
    except ValueError as err:
        raise SchemaError(f"{TELEMETRY_REL} appendix B: no '{SHAPE_HEADING}'") from err
    end = next(i for i in range(s + 1, len(lines)) if lines[i].startswith("### "))
    return "\n".join(lines[s + 1 : end]).strip()


def _row(line: str) -> Row:
    cells = _cells(line)
    if len(cells) != len(COLUMNS):
        raise SchemaError(
            f"appendix B row has {len(cells)} cells, not {len(COLUMNS)}: {line}"
        )
    row = Row(
        key=_unquote(cells[0]),
        kind=cells[1],
        on=cells[2],
        type=cells[3],
        challenges=cells[4],
        status=cells[5],
        source=_unquote(cells[6]),
    )
    if row.kind not in KINDS:
        raise SchemaError(
            f"appendix B `{row.key}`: kind '{row.kind}' is not one of {KINDS}"
        )
    if not _STATUS_RE.match(row.status):
        raise SchemaError(
            f"appendix B `{row.key}`: status '{row.status}' is not 'stable' or 'planned #N'"
        )
    return row


def _rows(lines: list[str]) -> tuple[Row, ...]:
    header = next((i for i, l in enumerate(lines) if _cells(l) == COLUMNS), None)
    if header is None:
        raise SchemaError(
            f"{TELEMETRY_REL} appendix B: no table '| {' | '.join(COLUMNS)} |'"
        )
    rows: list[Row] = []
    for line in itertools.takewhile(lambda l: l.startswith("|"), lines[header + 2 :]):
        row = _row(line)
        if any((r.kind, r.key) == (row.kind, row.key) for r in rows):
            raise SchemaError(f"appendix B lists {row.kind} `{row.key}` twice")
        rows.append(row)
    if not rows:
        raise SchemaError(f"{TELEMETRY_REL} appendix B: the table has no rows")
    return tuple(rows)


def parse(text: str) -> Schema:
    lines = _section(text)
    version, namespaces = _meta(lines)
    return Schema(version, namespaces, _shape(lines), _rows(lines))


def load(root: Path = REPO_ROOT) -> Schema:
    return parse((root / TELEMETRY_REL).read_text(encoding="utf-8"))


# --------------------------------------------------------------------------- #
_SECTIONS = (
    ("Resource attributes", ("resource",)),
    ("Spans and span events", ("span", "span_event")),
    ("Attributes", ("attribute",)),
    ("Metrics and labels", ("metric", "label")),
)


def render_markdown(schema: Schema) -> str:
    out = [
        "# Benchmark telemetry schema",
        "",
        (
            f"Schema version **{schema.version}**, sent as the resource attribute "
            "`orangecase.schema_version` on every OTLP signal the case forwards. "
            "Machine-readable copy: [benchmark-schema.json](benchmark-schema.json). "
            "Source, version rules and the drift check: "
            "[telemetry.md appendix B](../telemetry.md#appendix-b-benchmark-schema)."
        ),
        "",
        "## Trace shape",
        "",
        schema.shape,
        "",
    ]
    for title, kinds in _SECTIONS:
        out += [
            f"## {title}",
            "",
            "| Key | Kind | On | Type | Challenges | Status |",
            "|---|---|---|---|---|---|",
        ]
        for r in schema.rows:
            if r.kind in kinds:
                out.append(
                    f"| `{r.key}` | {r.kind} | {r.on} | {r.type} | {r.challenges} | {r.status} |"
                )
        out.append("")
    return "\n".join(out)


def render_json(schema: Schema) -> str:
    doc = {
        "schema": "orangecase-telemetry",
        "version": schema.version,
        "digest": schema.digest(),
        "source": f"{TELEMETRY_REL}#appendix-b-benchmark-schema",
        "generated_by": "scripts/gen_specs.py (make specs); do not edit",
        "owned_namespaces": list(schema.namespaces),
        "entries": schema.entries(),
    }
    return json.dumps(doc, indent=2) + "\n"


# --------------------------------------------------------------------------- #
def _code_files(root: Path):
    for top in ("lib", "services"):
        for path in sorted((root / top).rglob("*")):
            if path.suffix not in _CODE_SUFFIXES or not path.is_file():
                continue
            rel = path.relative_to(root)
            name = path.name
            if (
                _SKIP_PARTS & set(rel.parts[:-1])
                or name.startswith("test_")
                or name.endswith(
                    ("_test.go", "_test.py", "_pb2.py", "_pb2_grpc.py", ".pb.go")
                )
            ):
                continue
            yield rel, path


def _flag_keys(root: Path) -> set[str]:
    keys: set[str] = set()
    for path in (root / "services" / "flagd").rglob("*.json"):
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (ValueError, OSError):
            continue
        if isinstance(data, dict) and isinstance(data.get("flags"), dict):
            keys.update(data["flags"])
    return keys


def _literal(key: str) -> re.Pattern[str]:
    return re.compile(r"[\"'`]" + re.escape(key) + r"[\"'`]")


def _token(key: str) -> re.Pattern[str]:
    return re.compile(r"(?<![\w.])" + re.escape(key) + r"(?![\w.])")


def _version_errors(root: Path, schema: Schema) -> list[str]:
    collector = (root / COLLECTOR_REL).read_text(encoding="utf-8")
    m = _COLLECTOR_VERSION_RE.search(collector)
    if not m:
        return [
            f"{COLLECTOR_REL}: the resource processor sets no orangecase.schema_version"
        ]
    if m[1] != schema.version:
        return [
            (
                f"{COLLECTOR_REL}: orangecase.schema_version is {m[1]}, "
                f"{TELEMETRY_REL} appendix B says {schema.version}"
            )
        ]
    return []


def _listed_errors(root: Path, schema: Schema) -> list[str]:
    """Rows whose source no longer contains the key (listed but not emitted)."""
    errors = []
    for r in schema.rows:
        if r.source == NO_SOURCE:
            continue
        path = root / r.source
        if not path.is_file():
            errors.append(f"appendix B `{r.key}`: source {r.source} does not exist")
            continue
        pattern = _literal(r.key) if path.suffix in _CODE_SUFFIXES else _token(r.key)
        if not pattern.search(path.read_text(encoding="utf-8", errors="ignore")):
            errors.append(
                f"appendix B `{r.key}`: listed but not emitted: {r.source} has no literal {r.key!r}"
            )
    return errors


def _emitted_errors(root: Path, schema: Schema) -> list[str]:
    """Owned-namespace literals in the code that no row lists (emitted but not listed)."""
    known = {r.key for r in schema.rows} | _flag_keys(root)
    prefixes = "|".join(re.escape(n.removesuffix("*")) for n in schema.namespaces)
    owned = re.compile(r"[\"']((?:" + prefixes + r")[A-Za-z0-9_.]+)[\"']")
    errors = []
    for rel, path in _code_files(root):
        text = path.read_text(encoding="utf-8", errors="ignore")
        for n, line in enumerate(text.splitlines(), 1):
            errors += [
                f"{rel.as_posix()}:{n}: `{key}` is in an owned namespace but not in "
                f"{TELEMETRY_REL} appendix B: add its row"
                for key in owned.findall(line)
                if key not in known and not key.endswith(".")
            ]
    return errors


def check(root: Path = REPO_ROOT, schema: Schema | None = None) -> list[str]:
    """Every drift between the code and the schema, one message each."""
    schema = schema or load(root)
    return (
        _version_errors(root, schema)
        + _listed_errors(root, schema)
        + _emitted_errors(root, schema)
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--check", action="store_true", help="report drift (default)")
    parser.parse_args(argv)
    try:
        errors = check()
    except SchemaError as err:
        print(f"benchmark_schema: {err}", file=sys.stderr)
        return 1
    for e in errors:
        print(e, file=sys.stderr)
    if errors:
        print(
            f"::error title=Benchmark schema drift::{len(errors)} problem(s); see above",
            file=sys.stderr,
        )
        return 1
    print("benchmark schema: code and appendix B agree.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
