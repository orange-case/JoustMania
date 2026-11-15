"""The fake hosted Flagsmith itself (#2592): it keeps state the way Flagsmith does, so the
Flagsmith link journeys (test_flagsmith_link.py, test_flagsmith_sync.py) test the link, not
the fake. Pinned to the Go client's requests (services/flag-service/flagsmith: push.go,
identities.go, audit.go, document.go, in.go).

No stack is started here; these run without docker or go.
"""

from __future__ import annotations

import email.utils
import json
import time
import urllib.error
import urllib.request

import pytest

from tests.service.harness.fakes.flagsmith import ADMIN_KEY, ENVIRONMENT_KEY, SERVER_KEY, FakeFlagsmith

pytestmark = pytest.mark.tier("service")


@pytest.fixture
def fake():
    f = FakeFlagsmith()
    f.preseed([("agent", "experiment_tick_seconds"), ("agent", "decision.capture_content"), ("game", "sensitivity")])
    f.start()
    yield f
    f.stop()


def _call(fake, method, path, body=None, headers=None):
    req = urllib.request.Request(
        f"http://127.0.0.1:{fake.port}{path}",
        method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": f"Api-Key {ADMIN_KEY}", "Content-Type": "application/json", **(headers or {})},
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            raw = resp.read()
            return resp.status, dict(resp.headers.items()), json.loads(raw) if raw else None
    except urllib.error.HTTPError as err:
        raw = err.read()
        return err.code, dict(err.headers.items()), json.loads(raw) if raw else None


def _document(fake, ims=None):
    headers = {"X-Environment-Key": SERVER_KEY}
    if ims:
        headers["If-Modified-Since"] = ims
    return _call(fake, "GET", "/api/v1/environment-document/", headers=headers)


def _state(doc, name):
    return next(fs for fs in doc["feature_states"] if fs["feature"]["name"] == name)


def test_patch_changes_the_document_and_adds_an_api_key_audit_row(fake):
    _, _, before = _document(fake)
    assert _state(before, "experiment_tick_seconds")["enabled"] is False
    fid = fake.feature_id("experiment_tick_seconds")
    status, _, _ = _call(
        fake,
        "PATCH",
        f"/api/__future__/environments/{ENVIRONMENT_KEY}/features/{fid}/",
        {"environment_default": {"enabled": True, "value": {"type": "integer", "value": "12"}}},
    )
    assert status == 200
    _, headers, after = _document(fake)
    fs = _state(after, "experiment_tick_seconds")
    assert fs["enabled"] is True and fs["feature_state_value"] == 12
    assert after["updated_at"] > before["updated_at"]
    assert float(headers["X-Flagsmith-Document-Updated-At"]) > 0
    assert fake.audit_rows[0] == {"log": "Flag state updated for feature: experiment_tick_seconds", "author": None}
    assert len(fake.pushes("experiment_tick_seconds")) == 1


def test_patch_of_an_unknown_feature_is_a_json_404(fake):
    status, _, body = _call(
        fake,
        "PATCH",
        f"/api/__future__/environments/{ENVIRONMENT_KEY}/features/999999/",
        {"environment_default": {"enabled": True, "value": {"type": "string", "value": "x"}}},
    )
    assert status == 404 and "detail" in body


def test_conditional_pull_answers_304_until_a_change(fake):
    _, headers, _ = _document(fake)
    last_modified = headers["Last-Modified"]
    status, _, _ = _document(fake, ims=last_modified)
    assert status == 304
    time.sleep(1.1)  # Last-Modified has a one-second resolution
    fake.person_sets("decision.capture_content", "off")
    status, headers, doc = _document(fake, ims=last_modified)
    assert status == 200
    assert email.utils.parsedate_to_datetime(headers["Last-Modified"]) > email.utils.parsedate_to_datetime(
        last_modified
    )
    assert _state(doc, "decision.capture_content")["feature_state_value"] == "off"
    assert fake.audit_rows[0]["author"] == {"id": 1, "email": "operator@example.invalid"}


def test_quoted_identity_query_is_exact_unquoted_a_prefix(fake):
    fake.add_identity("game:g1")
    fake.add_identity("game:g10")
    base = f"/api/v1/environments/{ENVIRONMENT_KEY}/edge-identities/"
    _, _, exact = _call(fake, "GET", base + "?q=%22game%3Ag1%22")
    assert [r["identifier"] for r in exact["results"]] == ["game:g1"]
    _, _, prefix = _call(fake, "GET", base + "?q=game%3Ag1")
    assert sorted(r["identifier"] for r in prefix["results"]) == ["game:g1", "game:g10"]


def test_identity_create_duplicate_override_and_delete(fake):
    base = f"/api/v1/environments/{ENVIRONMENT_KEY}/"
    status, _, created = _call(fake, "POST", base + "edge-identities/", {"identifier": "game:g2"})
    assert status == 201 and created["identity_uuid"]
    status, _, dup = _call(fake, "POST", base + "edge-identities/", {"identifier": "game:g2"})
    assert status == 400 and "already exist" in json.dumps(dup).lower()
    status, _, _ = _call(
        fake, "PUT", base + f"edge-identities/{created['identity_uuid']}/update-traits/",
        {"trait_key": "game_kind", "trait_value": "real"},
    )  # fmt: skip
    assert status == 200 and fake.identities()["game:g2"]["traits"] == {"game_kind": "real"}
    fid = fake.feature_id("sensitivity")
    status, _, _ = _call(
        fake, "PUT", base + "edge-identities-featurestates",
        {"identifier": "game:g2", "feature": fid, "enabled": True, "feature_state_value": "2"},
    )  # fmt: skip
    assert status == 200 and fake.override("game:g2", "sensitivity") == {"enabled": True, "value": "2"}
    assert fake.audit_rows[0]["log"] == "Feature override created for feature 'sensitivity' and identity 'game:g2'"
    _, _, doc = _document(fake)
    assert [o["identifier"] for o in doc["identity_overrides"]] == ["game:g2"]
    status, _, _ = _call(fake, "DELETE", base + f"edge-identities/{created['identity_uuid']}/")
    assert status == 204 and "game:g2" not in fake.identities()
    _, _, doc = _document(fake)
    assert doc["identity_overrides"] == []


def test_refusal_knob_counts_down_and_carries_retry_after(fake):
    fid = fake.feature_id("experiment_tick_seconds")
    path = f"/api/__future__/environments/{ENVIRONMENT_KEY}/features/{fid}/"
    body = {"environment_default": {"enabled": True, "value": {"type": "integer", "value": "3"}}}
    fake.refuse("experiment_tick_seconds", 429, retry_after=2, times=1)
    status, headers, _ = _call(fake, "PATCH", path, body)
    assert status == 429 and headers["Retry-After"] == "2"
    assert _call(fake, "PATCH", path, body)[0] == 200
    fake.refuse("experiment_tick_seconds", 409)
    status, _, refused = _call(fake, "PATCH", path, body)
    assert status == 409 and refused == {"detail": "change_requests_enabled"}
    assert fake.env_value("experiment_tick_seconds") == (True, 3)


def test_health_webhook_is_recorded_without_the_admin_key(fake):
    req = urllib.request.Request(
        f"http://127.0.0.1:{fake.port}/health-webhook/",
        method="POST",
        data=json.dumps({"feature": "x", "status": "unhealthy"}).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        assert resp.status == 200
    assert fake.health_posts() == [{"feature": "x", "status": "unhealthy"}]


def test_reset_values_keeps_features_and_drops_the_rest(fake):
    fake.person_sets("decision.capture_content", "off")
    fake.person_overrides("game:g3", "sensitivity", "1")
    fake.down = True
    fake.reset_values()
    assert fake.env_value("decision.capture_content") == (False, None)
    assert fake.identities() == {} and fake.audit_rows == [] and fake.down is False
    assert len(fake.features) == 3
