#!/usr/bin/env python3
"""Offline archetype fit (UC-047): propose new ``experiment.mock_archetype`` parameters.

Reads data that already exists and writes nothing unless ``--write`` is given:

  * real games: the shadow realism dataset (``scripts/agent-shadow-realism.sh --save``) or, with
    ``--journal``, a copy of the agent's game journal ``calibration/real_games.jsonl``;
  * shadow games: the same realism dataset, labelled with their archetype from a copy of the
    agent's experiment journals (``<experiments dir>/<experiment id>/events.jsonl``);
  * the current archetypes: the ``experiment.mock_archetype`` flag in ``--flags``.

It keeps the eligible real games (UC-047 BR-002), splits them into three bands by elimination
pace and scales each archetype's parameter groups by the ratio of its band's median to the
archetype's own shadow median (BR-005). The report goes to stdout; ``--write FILE`` rewrites the
changed values in a flagd file in place, for a pull request a human reviews (UC-047 step 6).

Exit codes: 0 proposal made, 2 unreadable input, 3 too few eligible real games (A10).
Stdlib only, deterministic: the same inputs give the same proposal.
"""

from __future__ import annotations

import argparse
import json
import math
import re
import sys
from collections import Counter
from pathlib import Path

FLAG = "experiment.mock_archetype"
MODES = {"FFA", "JoustFFA"}  # realism dataset label, journal mode name
MIN_DURATION_S = 10.0
MOVE_FLOOR = 0.005  # g^2
DOMINANCE = 10.0
BAND_MIN = 8
MIN_SHADOW_GAMES = 4
ROUND = 4
ROUND_STEPS = 100  # ratio steps of 0.01 %, at most 1 % below the clamp

BAND_OF = {
    "calm_long": "low",
    "slow_cascade": "low",
    "close_race": "high",
    "quick_cascade": "high",
}
# metric -> (parameters it scales, whether the ratio applies as its square root)
GROUPS = {
    "intensity": (("base", "d_base"), False),
    "movement_variance": (("amp", "d_amp", "burst_h"), True),
    "elimination_pace": (("drift", "d_drift"), False),
}


class InputError(Exception):
    pass


def _finite(x) -> bool:
    return isinstance(x, (int, float)) and not isinstance(x, bool) and math.isfinite(x)


def _mean(xs):
    xs = [x for x in xs if _finite(x)]
    return sum(xs) / len(xs) if xs else None


def median(xs):
    xs = sorted(x for x in xs if _finite(x))
    if not xs:
        return None
    m = len(xs) // 2
    return xs[m] if len(xs) % 2 else (xs[m - 1] + xs[m]) / 2


def _load_json(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError) as e:
        raise InputError(f"{path}: {e}") from e


def _jsonl(path):
    try:
        lines = Path(path).read_text().splitlines()
    except OSError as e:
        raise InputError(f"{path}: {e}") from e
    out = []
    for line in lines:
        try:
            out.append(json.loads(line))
        except ValueError:
            continue  # a torn last line of a live journal
    return out


# ------------------------------------------------------------------ games


def _game(gid, mode, duration, pace, players, arm=""):
    """players: list of (intensity, movement variance) per player, None when unknown."""
    return {
        "game_id": gid,
        "mode": mode or "",
        "duration": duration,
        "elimination_pace": pace,
        "arm": arm,
        "player_variances": [v for _, v in players if _finite(v)],
        "intensity": _mean(i for i, _ in players),
        "movement_variance": _mean(v for _, v in players),
    }


def dataset_games(ds) -> dict[str, list[dict]]:
    by_game: dict[str, list] = {}
    for p in ds.get("players", []):
        by_game.setdefault(p.get("game_id"), []).append(
            (p.get("intensity"), p.get("movement_variance"))
        )
    out: dict[str, list[dict]] = {"real": [], "shadow": []}
    for g in ds.get("games", []):
        if g.get("kind") in out:
            out[g["kind"]].append(
                _game(
                    g.get("game_id"),
                    g.get("mode"),
                    g.get("duration"),
                    g.get("elimination_pace"),
                    by_game.get(g.get("game_id"), []),
                    g.get("arm", ""),
                )
            )
    return out


def journal_games(records) -> list[dict]:
    return [
        _game(
            r.get("game_id"),
            r.get("mode"),
            r.get("duration_seconds"),
            (r.get("metrics") or {}).get("elimination_pace"),
            [
                (p.get("mean_intensity"), p.get("last_movement_variance"))
                for p in r.get("players") or []
            ],
        )
        for r in records
        if r.get("schema_version") == 1
    ]


def exclusion(g) -> str | None:
    """BR-002: the first failing reason, or None for an eligible real game."""
    if g["mode"] not in MODES:
        return "other_mode"
    if not _finite(g["duration"]) or not _finite(g["elimination_pace"]):
        return "metrics_missing"
    if g["duration"] < MIN_DURATION_S:
        return "too_short"
    if sum(v >= MOVE_FLOOR for v in g["player_variances"]) < 2:
        return "too_few_moved"
    top = sorted(g["player_variances"], reverse=True)
    if top[0] >= DOMINANCE * top[1]:
        return "dominant"
    return None


def bands(games: list[dict]) -> dict[str, list[dict]]:
    """Thirds by elimination pace (ties in input order), the remainder in the middle band."""
    order = sorted(range(len(games)), key=lambda i: (games[i]["elimination_pace"], i))
    k = len(games) // 3
    ranked = [games[i] for i in order]
    return {
        "low": ranked[:k],
        "middle": ranked[k : len(ranked) - k],
        "high": ranked[len(ranked) - k :],
    }


def shadow_labels(exp_dir) -> dict[str, dict]:
    """game id -> {archetype, arm, params} from the experiment journals' events."""
    root = Path(exp_dir)
    if not root.is_dir():
        raise InputError(f"{exp_dir}: not a directory")
    labels: dict[str, dict] = {}
    for events in sorted(root.glob("*/events.jsonl")):
        pinned: dict[str, dict] = {}
        for e in _jsonl(events):
            pat = e.get("pattern") or {}
            if e.get("kind") == "pattern_pinned":
                for name, params in zip(
                    pat.get("archetypes") or [], pat.get("params_json") or []
                ):
                    try:
                        pinned[name] = json.loads(params)
                    except ValueError:
                        pass
            elif e.get("kind") == "game_assigned" and pat.get("archetype"):
                labels[e.get("game_id", "")] = {
                    "archetype": pat["archetype"],
                    "arm": e.get("arm", ""),
                    "params": pinned.get(pat["archetype"]),
                }
    return labels


# ------------------------------------------------------------------ fit


def _slots(base, step, n):
    return [base + i * step for i in range(int(n))]


def ratio_limit(params: dict, metric: str) -> float:
    """The largest ratio that keeps every slot value inside UC-046 BR-003's bounds."""
    lim = math.inf
    n = params.get("players", 1)

    def cap(value, bound):
        nonlocal lim
        if value > 0:
            lim = min(lim, bound / value)

    if metric == "intensity":
        for v in _slots(params["base"], params["d_base"], n):
            cap(v, 10)
        cap(abs(params["d_base"]), 1)
    elif metric == "movement_variance":
        for v in _slots(params["amp"], params["d_amp"], n):
            cap(v, 10)
        cap(params["burst_h"], 10)
        cap(abs(params["d_amp"]), 1)
    else:
        cap(abs(params["drift"]), 1)
        cap(abs(params["d_drift"]), 1)
    return lim


def fit_archetype(name, params, band_games, shadow_games) -> dict:
    band = BAND_OF.get(name, "middle")
    out = {
        "archetype": name,
        "band": band,
        "shadow_games": len(shadow_games),
        "status": "fitted",
        "ratios": {},
        "notes": [],
        "params": dict(params),
    }
    if len(shadow_games) < MIN_SHADOW_GAMES:
        out["status"] = "insufficient_shadow"
        return out
    new = dict(params)
    for metric, (keys, sqrt) in GROUPS.items():
        real_med = median(g[metric] for g in band_games)
        shadow_med = median(g[metric] for g in shadow_games)
        if not real_med or not shadow_med:  # None or 0: no ratio
            out["notes"].append(f"ratio_undefined:{metric}")
            out["ratios"][metric] = 1.0
            continue
        r = real_med / shadow_med
        if sqrt:
            r = math.sqrt(r)
        limit = ratio_limit(params, metric)
        if r > limit:
            out["notes"].append(f"clamped:{metric}")
            r = limit
        # Fields round independently, so a slot at a bound can round past it.
        for step in range(ROUND_STEPS):
            trial = {
                k: round(params[k] * r, ROUND) if params[k] else params[k] for k in keys
            }
            if valid({**new, **trial}):
                break
            r *= 1 - 1e-4
        else:
            out["notes"].append(f"unrepresentable:{metric}")
            out["ratios"][metric] = 1.0
            continue
        if step:
            out["notes"].append(f"rounded:{metric}")
        out["ratios"][metric] = round(r, ROUND)
        new.update(trial)
    out["params"] = new
    return out


def valid(p: dict) -> bool:
    """The numeric bounds of UC-046 BR-003, per slot."""
    n = int(p.get("players", 1))
    slots = _slots(p["base"], p["d_base"], n) + _slots(p["amp"], p["d_amp"], n)
    return (
        all(0 <= v <= 10 for v in slots)
        and all(0 <= p[k] <= 10 for k in ("base", "amp", "burst_h"))
        and all(-1 <= p[k] <= 1 for k in ("drift", "d_base", "d_drift", "d_amp"))
    )


def propose(real, shadow, labels, archetypes) -> dict:
    excluded = Counter()
    eligible = []
    for g in real:
        why = exclusion(g)
        if why:
            excluded[why] += 1
        else:
            eligible.append(g)
    rep = {
        "real_games": len(real),
        "eligible": len(eligible),
        "excluded": dict(sorted(excluded.items())),
        "status": "proposal",
        "archetypes": [],
    }
    if len(eligible) < 3 * BAND_MIN:
        rep["status"] = "insufficient_games"
        return rep
    by_band = bands(eligible)
    rep["bands"] = {
        b: {
            "games": len(gs),
            **{m: median(g[m] for g in gs) for m in GROUPS},
        }
        for b, gs in by_band.items()
    }
    skipped = Counter()
    per_arch: dict[str, list] = {}
    for g in shadow:
        lab = labels.get(g["game_id"])
        if lab is None:
            skipped["unlabelled"] += 1
        elif lab["arm"] != "control":
            skipped["experimental_arm"] += 1
        elif lab["archetype"] not in archetypes:
            skipped["unknown_archetype"] += 1
        elif lab["params"] != archetypes[lab["archetype"]]:
            skipped["params_changed"] += 1
        else:
            per_arch.setdefault(lab["archetype"], []).append(g)
    rep["shadow_skipped"] = dict(sorted(skipped.items()))
    for name in sorted(archetypes):
        rep["archetypes"].append(
            fit_archetype(
                name,
                archetypes[name],
                by_band[BAND_OF.get(name, "middle")],
                per_arch.get(name, []),
            )
        )
    return rep


# ------------------------------------------------------------------ flag file


def flag_archetypes(flags_doc) -> dict[str, dict]:
    try:
        variants = flags_doc["flags"][FLAG]["variants"]
    except (KeyError, TypeError) as e:
        raise InputError(f"no {FLAG} variants: {e}") from e
    return {k: v for k, v in variants.items() if k != "off"}


def _num(x) -> str:
    return repr(float(x)) if isinstance(x, float) else str(x)


def rewrite(text: str, changes: dict[str, dict]) -> str:
    """Replace changed numbers inside the flag's variant objects, keeping the file's layout."""
    start = text.index(f'"{FLAG}": {{')
    for name, new in changes.items():
        head = text.index(f'"{name}": {{', start)
        end, depth = text.index("{", head), 0
        while True:  # the variant's closing brace; trigger_policy nests one object
            depth += {"{": 1, "}": -1}.get(text[end], 0)
            if depth == 0:
                break
            end += 1
        body = text[head:end]
        for key, value in new.items():
            body, n = re.subn(
                rf'("{re.escape(key)}":\s*)-?[0-9][0-9.eE+-]*',
                rf"\g<1>{_num(value)}",
                body,
            )
            if n != 1:
                raise InputError(f"{name}.{key}: found {n} times in the flag file")
        text = text[:head] + body + text[end:]
    return text


def write_flags(paths, rep):
    """Rewrite every file or none: all new texts are built before the first write."""
    changes = {}
    for a in rep["archetypes"]:
        if a["status"] != "fitted":
            continue
        changed = {k: v for k, v in a["params"].items() if v != a["old"].get(k)}
        if changed:
            changes[a["archetype"]] = changed
    outs = []
    for path in paths:
        p = Path(path)
        try:
            raw = p.read_bytes().decode()
            crlf = "\r\n" in raw
            text = rewrite(raw.replace("\r\n", "\n"), changes)
            variants = json.loads(text)["flags"][FLAG]["variants"]
            ok = all(variants[a["archetype"]] == a["params"] for a in rep["archetypes"])
        except (OSError, ValueError, KeyError, TypeError) as e:
            raise InputError(f"{path}: {e!r}") from e
        if not ok:
            raise InputError(f"{path}: the variants did not round-trip")
        outs.append((p, text.replace("\n", "\r\n") if crlf else text))
    for p, text in outs:
        p.write_bytes(text.encode())


# ------------------------------------------------------------------ report


def render(rep) -> str:
    lines = [
        f"status: {rep['status']}",
        f"real games: {rep['real_games']}, eligible {rep['eligible']}, excluded {rep['excluded']}",
    ]
    if rep["status"] == "insufficient_games":
        lines.append(
            f"need at least {3 * BAND_MIN} eligible real games; nothing proposed"
        )
        return "\n".join(lines)
    for b, v in rep["bands"].items():
        lines.append(f"band {b}: " + ", ".join(f"{k} {v[k]}" for k in v))
    lines.append(f"shadow games skipped: {rep['shadow_skipped']}")
    for a in rep["archetypes"]:
        lines.append(
            f"{a['archetype']} ({a['band']}, {a['shadow_games']} shadow games): {a['status']}"
            + (f" ratios {a['ratios']}" if a["ratios"] else "")
            + (f" notes {a['notes']}" if a["notes"] else "")
        )
        for k, v in a["params"].items():
            if v != a["old"].get(k):
                lines.append(f"  {k}: {a['old'][k]} -> {v}")
    return "\n".join(lines)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument(
        "--dataset",
        required=True,
        help="realism dataset (agent-shadow-realism.sh --save)",
    )
    ap.add_argument(
        "--experiments", required=True, help="copy of the agent's experiments directory"
    )
    ap.add_argument(
        "--journal", help="copy of calibration/real_games.jsonl (real games from it)"
    )
    ap.add_argument("--flags", default="services/flagd/agent.json")
    ap.add_argument(
        "--write", action="append", default=[], help="flagd file to rewrite in place"
    )
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args(argv)
    try:
        ds = dataset_games(_load_json(a.dataset))
        real = journal_games(_jsonl(a.journal)) if a.journal else ds["real"]
        archetypes = flag_archetypes(_load_json(a.flags))
        rep = propose(real, ds["shadow"], shadow_labels(a.experiments), archetypes)
        for arch in rep["archetypes"]:
            arch["old"] = archetypes[arch["archetype"]]
        if rep["status"] == "proposal" and a.write:
            write_flags(a.write, rep)
    except InputError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    print(json.dumps(rep, indent=2, sort_keys=True) if a.json else render(rep))
    return 3 if rep["status"] == "insufficient_games" else 0


if __name__ == "__main__":
    sys.exit(main())
