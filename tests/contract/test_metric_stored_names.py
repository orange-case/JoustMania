"""A metric's OTLP unit must never rename the series VictoriaMetrics stores (#2630).

The collector's prometheus_remote_write exporter builds the stored name from the
OTLP name, unit and type (prometheus/otlptranslator ``normalizeName``): it appends
the unit's suffix, ``_total`` to a counter and ``_ratio`` to a gauge with unit
"1". Dashboards, alerts and the agent query the stored names, so every Python
instrument has to store under its own name, and the catalog in
docs/specs/generated/telemetry-metric-catalog.md pins those names.
"""

from __future__ import annotations

import ast
import re
import infra_config as ic
import pytest
import telemetry_inventory as ti

CATALOG = ic.REPO_ROOT / "docs" / "specs" / "generated" / "telemetry-metric-catalog.md"

# otlptranslator v1.0.0 metric_namer.go (unitMap, perUnitMap); an unknown unit is used as is.
_UNIT_MAP = {
    "d": "days",
    "h": "hours",
    "min": "minutes",
    "s": "seconds",
    "ms": "milliseconds",
    "us": "microseconds",
    "ns": "nanoseconds",
    "By": "bytes",
    "KiBy": "kibibytes",
    "MiBy": "mebibytes",
    "GiBy": "gibibytes",
    "TiBy": "tibibytes",
    "KBy": "kilobytes",
    "MBy": "megabytes",
    "GBy": "gigabytes",
    "TBy": "terabytes",
    "m": "meters",
    "V": "volts",
    "A": "amperes",
    "J": "joules",
    "W": "watts",
    "g": "grams",
    "Cel": "celsius",
    "Hz": "hertz",
    "1": "",
    "%": "percent",
}
_PER_UNIT_MAP = {
    "s": "second",
    "m": "minute",
    "h": "hour",
    "d": "day",
    "w": "week",
    "mo": "month",
    "y": "year",
}

COUNTER, GAUGE, HISTOGRAM = "counter", "gauge", "histogram"
# Prometheus type per instrument constructor: an up-down counter is not monotonic, so it is a gauge.
_KINDS = {
    "Counter": COUNTER,
    "Gauge": GAUGE,
    "Histogram": HISTOGRAM,
    "create_counter": COUNTER,
    "create_observable_counter": COUNTER,
    "create_up_down_counter": GAUGE,
    "create_observable_up_down_counter": GAUGE,
    "create_gauge": GAUGE,
    "create_observable_gauge": GAUGE,
    "create_histogram": HISTOGRAM,
}
_NAMING_KEYS = {"namespace", "add_metric_suffixes", "translation_strategy"}


def _clean_unit(unit: str) -> str:
    return re.sub(r"_+", "_", re.sub(r"[^a-zA-Z0-9_:]", "_", unit)).removeprefix("_")


def _unit_suffixes(unit: str) -> tuple[str, str]:
    main, _, per = (t.strip() for t in unit.partition("/"))
    main_suffix = (
        _UNIT_MAP.get(main, main) if main and not set(main) & set("{}") else ""
    )
    per_suffix = _PER_UNIT_MAP.get(per, per) if per and not set(per) & set("{}") else ""
    return _clean_unit(main_suffix), _clean_unit(
        f"per_{per_suffix}" if per_suffix else ""
    )


def stored_name(name: str, unit: str, kind: str) -> str:
    """Port of otlptranslator v1.0.0 ``normalizeName`` with suffixes, no namespace."""
    tokens = [t for t in re.split(r"[^a-zA-Z0-9:]", name) if t]
    main, per = _unit_suffixes(unit)
    if main in tokens:
        main = ""
    per = "" if per == "per_" else per.removesuffix("_")
    if per in tokens:
        per = ""
    if per:
        main = main.removesuffix("_")
    tokens += [t for t in (main, per) if t]
    if kind == COUNTER:
        tokens = [t for t in tokens if t != "total"] + ["total"]
    if unit == "1" and kind == GAUGE:
        tokens = [t for t in tokens if t != "ratio"] + ["ratio"]
    joined = "_".join(tokens)
    return f"_{joined}" if joined[:1].isdigit() else joined


class TestTranslatorModel:
    @pytest.mark.parametrize(
        ("name", "unit", "kind", "expected"),
        [
            ("op_duration_seconds", "s", HISTOGRAM, "op_duration_seconds"),
            ("op_duration", "s", HISTOGRAM, "op_duration_seconds"),
            ("process_cpu_seconds_total", "s", COUNTER, "process_cpu_seconds_total"),
            ("events", "", COUNTER, "events_total"),
            ("events_total_x", "", COUNTER, "events_x_total"),
            (
                "process_resident_memory_bytes",
                "By",
                GAUGE,
                "process_resident_memory_bytes",
            ),
            ("accel", "{g}", GAUGE, "accel"),
            ("accel", "g", GAUGE, "accel_grams"),
            ("share", "1", GAUGE, "share_ratio"),
            ("hits", "1", COUNTER, "hits_total"),
            ("jitter_ms", "ms", GAUGE, "jitter_ms_milliseconds"),
            ("rate", "By/s", GAUGE, "rate_bytes_per_second"),
            (
                "gen_ai.client.operation.duration",
                "s",
                HISTOGRAM,
                "gen_ai_client_operation_duration_seconds",
            ),
        ],
    )
    def test_matches_otlptranslator(self, name, unit, kind, expected):
        assert stored_name(name, unit, kind) == expected


def _str_constants(tree: ast.Module) -> dict[str, str]:
    return {
        n.targets[0].id: n.value.value
        for n in tree.body
        if isinstance(n, ast.Assign)
        and isinstance(n.targets[0], ast.Name)
        and isinstance(n.value, ast.Constant)
        and isinstance(n.value.value, str)
    }


def _literal(node: ast.expr | None, constants: dict[str, str]) -> str | None:
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.Name):
        return constants.get(node.id)
    return None


def _instruments() -> list[tuple[str, str, str, str, str]]:
    """(site, constructor, name, unit, kind) for every wrapper or ``create_*`` call in lib/ and services/."""
    found = []
    for path in ti.python_sources():
        if path.name == "otel_metrics.py":
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        constants = _str_constants(tree)
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            ctor = node.func.id if isinstance(node.func, ast.Name) else None
            if isinstance(node.func, ast.Attribute) and node.func.attr.startswith(
                "create_"
            ):
                ctor = node.func.attr
            if ctor not in _KINDS or not node.args:
                continue
            site = f"{path.relative_to(ic.REPO_ROOT).as_posix()}:{node.lineno}"
            name = _literal(node.args[0], constants)
            unit_kw = next((kw.value for kw in node.keywords if kw.arg == "unit"), None)
            unit = "" if unit_kw is None else _literal(unit_kw, constants)
            assert name is not None and unit is not None, (
                f"{site}: name and unit must be literals or module constants"
            )
            found.append((site, ctor, name, unit, _KINDS[ctor]))
    return found


_INSTRUMENTS = _instruments()


def _catalog_names() -> set[str]:
    return set(
        re.findall(
            r"^\| `([a-z0-9_]+)` \|", CATALOG.read_text(encoding="utf-8"), re.MULTILINE
        )
    )


@pytest.mark.uc("UC-035", flow="main", steps=[6])
class TestStoredNamesArePinned:
    def test_discovery_finds_wrappers_and_direct_instruments(self):
        found = {(name, unit) for _, _, name, unit, _ in _INSTRUMENTS}
        assert {
            ("grpc_client_request_duration_seconds", "s"),
            ("process_resident_memory_bytes", "By"),
            ("stowaway_swing_accel_magnitude", "{g}"),
            ("feature_flag.evaluation_requests_total", ""),
        } <= found

    @pytest.mark.parametrize(
        ("site", "ctor", "name", "unit", "kind"),
        _INSTRUMENTS,
        ids=[f"{i[0]}:{i[2]}" for i in _INSTRUMENTS],
    )
    def test_unit_keeps_the_name(self, site, ctor, name, unit, kind):
        assert stored_name(name, unit, kind) == ti.prometheus_name(name), (
            f"{site}: {ctor}({name!r}, unit={unit!r}) would be stored as {stored_name(name, unit, kind)!r}"
        )

    def test_stored_wrapper_names_are_the_catalog(self):
        wrappers = {
            i for i in _INSTRUMENTS if i[1] in {"Counter", "Gauge", "Histogram"}
        }
        stored = {stored_name(name, unit, kind) for _, _, name, unit, kind in wrappers}
        assert stored == _catalog_names()

    def test_exporter_keeps_default_naming(self):
        exporters = ic.load_yaml(ic.COLLECTOR_FILE)["exporters"]
        remote_write = {
            k: v
            for k, v in exporters.items()
            if k.split("/")[0] == "prometheus_remote_write"
        }
        assert remote_write, (
            "no prometheus_remote_write exporter in the collector config"
        )
        for key, cfg in remote_write.items():
            assert not _NAMING_KEYS & set(cfg or {}), (
                f"{key} overrides the naming the stored names rely on"
            )
