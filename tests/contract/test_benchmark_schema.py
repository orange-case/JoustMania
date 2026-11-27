"""Benchmark telemetry schema (#2570, FR-254): telemetry.md appendix B against the code.

The real-repo test is the CI drift check; the tmp-repo tests pin both directions
(listed but not emitted, emitted but not listed) and the version check.
"""

from __future__ import annotations

import json
import sys

import infra_config as ic
import pytest
import telemetry_inventory as ti

sys.path.insert(0, str(ti.REPO_ROOT / "scripts"))
import benchmark_schema as bs

APPENDIX = """# Telemetry

## Appendix B. Benchmark schema

**Schema version:** `1.2.0`

**Owned namespaces:** `challenge.*`, `orangecase.*`

### B.1 Trace shape

One trace per game.

### B.2 Attributes

| Key | Kind | On | Type | Challenges | Status | Source |
|---|---|---|---|---|---|---|
| `orangecase.schema_version` | resource | every signal | string | all | stable | `collector.yaml` |
| `challenge.ground_truth` | span_event | game root | | UC-100, UC-101 | stable | `lib/gt.py` |
| `game.player_death` | span | player | | UC-100 | stable | - |
| `orangecase.synthetic` | resource | stowaway | bool | all | planned #2576 | `collector.yaml` |

## Next section
"""

COLLECTOR = """processors:
  resource:
    attributes:
      - key: orangecase.schema_version
        value: "1.2.0"
        action: upsert
      - key: orangecase.synthetic
"""


@pytest.fixture
def repo(tmp_path, monkeypatch):
    monkeypatch.setattr(bs, "COLLECTOR_REL", "collector.yaml")
    (tmp_path / "docs/specs").mkdir(parents=True)
    (tmp_path / "docs/specs/telemetry.md").write_text(APPENDIX)
    (tmp_path / "collector.yaml").write_text(COLLECTOR)
    (tmp_path / "lib/tests").mkdir(parents=True)
    (tmp_path / "lib/gt.py").write_text('EVENT = "challenge.ground_truth"\n')
    (tmp_path / "services/flagd").mkdir(parents=True)
    (tmp_path / "services/flagd/booth.json").write_text(
        json.dumps({"flags": {"challenge.enabled": {}}})
    )
    return tmp_path


@pytest.mark.uc("UC-100")
def test_code_and_published_schema_agree():
    """The CI drift check on the real repo: no attribute listed but not emitted, or the reverse."""
    assert bs.check(ti.REPO_ROOT) == []


@pytest.mark.uc("UC-100")
def test_schema_lists_what_the_first_challenges_read():
    keys = {r.key for r in bs.load(ti.REPO_ROOT).rows}
    for key in (
        "orangecase.schema_version",
        "telemetry.moment_shape",
        "moment.id",
        "player.serial",
        "challenge.warning_times_ns",
        "challenge.death_times_ns",
        "intervention.id",
        "decision.intervention_id",
        "challenge.first_death_at_ns",
        "game_player_accel_magnitude",
    ):
        assert key in keys


@pytest.mark.uc("UC-100")
def test_parse_reads_version_namespaces_shape_and_rows(repo):
    schema = bs.load(repo)
    assert schema.version == "1.2.0"
    assert schema.namespaces == ("challenge.*", "orangecase.*")
    assert schema.shape == "One trace per game."
    assert [r.key for r in schema.rows] == [
        "orangecase.schema_version",
        "challenge.ground_truth",
        "game.player_death",
        "orangecase.synthetic",
    ]
    assert bs.check(repo) == []


@pytest.mark.uc("UC-100")
def test_listed_but_not_emitted_fails(repo):
    (repo / "lib/gt.py").write_text('EVENT = "challenge.other"\n')
    errors = bs.check(repo)
    assert any("`challenge.ground_truth`: listed but not emitted" in e for e in errors)


@pytest.mark.uc("UC-100")
def test_emitted_but_not_listed_fails_and_names_the_line(repo):
    (repo / "lib/new.py").write_text('\nATTR = "orangecase.challenge"\n')
    assert bs.check(repo) == [
        (
            "lib/new.py:2: `orangecase.challenge` is in an owned namespace but not in "
            "docs/specs/telemetry.md appendix B: add its row"
        )
    ]


@pytest.mark.uc("UC-100")
def test_tests_flag_keys_and_other_namespaces_are_not_scanned(repo):
    (repo / "lib/tests/test_x.py").write_text('X = "challenge.only_in_a_test"\n')
    (repo / "lib/flags.py").write_text('F = "challenge.enabled"\nG = "game.unowned"\n')
    assert bs.check(repo) == []


@pytest.mark.uc("UC-100")
def test_collector_version_must_match(repo):
    (repo / "collector.yaml").write_text(COLLECTOR.replace('"1.2.0"', '"1.1.0"'))
    assert bs.check(repo) == [
        (
            "collector.yaml: orangecase.schema_version is 1.1.0, "
            "docs/specs/telemetry.md appendix B says 1.2.0"
        )
    ]


@pytest.mark.uc("UC-100")
def test_adding_a_row_is_one_line(repo):
    """A new attribute is one new row; nothing else in the source changes (no version bump)."""
    (repo / "lib/new.py").write_text('ATTR = "challenge.start"\n')
    assert bs.check(repo)
    row = "| `challenge.start` | span_event | game root | | UC-103 | stable | `lib/new.py` |\n"
    text = APPENDIX.replace("\n## Next section", row + "\n## Next section")
    (repo / "docs/specs/telemetry.md").write_text(text)
    assert bs.check(repo) == []
    before, after = bs.parse(APPENDIX), bs.parse(text)
    assert after.version == before.version
    assert after.digest() != before.digest()


GT_ROW = "| `challenge.ground_truth` | span_event | game root | | UC-100, UC-101 | stable | `lib/gt.py` |"


@pytest.mark.uc("UC-100")
@pytest.mark.parametrize(
    "old, new, message",
    [
        ("| stable |", "| shipped |", "status"),
        ("| span_event |", "| span-event |", "kind"),
        (" `lib/gt.py` |", "", "cells"),
    ],
)
def test_malformed_rows_fail(old, new, message):
    with pytest.raises(bs.SchemaError, match=message):
        bs.parse(APPENDIX.replace(GT_ROW, GT_ROW.replace(old, new)))


@pytest.mark.uc("UC-100")
def test_duplicate_row_fails():
    row = "| `game.player_death` | span | player | | UC-100 | stable | - |"
    with pytest.raises(bs.SchemaError, match="twice"):
        bs.parse(APPENDIX.replace(row, row + "\n" + row))


@pytest.mark.uc("UC-100")
def test_json_copy_is_versioned_and_drops_source(repo):
    doc = json.loads(bs.render_json(bs.load(repo)))
    assert doc["version"] == "1.2.0"
    assert len(doc["digest"]) == 64
    gt = doc["entries"][1]
    assert gt == {
        "key": "challenge.ground_truth",
        "kind": "span_event",
        "on": "game root",
        "type": None,
        "challenges": ["UC-100", "UC-101"],
        "status": "stable",
    }
    assert (
        doc["entries"][3]["status"] == "planned" and doc["entries"][3]["issue"] == 2576
    )


@pytest.mark.uc("UC-035", flow="A7")
def test_every_forwarding_pipeline_sets_the_schema_version():
    """The `resource` processor carries the version; every OTLP pipeline (in-case and gateway) runs it."""
    base = ic.load_yaml(ic.REPO_ROOT / "services/otel-collector/config.yaml")
    attrs = {a["key"]: a for a in base["processors"]["resource"]["attributes"]}
    assert attrs["orangecase.schema_version"]["action"] == "upsert"
    assert attrs["orangecase.schema_version"]["value"] == bs.load(ti.REPO_ROOT).version
    gateway = ic.load_yaml(ic.REPO_ROOT / "services/otel-collector/config.gateway.yaml")
    pipelines = {**base["service"]["pipelines"], **gateway["service"]["pipelines"]}
    for name in (
        "traces",
        "metrics",
        "logs",
        "traces/gateway",
        "metrics/gateway",
        "logs/gateway",
    ):
        assert "resource" in pipelines[name]["processors"], name
