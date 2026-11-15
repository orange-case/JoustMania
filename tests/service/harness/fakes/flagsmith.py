"""FakeFlagsmith: a recording hosted-Flagsmith stub, one per stack (#2591, UC-054, UC-055).

The flag service's Flagsmith link (``services/flag-service/flagsmith``) is pointed here
with ``FLAGSMITH_API_URL`` and ``FLAGSMITH_DOCUMENT_URL`` (``flag_service_env()``), so
the service tier never reaches flagsmith.com. It serves the
endpoints the link uses (architecture note flagsmith-sync.md §5.0, §5.1):

- ``GET  /api/v1/environment-document/`` (``X-Environment-Key``: the server key)
- ``GET  /api/v1/environments/<key>/``, ``.../edge-identities/`` (``q``, ``last_evaluated_key``),
  ``DELETE .../edge-identities/<uuid>/``
- ``GET|POST /api/v1/projects/<id>/features/``, ``.../tags/``; ``GET .../audit/``
  (newest first, ``search`` = log contains, paged by ``page`` / ``page_size``)
- every other request (the out side's pushes: ``/api/__future__/...`` PATCH, edge identity
  feature states) answers 200 ``{}`` and is recorded.

Admin calls need ``Authorization: Api-Key <ADMIN_KEY>`` (401 otherwise), as in Flagsmith.
Knobs: ``down`` (every request 503), ``audit_rows`` (dicts with ``log`` and ``author``,
``None`` = an API key), ``audit_status``, ``versioning`` (v2), ``identity_overrides_in_local_eval``,
``features`` / ``tags`` (existing), ``document`` (feature_states, identity_overrides).

An in-process threaded server (stdlib only, instance state), like fakes/github.py.
"""

from __future__ import annotations

import json
import re
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from ..processes import free_port

# Dummy credentials the flag service is given; never real ones.
SERVER_KEY = "ser.fake-flagsmith-service-tier"
ADMIN_KEY = "fake-flagsmith-admin-key"
ENVIRONMENT_KEY = "fakeEnvKey123"
PROJECT_ID = 4242
ENVIRONMENT_ID = 77
ENVIRONMENT_NAME = "service-tier"

_ENV = re.compile(r"^/api/v1/environments/([^/]+)/$")
_EDGE_LIST = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities/$")
_EDGE_ONE = re.compile(r"^/api/v1/environments/([^/]+)/edge-identities/([^/]+)/$")
_PROJECT = re.compile(r"^/api/v1/projects/(\d+)/(features|tags|audit)/$")


class FakeFlagsmith:
    def __init__(self):
        self.port = free_port()
        self._lock = threading.Lock()
        self._requests: list[dict] = []
        self._server: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._next_id = 1000
        self.down = False
        self.versioning = False
        self.identity_overrides_in_local_eval = True
        self.audit_status = 200
        self.audit_rows: list[dict] = []
        self.features: list[dict] = []
        self.tags: list[dict] = []
        self.identities: list[dict] = []
        self.feature_states: list[dict] = []
        self.identity_overrides: list[dict] = []

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
                status, payload = fake._answer(self.command, self.path, dict(self.headers.items()), body)
                data = b"" if payload is None else json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
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
        """The calls so far, oldest first."""
        with self._lock:
            return [
                dict(r)
                for r in self._requests
                if (method is None or r["method"] == method) and path_contains in r["path"]
            ]

    def created_features(self) -> list[str]:
        return [r["body"]["name"] for r in self.requests("POST", "/features/") if r["status"] == 201]

    def set_document(self, feature_states: list[dict], identity_overrides: list[dict] | None = None) -> None:
        with self._lock:
            self.feature_states = list(feature_states)
            self.identity_overrides = list(identity_overrides or [])

    # ----------------------------------------------------------------- server
    def _answer(self, method: str, raw_path: str, headers: dict, body) -> tuple[int, dict | list | None]:
        url = urllib.parse.urlsplit(raw_path)
        path, query = url.path, urllib.parse.parse_qs(url.query)
        lower = {k.lower(): v for k, v in headers.items()}
        status, payload = self._route(method, path, query, lower, body)
        with self._lock:
            self._requests.append({"method": method, "path": path, "query": url.query, "body": body, "status": status})
        return status, payload

    def _route(self, method, path, query, headers, body):
        with self._lock:
            if self.down:
                return 503, {"detail": "fake flagsmith down"}
            if path == "/api/v1/environment-document/":
                if headers.get("x-environment-key") != SERVER_KEY:
                    return 401, {"detail": "Invalid or missing Environment Key"}
                return 200, {
                    "name": ENVIRONMENT_NAME,
                    "use_identity_overrides_in_local_eval": self.identity_overrides_in_local_eval,
                    "feature_states": self.feature_states,
                    "identity_overrides": self.identity_overrides,
                }
            if headers.get("authorization") != f"Api-Key {ADMIN_KEY}":
                return 401, {"detail": "Authentication credentials were not provided."}
            if (m := _ENV.match(path)) and method == "GET":
                return 200, {
                    "id": ENVIRONMENT_ID,
                    "name": ENVIRONMENT_NAME,
                    "api_key": m.group(1),
                    "project": PROJECT_ID,
                    "use_v2_feature_versioning": self.versioning,
                    "use_identity_overrides_in_local_eval": self.identity_overrides_in_local_eval,
                }
            if _EDGE_LIST.match(path) and method == "GET":
                prefix = (query.get("q") or [""])[0]
                rows = [i for i in self.identities if i["identifier"].startswith(prefix)]
                return 200, {"results": rows, "last_evaluated_key": None}
            if (m := _EDGE_ONE.match(path)) and method == "DELETE":
                self.identities = [i for i in self.identities if i["identity_uuid"] != m.group(2)]
                return 204, None
            if m := _PROJECT.match(path):
                return self._project(method, m.group(2), query, body)
            return 200, {}

    def _project(self, method, kind, query, body):
        page = int((query.get("page") or ["1"])[0])
        if kind == "features":
            if method == "POST":
                if any(f["name"].lower() == body["name"].lower() for f in self.features):
                    return 400, {"name": ["Feature with that name already exists for this project."]}
                self._next_id += 1
                feature = {"id": self._next_id, "name": body["name"], "is_archived": False}
                self.features.append(feature)
                return 201, feature
            return 200, {"count": len(self.features), "next": None, "previous": None, "results": self.features}
        if kind == "tags":
            if method == "POST":
                self._next_id += 1
                tag = {"id": self._next_id, "label": body["label"]}
                self.tags.append(tag)
                return 201, tag
            return 200, {"count": len(self.tags), "next": None, "previous": None, "results": self.tags}
        if self.audit_status != 200:
            return self.audit_status, {"detail": "audit"}
        rows = self.audit_rows
        search = (query.get("search") or [""])[0].lower()
        if search:
            rows = [r for r in rows if search in r["log"].lower()]
        size = int((query.get("page_size") or ["10"])[0])
        chunk = rows[(page - 1) * size : page * size]
        more = page * size < len(rows)
        results = [{"is_system_event": False, **r} for r in chunk]
        return 200, {"count": len(rows), "next": "more" if more else None, "previous": None, "results": results}
