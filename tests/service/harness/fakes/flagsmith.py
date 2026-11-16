"""FakeFlagsmith: a stateful hosted-Flagsmith stub, one per stack (#2591, #2592, UC-054, UC-055).

The flag service's Flagsmith link (``services/flag-service/flagsmith``) is pointed here
with ``FLAGSMITH_API_URL`` and ``FLAGSMITH_DOCUMENT_URL`` (``flag_service_env()``), so
the service tier never reaches flagsmith.com. The fake keeps state like Flagsmith does
(architecture note flagsmith-sync.md §4, §5; the Go client's requests in push.go,
identities.go, document.go; the audit log is Flagsmith's, the link reads it no more since #2621):

- one environment value per feature (created disabled, value null) and one override
  per (edge identity, feature); the environment document lists them all;
- every change bumps the document's ``updated_at`` (RFC 3339, strictly increasing),
  its ``X-Flagsmith-Document-Updated-At`` header (a float epoch) and ``Last-Modified``
  (the ``updated_at`` second); a pull with ``If-Modified-Since`` at or after that second
  answers 304;
- the link's pushes change that state: ``PATCH /api/__future__/environments/<key>/features/<id>/``
  (``environment_default``: enabled and ``value {type, value}``; an unknown feature 404 with a
  JSON ``detail``), ``PUT|DELETE .../edge-identities-featurestates`` (JSON body, no trailing
  slash; a PUT creates a missing identity), ``POST .../edge-identities/`` (a duplicate is 400
  "already exists"), ``PUT .../edge-identities/<uuid>/update-traits/`` (a null value deletes
  the trait), ``GET .../edge-identities/<uuid>/list-traits/`` and
  ``GET .../edge-identities/<uuid>/edge-featurestates/`` (the controller settle, #2624),
  ``DELETE .../edge-identities/<uuid>/`` (with its overrides);
- ``GET .../edge-identities/?q=``: a quoted ``"x"`` is an exact match, unquoted a prefix;
- an Admin API write adds an audit row (newest first) with ``author: None`` (an API key) and
  the log text Flagsmith writes; ``person_sets`` / ``person_overrides`` change the state
  as a person in the Flagsmith UI (``author: {"id": 1}``);
- ``POST /health-webhook/`` (``FLAGSMITH_HEALTH_WEBHOOK_URL``) records the feature health
  posts (``health_posts()``), answered after the ``down`` check and before the key check.

Knobs: ``down`` (every request 503), ``delay`` (seconds before every answer), ``refuse(...)``
(push answers for one feature: 409 change request, 429 Retry-After, ``times``),
``audit_rows`` (settable), ``audit_status``, ``versioning`` (v2),
``identity_overrides_in_local_eval``. No realtime SSE route (maintainer decision D2, #2592).

An in-process threaded server (stdlib only, instance state), like fakes/github.py.
"""

from __future__ import annotations

import datetime as dt
import email.utils
import json
import re
import threading
import time
import urllib.parse
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from ..processes import free_port

# Dummy credentials the flag service is given; never real ones.
SERVER_KEY = "ser.fake-flagsmith-service-tier"
ADMIN_KEY = "fake-flagsmith-admin-key"
ENVIRONMENT_KEY = "fakeEnvKey123"
PROJECT_ID = 4242
ENVIRONMENT_ID = 77
ENVIRONMENT_NAME = "service-tier"
HEALTH_PATH = "/health-webhook/"

# A person in the Flagsmith UI (an audit row with a user); an API key's rows carry None.
PERSON = {"id": 1, "email": "operator@example.invalid"}

_ENV = re.compile(r"^/api/v1/environments/([^/]+)/$")
_EDGE_LIST = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities/$")
_EDGE_ONE = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities/([^/]+)/$")
_EDGE_TRAITS = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities/([^/]+)/update-traits/$")
# A controller identity (#2624): its traits and its overrides, read by the link.
_EDGE_SUB = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities/([^/]+)/(list-traits|edge-featurestates)/$")
# No trailing slash: push.go sends exactly this path (and Flagsmith 404s the other form).
_EDGE_FS = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities-featurestates$")
# Every Admin path names the environment by its client key and the project by its id.
_ANY_ENV = re.compile(r"^/api/(?:v1|__future__)/environments/([^/]+)/")
_ANY_PROJECT = re.compile(r"^/api/v1/projects/([^/]+)/")
_FUTURE = re.compile(r"^/api/__future__/environments/([^/]+)/features/(\d+)/$")
_PROJECT = re.compile(r"^/api/v1/projects/(\d+)/(features|tags|audit)/$")


def _iso(t: dt.datetime) -> str:
    return t.astimezone(dt.UTC).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def _typed(value: dict) -> object:
    """The value of a ``__future__`` PATCH (``{"type", "value"}``, the value as text)."""
    kind, text = value.get("type"), value.get("value")
    if kind == "integer":
        return int(text)
    if kind == "boolean":
        return text == "true"
    return text


def registry_features(registry: Path) -> list[tuple[str, str]]:
    """(flag set, key) of every registry row, the features the link syncs (FR-214)."""
    import yaml

    doc = yaml.safe_load(registry.read_text())
    rows = doc["flags"] if isinstance(doc, dict) and "flags" in doc else doc
    return [(row["set"], row["key"]) for row in rows]


class FakeFlagsmith:
    def __init__(self):
        self.port = free_port()
        self._lock = threading.Lock()
        self._requests: list[dict] = []
        self._health: list[dict] = []
        self._server: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._next_id = 1000
        self._refusals: list[dict] = []
        self._updated = dt.datetime.now(dt.UTC)
        self.down = False
        self.delay = 0.0
        self.versioning = False
        self.identity_overrides_in_local_eval = True
        self.audit_status = 200
        self.audit_rows: list[dict] = []
        self.features: list[dict] = []
        self.tags: list[dict] = []
        # feature id -> {"enabled": bool, "value": object}
        self._env: dict[int, dict] = {}
        # identifier -> {"identity_uuid", "traits": {k: v}, "overrides": {feature id: {"enabled", "value"}}}
        self._identities: dict[str, dict] = {}

    # -------------------------------------------------------------- addresses
    @property
    def base_url(self) -> str:
        """The API base the link dials (``FLAGSMITH_API_URL`` and ``FLAGSMITH_DOCUMENT_URL``)."""
        return f"http://127.0.0.1:{self.port}/api/v1/"

    def flag_service_env(self) -> dict[str, str]:
        """The FLAGSMITH_* env of a flag service linked to this fake (services.md §7.3 Config)."""
        return {
            "FLAGSMITH_LINK": "on",
            "FLAGSMITH_API_URL": self.base_url,
            "FLAGSMITH_DOCUMENT_URL": self.base_url,
            "FLAGSMITH_SERVER_KEY": SERVER_KEY,
            "FLAGSMITH_ADMIN_API_KEY": ADMIN_KEY,
            "FLAGSMITH_ENVIRONMENT_KEY": ENVIRONMENT_KEY,
            "FLAGSMITH_PROJECT_ID": str(PROJECT_ID),
            "FLAGSMITH_POLL_SECONDS": "5",
            "FLAGSMITH_TIMEOUT_SECONDS": "1",
            "FLAGSMITH_HEALTH_WEBHOOK_URL": f"http://127.0.0.1:{self.port}{HEALTH_PATH}",
        }

    # -------------------------------------------------------------- lifecycle
    def start(self) -> int:
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):  # noqa: ARG002
                pass

            def _handle(self):
                length = int(self.headers.get("Content-Length", 0) or 0)
                raw = self.rfile.read(length) if length else b""
                try:
                    body = json.loads(raw) if raw else None
                except ValueError:
                    body = raw.decode(errors="replace")
                if fake.delay > 0:
                    time.sleep(fake.delay)
                status, payload, extra = fake._answer(self.command, self.path, dict(self.headers.items()), body)
                data = b"" if payload is None or status == 304 else json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                for name, value in extra.items():
                    self.send_header(name, value)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _handle  # noqa: N815 (http.server names)

        self._server = ThreadingHTTPServer(("127.0.0.1", self.port), Handler)
        self._server.daemon_threads = True
        self._thread = threading.Thread(target=self._server.serve_forever, name="fake-flagsmith", daemon=True)
        self._thread.start()
        return self.port

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None

    def log_tail(self) -> str:
        with self._lock:
            tail = self._requests[-20:]
            return f"fake-flagsmith: {len(self._requests)} request(s); last: " + ", ".join(
                f"{r['method']} {r['path']} -> {r['status']}" for r in tail
            )

    # ---------------------------------------------------------------- control
    def requests(self, method: str | None = None, path_contains: str = "") -> list[dict]:
        """The calls so far, oldest first (``t``: the monotonic time of the answer)."""
        with self._lock:
            return [
                dict(r)
                for r in self._requests
                if (method is None or r["method"] == method) and path_contains in r["path"]
            ]

    def created_features(self) -> list[str]:
        return [r["body"]["name"] for r in self.requests("POST", "/features/") if r["status"] == 201]

    def preseed(self, features: list[tuple[str, str]]) -> None:
        """Create the features and their flag-set tags before the flag service starts, as an
        earlier start would have (no ~30 s of creations at FLAGSMITH_PUSH_RATE)."""
        with self._lock:
            tags: dict[str, int] = {t["label"]: t["id"] for t in self.tags}
            names = {f["name"] for f in self.features}
            for flag_set, key in features:
                if flag_set not in tags:
                    self._next_id += 1
                    tags[flag_set] = self._next_id
                    self.tags.append({"id": self._next_id, "label": flag_set})
                if key not in names:
                    self._new_feature(key, [tags[flag_set]])
                    names.add(key)

    def feature_id(self, feature: str) -> int:
        with self._lock:
            return self._feature(feature)["id"]

    def env_value(self, feature: str) -> tuple[bool, object]:
        """(enabled, value) of the feature's environment value."""
        with self._lock:
            state = self._env[self._feature(feature)["id"]]
            return state["enabled"], state["value"]

    def override(self, identifier: str, feature: str) -> dict | None:
        """The override of ``feature`` on the identity (``{"enabled", "value"}``), None when none."""
        with self._lock:
            identity = self._identities.get(identifier)
            if identity is None:
                return None
            found = identity["overrides"].get(self._feature(feature)["id"])
            return dict(found) if found else None

    def identities(self) -> dict[str, dict]:
        """identifier -> {"identity_uuid", "traits"} of every edge identity."""
        with self._lock:
            return {
                k: {"identity_uuid": v["identity_uuid"], "traits": dict(v["traits"])}
                for k, v in self._identities.items()
            }

    def pushes(self, feature: str) -> list[dict]:
        """The link's value requests for one feature (stack PATCH and identity overrides), oldest first."""
        fid = self.feature_id(feature)
        with self._lock:
            return [dict(r) for r in self._requests if r.get("feature") == fid]

    def health_posts(self) -> list[dict]:
        """The feature health webhook bodies, oldest first."""
        with self._lock:
            return [dict(p) for p in self._health]

    def refuse(
        self,
        feature: str,
        status: int = 409,
        body: dict | None = None,
        retry_after: float | None = None,
        times: int | None = None,
    ) -> None:
        """Answer the link's value requests for ``feature`` with ``status`` (``times`` requests,
        None = until ``clear_refusals``). 409 defaults to Flagsmith's change-request answer."""
        if body is None:
            body = {"detail": "change_requests_enabled"} if status == 409 else {"detail": "refused"}
        fid = self.feature_id(feature)
        with self._lock:
            self._refusals.append(
                {"feature": fid, "status": status, "body": body, "retry_after": retry_after, "times": times}
            )

    def clear_refusals(self) -> None:
        with self._lock:
            self._refusals = []

    def person_sets(self, feature: str, value: object, enabled: bool = True) -> None:
        """A person changes the feature's environment value in the Flagsmith UI."""
        self.set_value(feature, value, enabled=enabled, author=PERSON)

    def person_overrides(self, identifier: str, feature: str, value: object, enabled: bool = True) -> None:
        """A person sets (value not None) or removes (None) an identity override in the UI."""
        self.set_override(identifier, feature, value, enabled=enabled, author=PERSON)

    def set_value(self, feature: str, value: object, enabled: bool = True, author: dict | None = None) -> None:
        """Set an environment value; ``author`` None writes it as an API key would."""
        with self._lock:
            f = self._feature(feature)
            self._env[f["id"]] = {"enabled": enabled, "value": value}
            self._audit(f"Flag state updated for feature: {f['name']}", author)
            self._bump()

    def set_override(
        self, identifier: str, feature: str, value: object, enabled: bool = True, author: dict | None = None
    ) -> None:
        """Set (value not None) or remove (None) an override; ``author`` None = an API key."""
        with self._lock:
            f = self._feature(feature)
            identity = self._identity(identifier, create=value is not None)
            if value is None:
                if identity is None or identity["overrides"].pop(f["id"], None) is None:
                    return
                verb = "deleted"
            else:
                verb = "updated" if f["id"] in identity["overrides"] else "created"
                identity["overrides"][f["id"]] = {"enabled": enabled, "value": value}
            self._audit(f"Feature override {verb} for feature '{f['name']}' and identity '{identifier}'", author)
            self._bump()

    def add_identity(self, identifier: str, traits: dict | None = None) -> str:
        """An identity that exists in Flagsmith already (e.g. from an earlier run), with ``traits``;
        returns its uuid."""
        with self._lock:
            identity = self._identity(identifier, create=True)
            identity["traits"].update(traits or {})
            return identity["identity_uuid"]

    def reset_values(self) -> None:
        """Back to a fresh environment: every value off, no identities, overrides, audit rows,
        refusals, health posts or knobs; the features and tags stay."""
        with self._lock:
            for state in self._env.values():
                state["enabled"], state["value"] = False, None
            self._identities.clear()
            self.audit_rows = []
            self._refusals = []
            self._health = []
            self.down = False
            self.delay = 0.0
            self.versioning = False
            self.identity_overrides_in_local_eval = True
            self.audit_status = 200
            self._bump()

    # ------------------------------------------------------------ state (lock)
    def _feature(self, name_or_id: str | int) -> dict:
        for f in self.features:
            if f["name"] == name_or_id or f["id"] == name_or_id:
                return f
        raise KeyError(f"fake flagsmith has no feature {name_or_id!r}")

    def _new_feature(self, name: str, tags: list[int]) -> dict:
        self._next_id += 1
        feature = {"id": self._next_id, "name": name, "type": "STANDARD", "is_archived": False, "tags": tags}
        self.features.append(feature)
        self._env[feature["id"]] = {"enabled": False, "value": None}
        self._bump()
        return feature

    def _identity(self, identifier: str, create: bool) -> dict | None:
        identity = self._identities.get(identifier)
        if identity is None and create:
            identity = {"identity_uuid": str(uuid.uuid4()), "traits": {}, "overrides": {}}
            self._identities[identifier] = identity
        return identity

    def _audit(self, log: str, author: dict | None) -> None:
        self.audit_rows.insert(0, {"log": log, "author": author})

    def _bump(self) -> None:
        now = dt.datetime.now(dt.UTC)
        if now <= self._updated:
            now = self._updated + dt.timedelta(microseconds=1)
        self._updated = now

    def _state(self, feature: dict, state: dict) -> dict:
        return {
            "feature": {"id": feature["id"], "name": feature["name"], "type": "STANDARD"},
            "enabled": state["enabled"],
            "feature_segment": None,
            "feature_state_value": state["value"],
        }

    def _document(self) -> dict:
        overrides = []
        for identifier, identity in sorted(self._identities.items()):
            if identity["overrides"]:
                overrides.append(
                    {
                        "identifier": identifier,
                        "identity_uuid": identity["identity_uuid"],
                        "identity_features": [
                            self._state(self._feature(fid), state) for fid, state in identity["overrides"].items()
                        ],
                    }
                )
        return {
            "name": ENVIRONMENT_NAME,
            "updated_at": _iso(self._updated),
            "use_identity_overrides_in_local_eval": self.identity_overrides_in_local_eval,
            "feature_states": [self._state(f, self._env[f["id"]]) for f in self.features],
            "identity_overrides": overrides,
        }

    def _refusal(self, fid: int) -> dict | None:
        for r in self._refusals:
            if r["feature"] == fid:
                if r["times"] is not None:
                    r["times"] -= 1
                    if r["times"] <= 0:
                        self._refusals.remove(r)
                return r
        return None

    # ----------------------------------------------------------------- server
    def _answer(self, method: str, raw_path: str, headers: dict, body) -> tuple[int, object, dict]:
        url = urllib.parse.urlsplit(raw_path)
        path, query = url.path, urllib.parse.parse_qs(url.query)
        lower = {k.lower(): v for k, v in headers.items()}
        with self._lock:
            record = {"method": method, "path": path, "query": url.query, "body": body}
            status, payload, extra = self._route(method, path, query, lower, body, record)
            record.update(status=status, t=time.monotonic())
            self._requests.append(record)
        return status, payload, extra

    def _route(self, method, path, query, headers, body, record):
        if self.down:
            return 503, {"detail": "fake flagsmith down"}, {}
        if path == HEALTH_PATH and method == "POST":
            self._health.append(body)
            return 200, {}, {}
        if path == "/api/v1/environment-document/":
            if headers.get("x-environment-key") != SERVER_KEY:
                return 401, {"detail": "Invalid or missing Environment Key"}, {}
            return self._document_answer(headers)
        if headers.get("authorization") != f"Api-Key {ADMIN_KEY}":
            return 401, {"detail": "Authentication credentials were not provided."}, {}
        if (m := _ANY_ENV.match(path)) and m.group(1) != ENVIRONMENT_KEY:
            return 404, {"detail": "Not found."}, {}
        if (m := _ANY_PROJECT.match(path)) and m.group(1) != str(PROJECT_ID):
            return 404, {"detail": "Not found."}, {}
        if (m := _FUTURE.match(path)) and method == "PATCH":
            return self._patch(int(m.group(2)), body, record)
        if _EDGE_FS.match(path) and method in ("PUT", "DELETE"):
            return self._override_push(method, body or {}, record)
        if (m := _ENV.match(path)) and method == "GET":
            return (
                200,
                {
                    "id": ENVIRONMENT_ID,
                    "name": ENVIRONMENT_NAME,
                    "api_key": m.group(1),
                    "project": PROJECT_ID,
                    "use_v2_feature_versioning": self.versioning,
                    "use_identity_overrides_in_local_eval": self.identity_overrides_in_local_eval,
                },
                {},
            )
        if _EDGE_LIST.match(path):
            if method == "POST":
                return self._create_identity((body or {}).get("identifier", ""))
            if method == "GET":
                return self._list_identities(query)
        if (m := _EDGE_TRAITS.match(path)) and method == "PUT":
            for identity in self._identities.values():
                if identity["identity_uuid"] == m.group(2):
                    if body["trait_value"] is None:
                        # Flagsmith deletes a trait set to null.
                        identity["traits"].pop(body["trait_key"], None)
                    else:
                        identity["traits"][body["trait_key"]] = body["trait_value"]
                    return 200, {"trait_key": body["trait_key"], "trait_value": body["trait_value"]}, {}
            return 404, {"detail": "Not found."}, {}
        if (m := _EDGE_SUB.match(path)) and method == "GET":
            identity = next((i for i in self._identities.values() if i["identity_uuid"] == m.group(2)), None)
            if identity is None:
                return 404, {"detail": "Not found."}, {}
            if m.group(3) == "list-traits":
                return 200, [{"trait_key": k, "trait_value": v} for k, v in identity["traits"].items()], {}
            return (
                200,
                [
                    {
                        "feature": fid,
                        "enabled": state["enabled"],
                        "feature_state_value": state["value"],
                        "identity_uuid": identity["identity_uuid"],
                    }
                    for fid, state in identity["overrides"].items()
                ],
                {},
            )
        if (m := _EDGE_ONE.match(path)) and method == "DELETE":
            for identifier, identity in list(self._identities.items()):
                if identity["identity_uuid"] == m.group(2):
                    del self._identities[identifier]
                    if identity["overrides"]:
                        self._bump()
                    return 204, None, {}
            return 404, {"detail": "Not found."}, {}
        if m := _PROJECT.match(path):
            return self._project(method, m.group(2), query, body)
        return 404, {"detail": f"fake flagsmith: no route {method} {path}"}, {}

    def _document_answer(self, headers):
        floor = self._updated.replace(microsecond=0)
        ims = headers.get("if-modified-since")
        if ims:
            try:
                since = email.utils.parsedate_to_datetime(ims)
            except (TypeError, ValueError):
                since = None
            if since is not None and since >= floor:
                return 304, None, {"Last-Modified": email.utils.format_datetime(floor, usegmt=True)}
        return (
            200,
            self._document(),
            {
                "Last-Modified": email.utils.format_datetime(floor, usegmt=True),
                "X-Flagsmith-Document-Updated-At": f"{self._updated.timestamp():.6f}",
            },
        )

    def _refused(self, fid: int):
        r = self._refusal(fid)
        if r is None:
            return None
        extra = {"Retry-After": str(int(r["retry_after"]))} if r["retry_after"] is not None else {}
        return r["status"], r["body"], extra

    def _patch(self, fid: int, body, record):
        record["feature"] = fid
        try:
            feature = self._feature(fid)
        except KeyError:
            return 404, {"detail": "Not found."}, {}
        if refused := self._refused(fid):
            return refused
        default = body["environment_default"]
        self._env[fid] = {"enabled": bool(default["enabled"]), "value": _typed(default["value"])}
        self._audit(f"Flag state updated for feature: {feature['name']}", None)
        self._bump()
        return 200, {"feature": fid, **default}, {}

    def _override_push(self, method, body, record):
        fid, identifier = body.get("feature"), body.get("identifier", "")
        record["feature"] = fid
        try:
            feature = self._feature(fid)
        except KeyError:
            return 404, {"detail": "Not found."}, {}
        if refused := self._refused(fid):
            return refused
        if method == "DELETE":
            identity = self._identities.get(identifier)
            if identity is None or identity["overrides"].pop(fid, None) is None:
                return 404, {"detail": "Not found."}, {}
            verb = "deleted"
        else:
            identity = self._identity(identifier, create=True)
            verb = "updated" if fid in identity["overrides"] else "created"
            identity["overrides"][fid] = {
                "enabled": bool(body.get("enabled")),
                "value": body.get("feature_state_value"),
            }
        self._audit(f"Feature override {verb} for feature '{feature['name']}' and identity '{identifier}'", None)
        self._bump()
        return (204, None, {}) if method == "DELETE" else (200, dict(body), {})

    def _create_identity(self, identifier: str):
        if identifier in self._identities:
            return 400, {"identifier": [f"Identity with identifier '{identifier}' already exists."]}, {}
        identity = self._identity(identifier, create=True)
        return 201, {"identifier": identifier, "identity_uuid": identity["identity_uuid"]}, {}

    def _list_identities(self, query):
        q = (query.get("q") or [""])[0]
        if len(q) >= 2 and q.startswith('"') and q.endswith('"'):
            rows = [k for k in self._identities if k == q[1:-1]]
        else:
            rows = [k for k in self._identities if k.startswith(q)]
        size = int((query.get("page_size") or ["100"])[0])
        results = [
            {
                "identifier": k,
                "identity_uuid": self._identities[k]["identity_uuid"],
                "environment_api_key": ENVIRONMENT_KEY,
            }
            for k in sorted(rows)[:size]
        ]
        return 200, {"results": results, "last_evaluated_key": None}, {}

    def _project(self, method, kind, query, body):
        page = int((query.get("page") or ["1"])[0])
        if kind == "features":
            if method == "POST":
                if any(f["name"].lower() == body["name"].lower() for f in self.features):
                    return 400, {"name": ["Feature with that name already exists for this project."]}, {}
                feature = self._new_feature(body["name"], list(body.get("tags") or []))
                return 201, feature, {}
            return 200, {"count": len(self.features), "next": None, "previous": None, "results": self.features}, {}
        if kind == "tags":
            if method == "POST":
                self._next_id += 1
                tag = {"id": self._next_id, "label": body["label"]}
                self.tags.append(tag)
                return 201, tag, {}
            return 200, {"count": len(self.tags), "next": None, "previous": None, "results": self.tags}, {}
        if self.audit_status != 200:
            return self.audit_status, {"detail": "audit"}, {}
        if (query.get("environments") or [""])[0] != str(ENVIRONMENT_ID):
            # The link reads its own environment's rows only (audit.go): a missing or wrong filter is a bug.
            return 400, {"environments": ["the fake expects environments=<the environment id>"]}, {}
        rows = self.audit_rows
        search = (query.get("search") or [""])[0].lower()
        if search:
            rows = [r for r in rows if search in r["log"].lower()]
        size = int((query.get("page_size") or ["10"])[0])
        chunk = rows[(page - 1) * size : page * size]
        more = page * size < len(rows)
        results = [{"is_system_event": False, **r} for r in chunk]
        return 200, {"count": len(rows), "next": "more" if more else None, "previous": None, "results": results}, {}
