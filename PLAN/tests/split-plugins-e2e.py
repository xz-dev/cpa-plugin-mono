#!/usr/bin/env python3
import hashlib
import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
state = {
    "revision": 1,
    "models": [
        {
            "name": "keep",
            "alias": "custom",
            "display_name": "Keep",
            "max_context_length": 128000,
            "thinking": {"levels": ["low"]},
            "input_modalities": ["text"],
            "output_modalities": ["text"],
        },
        {"name": "remove", "alias": "old"},
    ],
}


def revision():
    return hashlib.sha256(json.dumps(state, sort_keys=True).encode()).hexdigest()


def descriptor():
    return {
        "kind": "openai-compatibility",
        "selector": {"name": "Demo", "base_url": "https://upstream.example/v1"},
        "disabled": False,
        "ready": True,
        "revision": revision(),
        "models": state["models"],
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, value):
        raw = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def request_json(self):
        return json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")

    def authorized(self):
        if self.headers.get("Authorization") != "Bearer management-key":
            self.reply(401, {"error": "unauthorized"})
            return False
        return True

    def do_GET(self):
        if not self.authorized():
            return
        if self.path == "/v0/management/model-channels":
            self.reply(200, {"channels": [descriptor()]})
        else:
            self.reply(404, {"error": "not found"})

    def do_POST(self):
        if not self.authorized():
            return
        body = self.request_json()
        if body.get("expected_revision") != revision():
            self.reply(409, {"error": "channel revision drifted"})
            return
        if self.path == "/v0/management/model-channels/catalog":
            catalog = {"models": [{"slug": "keep", "max_tokens": 64000}, {"slug": "new"}]}
            self.reply(200, {"status_code": 200, "body": json.dumps(catalog, separators=(",", ":"))})
            return
        if self.path == "/v0/management/model-channels/reconcile-membership":
            if body.get("expected_model_names") != [model["name"] for model in state["models"]]:
                self.reply(409, {"error": "model set drifted"})
                return
            old = {model["name"]: model for model in state["models"]}
            state["models"] = [old.get(name, {"name": name, "alias": name}) for name in body["desired_model_names"]]
            state["revision"] += 1
            self.reply(200, {"status": "ok", "revision": revision()})
            return
        self.reply(404, {"error": "not found"})

    def do_PATCH(self):
        if not self.authorized():
            return
        body = self.request_json()
        if body.get("expected_revision") != revision() or body.get("expected_model_names") != [model["name"] for model in state["models"]]:
            self.reply(409, {"error": "channel revision or model-set precondition drifted"})
            return
        by_name = {model["name"]: model for model in state["models"]}
        key_map = {
            "thinking.levels": "thinking",
            "max-context-length": "max_context_length",
            "max-input-tokens": "max_input_tokens",
            "max-output-tokens": "max_output_tokens",
            "input-modalities": "input_modalities",
            "output-modalities": "output_modalities",
        }
        for operation in body["operations"]:
            model = by_name[operation["model"]]
            for field, patch in operation["fields"].items():
                key = key_map[field]
                if patch["mode"] == "if-empty" and model.get(key):
                    continue
                model[key] = {"levels": patch["value"]} if field == "thinking.levels" else patch["value"]
        state["revision"] += 1
        self.reply(200, {"status": "ok", "revision": revision()})


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
env = dict(os.environ, SPLIT_E2E_URL=f"http://127.0.0.1:{server.server_port}")
cache_root = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "pi-agent" / "cpa-plugin-mono-split-e2e"
cache_root.mkdir(parents=True, exist_ok=True)
env["GOCACHE"] = str(cache_root / "go-build")
env["GOMODCACHE"] = str(cache_root / "go-mod")
env.pop("GOROOT", None)
commands = [
    ["go", "test", "./internal/plugin", "-run", "TestSplitE2EMetadataBeforeMembership", "-count=1"],
    ["go", "test", "./internal/plugin", "-run", "TestSplitE2EMembershipReconcileAndStaleRevision", "-count=1"],
    ["go", "test", "./internal/plugin", "-run", "TestSplitE2EMetadataBeforeMembership", "-count=1"],
]
modules = ["model-metadata-sync", "auto-pull-models", "model-metadata-sync"]
try:
    for index, (module, command) in enumerate(zip(modules, commands)):
        print("+", module, " ".join(command), flush=True)
        command_env = dict(env)
        if index == 2:
            command_env["SPLIT_E2E_AFTER_MEMBERSHIP"] = "1"
        subprocess.run(command, cwd=ROOT / module, env=command_env, check=True)
finally:
    server.shutdown()

models = state["models"]
assert [model["name"] for model in models] == ["keep", "new"], models
assert models[0]["alias"] == "custom", models
assert models[0]["max_output_tokens"] == 64000, models
print("PASS: metadata retained after membership; stale revision rejected; second metadata cycle idempotent")
