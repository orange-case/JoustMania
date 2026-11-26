"""scripts/gen_specs.py: the generated spec tables (#1280).

The drift check itself (`make specs-check`) runs as its own CI step; these tests
pin the generator's parsing and its failure modes.
"""

from __future__ import annotations

import json
import sys

import pytest
import telemetry_inventory as ti

sys.path.insert(0, str(ti.REPO_ROOT / "scripts"))
import gen_specs as g


@pytest.fixture
def tmp_repo(tmp_path, monkeypatch):
    """Point the generator at an empty repo layout under tmp_path."""
    monkeypatch.setattr(g, "REPO_ROOT", tmp_path)
    monkeypatch.setattr(g, "GENERATED", tmp_path / "docs/specs/generated")
    monkeypatch.setattr(g, "NOTES", tmp_path / "docs/specs/notes")
    monkeypatch.setattr(g, "FLAGD", tmp_path / "services/flagd")
    monkeypatch.setattr(g, "REGISTRY", tmp_path / "services/flag-service/registry.yaml")
    for d in (
        "proto",
        "docs/specs/generated",
        "docs/specs/notes",
        "services/flagd/ci",
        "services/flag-service",
    ):
        (tmp_path / d).mkdir(parents=True)
    (tmp_path / "services/flag-service/registry.yaml").write_text("flags: []\n")
    return tmp_path


PROTO = """syntax = "proto3";
package demo.v1;

service Demo {
  // comment
  rpc Unary(AReq) returns (AResp);
  rpc Server(AReq) returns (stream AResp);
  rpc Client(stream AReq) returns (AResp);
  rpc Bidi(stream AReq) returns (stream AResp);
}

message AReq {}
"""


def test_proto_rpc_kinds(tmp_repo):
    (tmp_repo / "proto/demo.proto").write_text(PROTO)
    [(svc, rel, rpcs)] = g.proto_rpcs()
    assert (svc, rel) == ("demo.v1.Demo", "proto/demo.proto")
    assert rpcs == [
        ("Unary", "unary", "AReq", "AResp"),
        ("Server", "server stream", "AReq", "AResp"),
        ("Client", "client stream", "AReq", "AResp"),
        ("Bidi", "bidi stream", "AReq", "AResp"),
    ]


def test_proto_rpcs_on_the_real_protos_match_a_plain_count():
    """The regex finds every `rpc` line of proto/*.proto, no more."""
    found = sum(len(rpcs) for _, _, rpcs in g.proto_rpcs())
    plain = sum(
        1
        for p in (ti.REPO_ROOT / "proto").glob("*.proto")
        for line in p.read_text().splitlines()
        if line.strip().startswith("rpc ")
    )
    assert found == plain > 0


def test_rpc_without_description_fails(tmp_repo):
    (tmp_repo / "proto/demo.proto").write_text(PROTO)
    (tmp_repo / "docs/specs/notes/grpc-rpcs.yaml").write_text(
        "demo.v1.Demo/Unary: one\n"
    )
    with pytest.raises(g.SpecSourceError, match="no description for .*Demo/Server"):
        g.grpc_rpcs()


def test_stale_note_key_fails(tmp_repo):
    (tmp_repo / "proto/demo.proto").write_text(PROTO)
    notes = {
        f"demo.v1.Demo/{r}": "x" for r in ("Unary", "Server", "Client", "Bidi", "Gone")
    }
    (tmp_repo / "docs/specs/notes/grpc-rpcs.yaml").write_text(json.dumps(notes))
    with pytest.raises(g.SpecSourceError, match="no longer exist.*Demo/Gone"):
        g.grpc_rpcs()


def test_rpc_table_merges_descriptions_and_escapes_pipes(tmp_repo):
    (tmp_repo / "proto/demo.proto").write_text(PROTO)
    notes = {
        f"demo.v1.Demo/{r}": f"does {r}" for r in ("Unary", "Server", "Client", "Bidi")
    }
    notes["demo.v1.Demo/Unary"] = "a | b"
    (tmp_repo / "docs/specs/notes/grpc-rpcs.yaml").write_text(json.dumps(notes))
    text = g.grpc_rpcs()
    assert "## `demo.v1.Demo` (`proto/demo.proto`)" in text
    assert "| `Unary` | unary | `AReq` → `AResp` | a \\| b |" in text


def _metric(module: str, recorded: bool) -> ti.PyMetric:
    return ti.PyMetric("m", "Counter", "m", module, (), ["x.py:1"] if recorded else [])


@pytest.mark.parametrize(
    ("defs", "cell"),
    [
        ([("services.menu.metrics", True)], "yes"),
        ([("services.menu.metrics", False)], "**no**: defined, never recorded"),
        (
            [("services.menu.metrics", True), ("services.audio.metrics", True)],
            "yes (each service)",
        ),
        (
            [("services.menu.metrics", True), ("services.audio.metrics", False)],
            "emitted by menu; defined but never recorded in audio",
        ),
    ],
)
def test_emitted_cell(defs, cell):
    assert g._emitted([_metric(m, r) for m, r in defs]) == cell


def test_definer_names():
    assert g._definer("services.game_coordinator.metrics") == "game-coordinator"
    assert (
        g._definer("services.pairing_daemon.psmove_pairing.metrics") == "pairing-daemon"
    )
    assert g._definer("lib.grpc_metrics") == "lib (`lib/grpc_metrics.py`)"


def test_metric_catalog_has_one_row_per_python_metric():
    text = g.telemetry_metric_catalog()
    rows = [line for line in text.splitlines() if line.startswith("| `")]
    assert len(rows) == len(ti.python_metrics())
    assert "unique Python metric names" not in text  # totals moved to --counts (#2152)
    assert (
        f"**{len(ti.python_metrics())} unique Python metric names**"
        in g.render_counts()
    )


def _flags(**flags):
    return json.dumps({"flags": flags})


def _flag(default, **variants):
    return {"state": "ENABLED", "defaultVariant": default, "variants": variants}


def test_flag_counts_and_ci_diff(tmp_repo):
    flagd = tmp_repo / "services/flagd"
    (flagd / "game.json").write_text(
        _flags(a=_flag("on", on=True, off=False), b=_flag("x", x=1))
    )
    (flagd / "agent.json").write_text(_flags(c=_flag("x", x="s")))
    (flagd / "ci/game.json").write_text(
        _flags(a=_flag("off", on=True, off=False), b=_flag("x", x=1, ci=2))
    )
    (flagd / "ci/agent.json").write_text(_flags(c=_flag("x", x="s"), d=_flag("x", x=1)))

    sets = g.flag_counts()
    assert sets.index("| `agent` |") < sets.index("| `game` |")
    assert g.flag_set_counts() == [
        "- **3 flags in 2 flag sets** (`ci/` copies not counted).",
        "  - `agent`: 1",
        "  - `game`: 2",
    ]

    diff = g.flags_ci_diff()
    assert "| game.json | `a` | `on` = true | `off` = false |" in diff
    assert "| game.json | `b` | `ci` = 2 |" in diff
    assert "| agent.json | `d` | `ci/` only |" in diff


def test_check_mode_reports_stale_and_orphan_files(tmp_repo, monkeypatch, capsys):
    monkeypatch.setattr(g, "GENERATORS", {"one.md": lambda: "fresh\n"})
    generated = tmp_repo / "docs/specs/generated"
    (generated / "one.md").write_text("old\n")
    (generated / "orphan.md").write_text("x\n")

    assert g.main(["--check"]) == 1
    err = capsys.readouterr().err
    assert "Run `make specs`" in err
    assert "one.md" in err and "orphan.md" in err

    assert g.main([]) == 0
    assert (generated / "one.md").read_text() == "fresh\n"
    assert not (generated / "orphan.md").exists()
    assert g.main(["--check"]) == 0


def test_committed_generated_files_have_a_generator():
    """Every file under docs/specs/generated/ is produced by gen_specs (no hand-written strays)."""
    on_disk = {p.name for p in (ti.REPO_ROOT / "docs/specs/generated").glob("*.md")}
    assert on_disk == set(g.GENERATORS)


def test_label_values_render_in_labels_column():
    text = g.telemetry_metric_catalog()
    assert "`outcome` (winner\\|tie\\|completed\\|force_ended\\|error)" in text


def test_label_values_for_unknown_label_fail(tmp_repo, monkeypatch):
    monkeypatch.setattr(
        g, "_load_label_values", lambda name: {"games_completed_total": {"nope": "a"}}
    )
    with pytest.raises(g.SpecSourceError, match="no label 'nope'"):
        g.telemetry_metric_catalog()


# --------------------------------------------------------------------------- #
# #2152: no count-style aggregate in a committed file. Two PRs that each add a
# flag (an RPC, a metric) to different places must not touch the same line, or
# the merge queue ejects the later one on a conflict.
# --------------------------------------------------------------------------- #


def test_adding_a_flag_to_an_existing_set_leaves_flag_counts_md_unchanged(tmp_repo):
    flagd = tmp_repo / "services/flagd"
    (flagd / "game.json").write_text(_flags(a=_flag("x", x=1)))
    (flagd / "agent.json").write_text(_flags(c=_flag("x", x=1)))
    before = g.flag_counts()

    (flagd / "game.json").write_text(_flags(a=_flag("x", x=1), b=_flag("x", x=2)))
    assert g.flag_counts() == before


def _changed_lines(before: str, after: str) -> list[str]:
    import difflib

    return [
        line
        for line in difflib.ndiff(before.splitlines(), after.splitlines())
        if line[:2] in ("- ", "+ ")
    ]


def test_adding_an_rpc_changes_only_its_own_row(tmp_repo):
    names = ("Unary", "Server", "Client", "Bidi")
    notes = {f"demo.v1.Demo/{r}": f"does {r}" for r in names}
    (tmp_repo / "proto/demo.proto").write_text(PROTO)
    (tmp_repo / "docs/specs/notes/grpc-rpcs.yaml").write_text(json.dumps(notes))
    before = g.grpc_rpcs()

    (tmp_repo / "proto/demo.proto").write_text(
        PROTO.replace(
            "  // comment\n", "  // comment\n  rpc Added(AReq) returns (AResp);\n"
        )
    )
    notes["demo.v1.Demo/Added"] = "new"
    (tmp_repo / "docs/specs/notes/grpc-rpcs.yaml").write_text(json.dumps(notes))
    assert _changed_lines(before, g.grpc_rpcs()) == [
        "+ | `Added` | unary | `AReq` → `AResp` | new |"
    ]


def test_counts_print_and_are_never_written(tmp_repo, monkeypatch, capsys):
    monkeypatch.setattr(g, "metric_counts", lambda: ["- metrics"])
    (tmp_repo / "proto/demo.proto").write_text(PROTO)
    (tmp_repo / "services/flagd/game.json").write_text(_flags(a=_flag("x", x=1)))

    assert g.main(["--counts"]) == 0
    out = capsys.readouterr().out
    assert "**1 flags in 1 flag sets**" in out
    assert "**4 RPCs in 1 services.**" in out
    assert "  - `demo.v1.Demo`: 4" in out
    assert not list((tmp_repo / "docs/specs/generated").iterdir())


def test_ci_diff_has_no_placeholder_rows(tmp_repo):
    """An empty section is an empty table, not a shared `(none)` line two PRs would both replace."""
    (tmp_repo / "services/flagd/game.json").write_text(_flags(a=_flag("x", x=1)))
    (tmp_repo / "services/flagd/ci/game.json").write_text(_flags(a=_flag("x", x=1)))
    assert "(none)" not in g.flags_ci_diff()


def test_check_and_counts_are_mutually_exclusive(capsys):
    with pytest.raises(SystemExit):
        g.main(["--check", "--counts"])
    assert "not allowed with" in capsys.readouterr().err


# --------------------------------------------------------------------------- #
# The per-set flag catalog (flags.md §3 to §10a, #2793)
# --------------------------------------------------------------------------- #
def _catalog_repo(tmp_repo, notes: dict, registry: list | None = None, unread=()):
    """Write the sidecar and registry, and reader sources naming every claimed key.

    A (set, key) in `unread` is left out of the sources, so its read_by claim fails.
    """
    (tmp_repo / "docs/specs/notes/flags.yaml").write_text(json.dumps(notes))
    sources: dict[str, list[str]] = {}
    for s, entries in notes.items():
        for key, entry in entries.items():
            for reader in entry.get("read_by") or []:
                if (s, key) not in unread and reader in g.READER_SOURCES:
                    sources.setdefault(reader, []).append(f'"{key}"')
    for reader, keys in sources.items():
        base, suffix = g.READER_SOURCES[reader]
        src = tmp_repo / base / f"reads{suffix}"
        src.parent.mkdir(parents=True, exist_ok=True)
        src.write_text("\n".join(keys) + "\n")
    (tmp_repo / "services/flag-service/registry.yaml").write_text(
        json.dumps({"flags": registry or []})
    )


def _entry(note="", reload="live", read_by=("agent",)):
    return {"read_by": list(read_by), "reload": reload, "note": note}


def test_flag_catalog_renders_file_registry_and_notes(tmp_repo):
    flagd = tmp_repo / "services/flagd"
    targeted = _flag("off", on=True, off=False) | {"targeting": {"if": [True, "on"]}}
    (flagd / "game.json").write_text(
        _flags(
            b=_flag("medium", low=0.5, medium=1),
            a=targeted,
            obj=_flag("shape", shape={"k": 1}) | {"state": "DISABLED"},
            s=_flag("x", x="a|b", long="y" * 40),
        )
    )
    _catalog_repo(
        tmp_repo,
        {
            "game": {
                "a": _entry("the switch", read_by=["game-coordinator", "menu"]),
                "b": _entry(reload="per-game"),
                "obj": _entry(reload="none", read_by=[]),
                "s": _entry("pipes a|b", reload="startup"),
            }
        },
        [
            {
                "set": "game",
                "key": "a",
                "kind": "overlay",
                "scope": ["stack"],
                "persist": True,
            },
            {
                "set": "game",
                "key": "b",
                "kind": "overlay",
                "scope": ["experiment+arm", "real"],
                "bounds": {"range": [0, 4], "integer": True},
                "description": "registry text, not shown",
            },
        ],
    )
    text = g.flag_catalog()
    table = text.split("## Startup-only flags")[0]
    rows = [line for line in table.splitlines() if line.startswith("| `")]
    assert [r.split(" |")[0] for r in rows] == [
        "| `a`",
        "| `b`",
        "| `obj` (DISABLED)",
        "| `s`",
    ]
    assert rows[0] == (
        "| `a` | bool | `on`=true, `off`=false | `off` = false | yes | "
        "overlay, stack; any declared variant; persistable | live | game-coordinator, menu | "
        "the switch |"
    )
    # The note has one home: a registry description is not shown. Mixed numbers are floats.
    assert rows[1] == (
        "| `b` | float | `low`=0.5, `medium`=1 | `medium` = 1 |  | "
        "overlay, experiment+arm or real; 0 to 4 (integer) | per-game | agent |  |"
    )
    assert (
        rows[2]
        == "| `obj` (DISABLED) | object | `shape` | `shape` = (object) |  | none | none | (none) |  |"
    )
    # A long value shows its name only; every pipe is escaped.
    assert '`x`="a\\|b", `long` |' in rows[3]
    assert rows[3].endswith("| startup | agent | pipes a\\|b |")
    assert "## `game`" in text
    # The generated startup-only and unread lists (flags.md §1.3, §11).
    lists = text.split("## Startup-only flags")[1]
    startup, unread = lists.split("## Flags without an effective reader")
    assert [r for r in startup.splitlines() if r.startswith("| `")] == [
        "| `game` | `s` | agent |"
    ]
    assert [r for r in unread.splitlines() if r.startswith("| `")] == [
        "| `game` | `obj` |"
    ]


def test_a_read_by_claim_the_code_does_not_back_fails(tmp_repo):
    (tmp_repo / "services/flagd/game.json").write_text(
        _flags(a=_flag("x", x=1), b=_flag("x", x=1))
    )
    notes = {
        "game": {
            "a": _entry(read_by=["agent", "menu"]),
            "b": _entry(read_by=["agent"]),
        }
    }
    _catalog_repo(tmp_repo, notes, unread={("game", "a")})
    # A test file naming the key does not count.
    test_file = tmp_repo / "services/menu/tests/test_reads.py"
    test_file.parent.mkdir(parents=True)
    test_file.write_text('"a"\n')
    with pytest.raises(
        g.SpecSourceError,
        match=r"never names the key: \['game/a \(agent\)', 'game/a \(menu\)'\]",
    ):
        g.flag_catalog()


def test_every_real_read_by_claim_is_backed_by_code():
    sets = g._flag_files(g.FLAGD)
    g._check_readers(g._load_flag_notes(sets))


@pytest.mark.parametrize(
    "notes, match",
    [
        ({"game": {}}, r"no entry for \['game/a'\]"),
        (
            {"game": {"a": _entry(), "gone": _entry()}},
            r"no longer exist: \['game/gone'\]",
        ),
        ({"game": {"a": _entry(reload="sometimes")}}, "reload must be one of"),
        ({"game": {"a": _entry(read_by=["nobody"])}}, "read_by must be a list"),
        (
            {"game": {"a": _entry(reload="none")}},
            "reload is none exactly when read_by is",
        ),
        ({"game": {"a": _entry(read_by=[])}}, "reload is none exactly when read_by is"),
        (
            {"game": {"a": _entry() | {"writer": "menu"}}},
            "only read_by, reload and note",
        ),
    ],
)
def test_flag_catalog_notes_must_match_the_flag_files(tmp_repo, notes, match):
    (tmp_repo / "services/flagd/game.json").write_text(_flags(a=_flag("x", x=1)))
    _catalog_repo(tmp_repo, notes)
    with pytest.raises(g.SpecSourceError, match=match):
        g.flag_catalog()


def test_flag_catalog_rejects_a_registry_row_without_a_flag(tmp_repo):
    (tmp_repo / "services/flagd/game.json").write_text(_flags(a=_flag("x", x=1)))
    _catalog_repo(
        tmp_repo,
        {"game": {"a": _entry()}},
        [{"set": "game", "key": "gone", "kind": "overlay", "scope": ["stack"]}],
    )
    with pytest.raises(
        g.SpecSourceError, match=r"no flag file declares: \['game/gone'\]"
    ):
        g.flag_catalog()


def test_changing_a_default_or_adding_a_flag_changes_only_its_own_row(tmp_repo):
    flagd = tmp_repo / "services/flagd"
    notes = {"game": {"a": _entry(), "c": _entry()}}
    (flagd / "game.json").write_text(_flags(a=_flag("x", x=1, y=2), c=_flag("x", x=1)))
    _catalog_repo(tmp_repo, notes)
    before = g.flag_catalog()

    (flagd / "game.json").write_text(
        _flags(a=_flag("y", x=1, y=2), b=_flag("x", x=3), c=_flag("x", x=1))
    )
    notes["game"]["b"] = _entry("new")
    _catalog_repo(tmp_repo, notes)
    assert _changed_lines(before, g.flag_catalog()) == [
        "- | `a` | integer | `x`=1, `y`=2 | `x` = 1 |  | none | live | agent |  |",
        "+ | `a` | integer | `x`=1, `y`=2 | `y` = 2 |  | none | live | agent |  |",
        "+ | `b` | integer | `x`=3 | `x` = 3 |  | none | live | agent | new |",
    ]


def test_flag_catalog_covers_every_flag_of_the_real_files():
    text = g.flag_catalog()
    for s, flags in g._flag_files(g.FLAGD).items():
        assert f"## `{s}`" in text
        for key in flags:
            assert f"| `{key}`" in text, f"{s}/{key}"


@pytest.mark.parametrize(
    "bounds, cell",
    [
        ({"variants": ["on", "off"]}, "variants `on`, `off`"),
        (
            {"pattern": [["float 0.1 30"], ["enum a", "int 1 2"]]},
            "`float 0.1 30` or `enum a:int 1 2`",
        ),
        ({"free_string": True}, "any string"),
        ({"payload": {}}, "event payload: none"),
        ({"fraction": [1, 100]}, "fraction 1 to 100 %"),
        ({"shape": True}, "object of the default's shape"),
    ],
)
def test_registry_bounds_cells(bounds, cell):
    assert g._bounds(bounds) == cell


def test_unknown_registry_bounds_fail():
    with pytest.raises(g.SpecSourceError, match="unknown bounds"):
        g._bounds({"mystery": 1})
