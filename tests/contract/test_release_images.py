"""Every released image is tagged on release and retagged on main pushes (#2027 F1).

``release.yml`` retags ``IMAGE:<sha>`` to ``IMAGE:<version>``; ``ci.yml`` keeps a
``:<sha>`` tag on every image it did not rebuild; ``check-retag-source.sh`` must know
each retagged image. A service in ``docker-compose.yml`` missing from any of the three
(the agent was, until #2027) has no ``:<version>`` tag, so a Pi pinned to a release
cannot pull it.
"""

from __future__ import annotations

import re

import infra_config as ic
import pytest

# #2599: compose takes the registry prefix from IMAGE_PREFIX (CI exports the
# publishing repository's prefix); the default is the release registry.
# #2614: the GitHub repository that publishes the images. ci.yml and release.yml derive
# the prefix from it at run time; compose cannot, so its default must match this repo.
PUBLISHING_REPO = "orange-case/JoustMania"
DEFAULT_PREFIX = "ghcr.io/orange-case/joustmania"
# The one expression that sets IMAGE_PREFIX in ci.yml (publish) and release.yml (retag).
PREFIX_EXPR = "ghcr.io/${GITHUB_REPOSITORY,,}"
PREFIX = "${IMAGE_PREFIX:-" + DEFAULT_PREFIX + "}/"
# Compose images under PREFIX that CI deliberately does not release. None today.
NOT_RELEASED: set[str] = set()


def _compose_images(name: str) -> set[str]:
    services = ic.load_yaml(ic.REPO_ROOT / name)["services"]
    return {
        m.group(1)
        for svc in services.values()
        if (m := re.match(re.escape(PREFIX) + r"([a-z0-9-]+):", svc.get("image", "")))
    }


def _release_images() -> set[str]:
    wf = ic.load_yaml(ic.REPO_ROOT / ".github/workflows/release.yml")
    return set(wf["jobs"]["tag-images"]["strategy"]["matrix"]["image"])


def _ci_retag_images() -> set[str]:
    text = (ic.REPO_ROOT / ".github/workflows/ci.yml").read_text()
    return set(
        re.findall(
            r'^\s*maybe_retag "\$CH_[A-Z_]+"\s+([a-z0-9-]+)\s*$', text, re.MULTILINE
        )
    )


def _check_retag_source_images() -> set[str]:
    text = (ic.REPO_ROOT / "scripts/ci/check-retag-source.sh").read_text()
    return set(re.findall(r"^\s+([a-z0-9-]+)\) filter=\w+ ;;\s*$", text, re.MULTILINE))


@pytest.mark.parametrize("compose", ["docker-compose.yml", "docker-compose.lite.yml"])
def test_compose_images_take_the_registry_prefix_from_image_prefix(compose):
    """#2599: a hard-coded registry prefix made the CI fast path pull images from a
    registry this repository does not publish to, while the freshness guard checked
    ${IMAGE_PREFIX}: every project image must follow IMAGE_PREFIX."""
    services = ic.load_yaml(ic.REPO_ROOT / compose)["services"]
    project = {
        name: svc["image"]
        for name, svc in services.items()
        if "joustmania/" in svc.get("image", "") or "joustmania}/" in svc.get("image", "")
    }
    assert project, f"{compose}: no project images found"
    hard_coded = {n: i for n, i in project.items() if not i.startswith(PREFIX)}
    assert not hard_coded, f"{compose}: images must start with {PREFIX}: {hard_coded}"


def test_collector_overlay_uses_the_compose_image_prefix():
    """The prebuilt-collector overlay and docker-compose.yml resolve the same registry."""
    text = (ic.REPO_ROOT / "docker-compose.collector-prebuilt.yml").read_text()
    # Regex, not yaml: the overlay uses the compose-only !reset tag.
    m = re.search(r"^\s+image:\s*(\S+)\s*$", text, re.MULTILINE)
    assert m and m.group(1).startswith(PREFIX + "otel-collector:"), m and m.group(1)
    main = ic.compose_services()["otel-collector"]["image"]
    assert main.startswith(PREFIX + "otel-collector:"), main


@pytest.mark.parametrize("compose", ["docker-compose.yml", "docker-compose.lite.yml"])
def test_compose_images_are_released(compose):
    missing = _compose_images(compose) - NOT_RELEASED - _release_images()
    assert not missing, (
        f"{compose} images not in release.yml tag-images: {sorted(missing)}"
    )


def test_release_images_cover_main_compose():
    # python-hid and operator-pad are profile-gated but still released.
    assert _release_images() == _compose_images("docker-compose.yml") - NOT_RELEASED


def test_ci_retag_list_equals_release_list():
    assert _ci_retag_images() == _release_images()


def test_check_retag_source_knows_every_release_image():
    assert _check_retag_source_images() == _release_images()


def _ci_ch_output_keys() -> dict[str, str]:
    """CH_X env name -> paths-filter output key, from the matrices step's env."""
    text = (ic.REPO_ROOT / ".github/workflows/ci.yml").read_text()
    return dict(
        re.findall(
            r"^\s+(CH_[A-Z_]+): \$\{\{ steps\.f\.outputs\.(\w+) \}\}\s*$",
            text,
            re.MULTILINE,
        )
    )


def _ci_retag_pairs() -> dict[str, str]:
    """image -> CH_X variable used in ``maybe_retag``."""
    text = (ic.REPO_ROOT / ".github/workflows/ci.yml").read_text()
    return {
        image: ch
        for ch, image in re.findall(
            r'^\s*maybe_retag "\$(CH_[A-Z_]+)"\s+([a-z0-9-]+)\s*$', text, re.MULTILINE
        )
    }


def _check_retag_source_filters() -> dict[str, str]:
    text = (ic.REPO_ROOT / "scripts/ci/check-retag-source.sh").read_text()
    return dict(
        re.findall(r"^\s+([a-z0-9-]+)\) filter=(\w+) ;;\s*$", text, re.MULTILINE)
    )


def _ci_filter_keys() -> set[str]:
    wf = ic.load_yaml(ic.REPO_ROOT / ".github/workflows/ci.yml")
    for step in wf["jobs"]["changes"]["steps"]:
        if step.get("id") == "f":
            return set(ic.yaml.safe_load(step["with"]["filters"]))
    raise AssertionError("no paths-filter step with id 'f'")


def test_retag_ch_variable_matches_check_retag_source_filter():
    """maybe_retag's CH_X must map to the same filter key check-retag-source.sh
    uses for that image, and that key must exist in ci.yml's paths-filter."""
    ch_keys = _ci_ch_output_keys()
    filters = _check_retag_source_filters()
    filter_keys = _ci_filter_keys()
    for image, ch in _ci_retag_pairs().items():
        assert ch in ch_keys, f"{ch} (for {image}) has no env entry in ci.yml"
        assert ch_keys[ch] == filters[image], (
            f"{image}: maybe_retag uses {ch} -> {ch_keys[ch]!r} but "
            f"check-retag-source.sh uses filter {filters[image]!r}"
        )
        assert ch_keys[ch] in filter_keys, (
            f"{image}: filter {ch_keys[ch]!r} not in ci.yml paths-filter"
        )


def _prefix_assignments(workflow: str) -> list[str]:
    """Every value a workflow gives IMAGE_PREFIX: `IMAGE_PREFIX=...` written to
    $GITHUB_ENV in a run step, and `IMAGE_PREFIX:` in any env block."""
    path = ic.REPO_ROOT / ".github/workflows" / workflow
    found = re.findall(r'IMAGE_PREFIX=(\S+?)"? >>', path.read_text())
    wf = ic.load_yaml(path)
    blocks = [wf.get("env") or {}]
    for job in wf["jobs"].values():
        blocks.append(job.get("env") or {})
        blocks.extend(st.get("env") or {} for st in job.get("steps", []))
    found.extend(str(b["IMAGE_PREFIX"]) for b in blocks if "IMAGE_PREFIX" in b)
    return found


def test_release_retag_uses_the_prefix_ci_publishes_to():
    """#2614: release.yml hard-coded ghcr.io/watchmejoustmyflags/joustmania after the
    org rename while CI published to ghcr.io/orange-case/joustmania, so the 0.15.0
    retag waited for IMAGE:<sha> in a registry nothing pushes to. Both workflows must
    set IMAGE_PREFIX only with the same run-time expression."""
    ci = _prefix_assignments("ci.yml")
    release = _prefix_assignments("release.yml")
    assert ci and set(ci) == {PREFIX_EXPR}, f"ci.yml IMAGE_PREFIX values: {ci}"
    assert release and set(release) == {PREFIX_EXPR}, (
        f"release.yml IMAGE_PREFIX values {release} must all be {PREFIX_EXPR!r} (as ci.yml)"
    )
    steps = ic.load_yaml(ic.REPO_ROOT / ".github/workflows/release.yml")["jobs"][
        "tag-images"
    ]["steps"]
    names = [st.get("name", "") for st in steps]
    assert "Set image prefix" in names and "Tag image with release version" in names
    assert names.index("Set image prefix") < names.index(
        "Tag image with release version"
    ), "tag-images must set IMAGE_PREFIX before the retag step"


def test_compose_default_is_the_publishing_registry():
    """#2614: the Pi sets no IMAGE_PREFIX, so the compose default is what it pulls; it
    must be the prefix CI's expression yields for the publishing repository."""
    assert DEFAULT_PREFIX == "ghcr.io/" + PUBLISHING_REPO.lower()
    for name in (
        "docker-compose.yml",
        "docker-compose.lite.yml",
        "docker-compose.collector-prebuilt.yml",
    ):
        text = (ic.REPO_ROOT / name).read_text()
        defaults = set(re.findall(r"\$\{IMAGE_PREFIX:-([^}]*)\}", text))
        assert defaults == {DEFAULT_PREFIX}, f"{name}: IMAGE_PREFIX defaults {defaults}"
