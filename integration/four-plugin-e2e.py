#!/usr/bin/env python3
"""Selected-Core, four-plugin integration test. Standard library only."""

from __future__ import annotations

import hashlib
import http.server
import json
import os
import re
import secrets
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[1]
PLUGIN_IDS = (
    "auto-pull-models",
    "model-metadata-sync",
    "model-info",
    "sync-config-write",
)
BASE = "1f53b2eb03b9e963bac647e5566ca2b304239116"
PATCHES = (
    ("908c46d2dfdf542805290740536e8e56669e80b2", "f26c0952fc1ecfa6aeb97032b9bb3df65dbb928b"),
    ("e469ef0361e6329a2d4a3dc3798380603e7e7540", "18db34cea8a8cb49856d358b6150c13539187b21"),
    ("eea510a8646045a6637c0d5e6433965627c5fa86", "eab132f3d8cd00a18e00521ff9c913120d0afcef"),
    ("fd70b12a4ea1c2c7957d479109d4df0c5f5c55ca", "a3c736f0c1142c1fa1d51b9e56a99ba5f83074a2"),
    ("b2b2d61db3b835363b881b0baa918b8357aa9da6", "48df055b241b50c081958cc11a4daf5e9e62a2fa"),
)
EXCLUDED = "07cb171df083d128120f9847debe512b8228807b"
ACTIVE_STATES = {"queued", "planning", "fetching", "committing", "waiting_reconfigure", "reconciling"}
OWNED_MODEL_KEYS = {
    "thinking",
    "max-context-length",
    "max-input-tokens",
    "max-output-tokens",
    "input-modalities",
    "output-modalities",
}


def phase(name: str) -> None:
    print(f"PASS {name}", flush=True)


def fail(message: str) -> None:
    raise RuntimeError(message)


def exit_on_signal(signum: int, _frame: Any) -> None:
    raise SystemExit(128 + signum)


def stop_process_group(process: subprocess.Popen[bytes], grace: float = 5) -> None:
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait(timeout=5)
    else:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def run(
    args: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    input_bytes: bytes | None = None,
) -> bytes:
    process = subprocess.Popen(
        args,
        cwd=cwd,
        env=env,
        stdin=subprocess.PIPE if input_bytes is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        stdout, stderr = process.communicate(input=input_bytes, timeout=1200)
    except subprocess.TimeoutExpired:
        stop_process_group(process)
        fail(f"command timed out: {args[0]} {args[1] if len(args) > 1 else ''}")
    except BaseException:
        stop_process_group(process)
        raise
    if process.returncode != 0:
        diagnostic = (stderr or stdout).decode(errors="replace")
        diagnostic = " | ".join(line.strip() for line in diagnostic.splitlines()[-5:] if line.strip())[:1000]
        fail(f"command failed: {args[0]} {args[1] if len(args) > 1 else ''}: {diagnostic}")
    return stdout


def git(repo: Path, *args: str, env: dict[str, str] | None = None, input_bytes: bytes | None = None) -> bytes:
    return run(["git", "-C", str(repo), *args], env=env, input_bytes=input_bytes)


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def quote(value: str | Path) -> str:
    return json.dumps(str(value))


def compose_core(source: Path, temp: Path) -> tuple[Path, str]:
    source = source.resolve()
    if not (source / ".git").exists():
        fail("CPA_CORE_SOURCE is not a Git checkout")
    if git(source, "rev-parse", "v7.2.142^{}").decode().strip() != BASE:
        fail("v7.2.142 does not resolve to fixed base")
    for commit, _ in PATCHES:
        if git(source, "cat-file", "-t", commit).decode().strip() != "commit":
            fail("required patch object unavailable")

    def source_state() -> tuple[bytes, bytes, bytes, bytes]:
        return (
            git(source, "rev-parse", "HEAD"),
            git(source, "status", "--porcelain=v1", "--untracked-files=no"),
            git(source, "for-each-ref", "--format=%(refname)%00%(objectname)"),
            git(source, "count-objects", "-v"),
        )

    before_source_state = source_state()
    worktree = temp / "core"
    run(["git", "clone", "--quiet", "--no-hardlinks", "--no-checkout", str(source), str(worktree)])
    alternates = worktree / ".git" / "objects" / "info" / "alternates"
    if alternates.exists() and alternates.read_text(encoding="utf-8").strip():
        fail("disposable Core clone depends on sibling object storage")

    direct_head = PATCHES[1][0]
    git(worktree, "checkout", "--quiet", "--detach", direct_head)
    direct_commits = git(worktree, "rev-list", "--reverse", f"{BASE}..HEAD").decode().splitlines()
    if direct_commits != [PATCHES[0][0], PATCHES[1][0]]:
        fail("selected Core direct ancestry mismatch")

    for commit, _ in PATCHES[2:]:
        commit_env = os.environ.copy()
        commit_env.update(
            {
                "GIT_AUTHOR_NAME": git(source, "show", "-s", "--format=%an", commit).decode().strip(),
                "GIT_AUTHOR_EMAIL": git(source, "show", "-s", "--format=%ae", commit).decode().strip(),
                "GIT_AUTHOR_DATE": git(source, "show", "-s", "--format=%aI", commit).decode().strip(),
                "GIT_COMMITTER_NAME": git(source, "show", "-s", "--format=%cn", commit).decode().strip(),
                "GIT_COMMITTER_EMAIL": git(source, "show", "-s", "--format=%ce", commit).decode().strip(),
                "GIT_COMMITTER_DATE": git(source, "show", "-s", "--format=%cI", commit).decode().strip(),
            }
        )
        git(worktree, "cherry-pick", "--no-gpg-sign", commit, env=commit_env)

    applied_commits = git(worktree, "rev-list", "--reverse", f"{BASE}..HEAD").decode().splitlines()
    if len(applied_commits) != len(PATCHES) or applied_commits[:2] != [PATCHES[0][0], PATCHES[1][0]]:
        fail("selected Core patch count/direct ancestry mismatch")
    for (source_commit, expected_patch_id), applied_commit in zip(PATCHES, applied_commits, strict=True):
        source_patch = git(source, "show", "--pretty=format:", "--binary", source_commit)
        applied_patch = git(worktree, "show", "--pretty=format:", "--binary", applied_commit)
        source_patch_id = run(["git", "patch-id", "--stable"], input_bytes=source_patch).decode().split()[0]
        applied_patch_id = run(["git", "patch-id", "--stable"], input_bytes=applied_patch).decode().split()[0]
        if source_patch_id != expected_patch_id or applied_patch_id != expected_patch_id:
            fail("selected Core stable patch-id mismatch")

    if subprocess.run(
        ["git", "-C", str(worktree), "merge-base", "--is-ancestor", EXCLUDED, "HEAD"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    ).returncode == 0:
        fail("excluded model-channel Core commit is present")
    if git(worktree, "diff", "--check", f"{BASE}..HEAD").strip():
        fail("selected Core diff check failed")
    grep = subprocess.run(
        [
            "git",
            "-C",
            str(worktree),
            "grep",
            "-nE",
            r"model-channels|current-config-revision|expected_revision|expected-revision",
            "--",
            ":!go.sum",
            ":!go.mod",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if grep.returncode not in (0, 1) or grep.stdout:
        fail("excluded model-channel/revision/CAS surface is present")
    if source_state() != before_source_state:
        fail("CPA_CORE_SOURCE changed during disposable composition")
    return worktree, git(worktree, "rev-parse", "HEAD").decode().strip()


def build_environment(temp: Path) -> dict[str, str]:
    go_tmp = temp / "go-tmp"
    command_tmp = temp / "tmp"
    go_tmp.mkdir(exist_ok=True)
    command_tmp.mkdir(exist_ok=True)
    env = os.environ.copy()
    env.update(
        {
            "GOCACHE": str(temp / "go-build-cache"),
            "GOTMPDIR": str(go_tmp),
            "TMPDIR": str(command_tmp),
            "CGO_ENABLED": "1",
        }
    )
    return env


def build_all(core: Path, temp: Path) -> tuple[Path, dict[str, str]]:
    build_env = build_environment(temp)
    run(["go", "test", "./internal/config", "./internal/pluginhost", "./internal/watcher"], cwd=core, env=build_env)
    server = temp / "cliproxy"
    run(
        ["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", str(server), "./cmd/server"],
        cwd=core,
        env=build_env,
    )

    plugin_dir = temp / "plugins" / "linux" / "amd64"
    plugin_dir.mkdir(parents=True)
    hashes: dict[str, str] = {}
    for plugin_id in PLUGIN_IDS:
        artifact = plugin_dir / f"{plugin_id}-v0.1.0.so"
        duplicate = temp / f"{plugin_id}-v0.1.0.repro.so"
        command = [
            "go",
            "build",
            "-trimpath",
            "-buildvcs=false",
            "-buildmode=c-shared",
            "-ldflags=-buildid= -X main.pluginVersion=0.1.0",
        ]
        for output in (artifact, duplicate):
            run(command + ["-o", str(output), f"./cmd/{plugin_id}"], cwd=ROOT / plugin_id, env=build_env)
        first, second = file_sha256(artifact), file_sha256(duplicate)
        if first != second:
            fail(f"non-reproducible plugin build: {plugin_id}")
        hashes[plugin_id] = first
    hashes["core"] = file_sha256(server)
    return server, hashes


RUNTIME_HASH_HELPER = r'''package main

import (
    "bytes"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "os"

    "gopkg.in/yaml.v3"
)

func mappingValue(node *yaml.Node, key string) *yaml.Node {
    if node == nil || node.Kind != yaml.MappingNode {
        return nil
    }
    for index := 0; index+1 < len(node.Content); index += 2 {
        if node.Content[index].Value == key {
            return node.Content[index+1]
        }
    }
    return nil
}

func main() {
    if len(os.Args) < 3 {
        panic("usage: runtime-hash config plugin...")
    }
    raw, err := os.ReadFile(os.Args[1])
    if err != nil {
        panic(err)
    }
    var document yaml.Node
    if err = yaml.Unmarshal(raw, &document); err != nil || len(document.Content) != 1 {
        panic("invalid config")
    }
    configs := mappingValue(mappingValue(document.Content[0], "plugins"), "configs")
    result := make(map[string]string, len(os.Args)-2)
    for _, id := range os.Args[2:] {
        node := mappingValue(configs, id)
        if node == nil {
            panic(fmt.Sprintf("missing plugin %s", id))
        }
        encoded, errMarshal := yaml.Marshal(node)
        if errMarshal != nil {
            panic(errMarshal)
        }
        normalized := append(bytes.TrimSpace(encoded), '\n')
        sum := sha256.Sum256(normalized)
        result[id] = hex.EncodeToString(sum[:])
    }
    if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
        panic(err)
    }
}
'''


def independent_runtime_hashes(temp: Path, config_raw: bytes) -> dict[str, str]:
    helper = temp / "runtime-hash.go"
    input_path = temp / "runtime-hash-input.yaml"
    if not helper.exists():
        helper.write_text(RUNTIME_HASH_HELPER, encoding="utf-8")
    input_path.write_bytes(config_raw)
    raw = run(
        ["go", "run", str(helper), str(input_path), *PLUGIN_IDS],
        cwd=ROOT / "sync-config-write",
        env=build_environment(temp),
    )
    result = json.loads(raw)
    if set(result) != set(PLUGIN_IDS) or any(not re.fullmatch(r"[0-9a-f]{64}", value) for value in result.values()):
        fail("independent runtime hash reproduction failed")
    return result


def generate_certificates(temp: Path) -> tuple[Path, Path]:
    cert_dir = temp / "certs"
    cert_dir.mkdir()
    ca_key, ca_cert = cert_dir / "ca.key", cert_dir / "ca.pem"
    server_key, server_csr, server_cert = cert_dir / "server.key", cert_dir / "server.csr", cert_dir / "server.pem"
    ext = cert_dir / "server.ext"
    ext.write_text("subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n", encoding="utf-8")
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=CPA E2E CA", "-keyout", str(ca_key), "-out", str(ca_cert)])
    run(["openssl", "req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=127.0.0.1", "-keyout", str(server_key), "-out", str(server_csr)])
    run(["openssl", "x509", "-req", "-days", "1", "-in", str(server_csr), "-CA", str(ca_cert), "-CAkey", str(ca_key), "-CAcreateserial", "-extfile", str(ext), "-out", str(server_cert)])
    return ca_cert, server_cert


class ProviderState:
    def __init__(self, token: str) -> None:
        self.token = token
        self.lock = threading.Lock()
        self.counts: dict[str, int] = {}
        self.total = 0

    def record(self, path: str) -> bool:
        with self.lock:
            self.total += 1
            self.counts[path] = self.counts.get(path, 0) + 1
            return self.total <= 20

    def count(self, path: str) -> int:
        with self.lock:
            return self.counts.get(path, 0)


class ProviderFixture:
    def __init__(self, state: ProviderState, cert: Path, key: Path) -> None:
        fixture_state = state

        class Handler(http.server.BaseHTTPRequestHandler):
            server_version = "CPA-E2E"
            sys_version = ""

            def log_message(self, *_: Any) -> None:
                return

            def reply(self, status: int, payload: dict[str, Any]) -> None:
                raw = json.dumps(payload, separators=(",", ":")).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self) -> None:
                if not fixture_state.record(self.path):
                    self.reply(429, {"error": "request_bound_exceeded"})
                    return
                authorization = self.headers.get("Authorization", "")
                if not secrets.compare_digest(authorization, "Bearer " + fixture_state.token):
                    self.reply(401, {"error": "unauthorized"})
                    return
                if self.path == "/v1/models":
                    self.reply(200, {"data": [{"id": "keep"}, {"id": "new"}]})
                    return
                if self.path == "/v1/models?client_version=1.0.0":
                    self.reply(
                        200,
                        {
                            "models": [
                                {
                                    "slug": "keep",
                                    "context_window": 200001,
                                    "max_tokens": 20001,
                                    "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}],
                                    "input_modalities": ["text", "image"],
                                    "output_modalities": ["text"],
                                },
                                {
                                    "slug": "new",
                                    "context_window": 200002,
                                    "max_tokens": 20002,
                                    "supported_reasoning_levels": [{"effort": "medium"}, {"effort": "high"}],
                                    "input_modalities": ["text", "image"],
                                    "output_modalities": ["text"],
                                },
                            ]
                        },
                    )
                    return
                self.reply(404, {"error": "not_found"})

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(certfile=cert, keyfile=key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, name="cpa-e2e-provider", daemon=True)

    @property
    def port(self) -> int:
        return int(self.server.server_port)

    def start(self) -> None:
        self.thread.start()

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


class CoreClient:
    def __init__(self, port: int, management_key: str) -> None:
        self.origin = f"http://127.0.0.1:{port}"
        self.management_key = management_key
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(
        self,
        method: str,
        path: str,
        *,
        payload: Any | None = None,
        raw: bytes | None = None,
        worker_token: str | None = None,
        proxy_key: str | None = None,
        timeout: float = 10,
    ) -> tuple[int, bytes]:
        if payload is not None and raw is not None:
            fail("invalid HTTP helper invocation")
        body = raw
        headers: dict[str, str] = {}
        if payload is not None:
            body = json.dumps(payload, separators=(",", ":")).encode()
            headers["Content-Type"] = "application/json"
        elif raw is not None:
            headers["Content-Type"] = "application/yaml"
        if path.startswith("/v0/management/"):
            headers["Authorization"] = "Bearer " + self.management_key
        elif proxy_key is not None:
            headers["Authorization"] = "Bearer " + proxy_key
        if worker_token is not None:
            headers["X-Sync-Config-Writer-Token"] = worker_token
        request = urllib.request.Request(self.origin + path, data=body, method=method, headers=headers)
        try:
            with self.opener.open(request, timeout=timeout) as response:
                return int(response.status), response.read()
        except urllib.error.HTTPError as error:
            return int(error.code), error.read()

    def json(self, method: str, path: str, **kwargs: Any) -> tuple[int, Any]:
        status, raw = self.request(method, path, **kwargs)
        try:
            return status, json.loads(raw)
        except json.JSONDecodeError:
            fail(f"non-JSON response for {method} {path}")


def log_endpoint_count(runtime: Path, endpoint: str) -> int:
    log = runtime / "core.log"
    if not log.exists():
        return 0
    return log.read_text(encoding="utf-8", errors="replace").count(f'"{endpoint}"')


def log_method_endpoint_count(runtime: Path, method: str, endpoint: str) -> int:
    log = runtime / "core.log"
    if not log.exists():
        return 0
    return sum(
        1
        for line in log.read_text(encoding="utf-8", errors="replace").splitlines()
        if f'"{endpoint}"' in line and re.search(rf"\b{re.escape(method)}\b", line)
    )


def assert_run_status_sanitized(status: dict[str, Any]) -> None:
    allowed = {
        "run_id",
        "operation",
        "state",
        "attempt",
        "queued_at",
        "started_at",
        "finished_at",
        "version",
        "changed",
        "error_code",
        "blocking_run_id",
        "instance_id",
        "reconfigure_seq",
        "config_sha256",
    }
    if not set(status).issubset(allowed):
        fail("Writer run status contains unexpected fields")


def plugin_store(plugin_id: str) -> str:
    return f"""      store:
        version: 0.1.0
        release-tag: v0.1.0
        manifest:
          linux:
            amd64:
              artifact: {plugin_id}-v0.1.0.so
              diagnostic:
                selected: true
"""


def generate_config(
    runtime: Path,
    core_port: int,
    provider_base: str,
    root_key: str,
) -> bytes:
    fingerprint = sha256(root_key.encode())
    parts = [
        "host: \"127.0.0.1\"\n",
        f"port: {core_port}\n",
        "tls:\n  enable: false\n",
        "remote-management:\n  allow-remote: false\n  secret-key: \"\"\n  disable-control-panel: true\n",
        f"auth-dir: {quote(runtime / 'auth')}\n",
        f"api-keys:\n  - {quote(root_key)}\n",
        "debug: false\nlogging-to-file: false\nusage-statistics-enabled: false\nproxy-url: http://127.0.0.1:1\n",
        "unrelated-root:\n  marker: preserve-root\n",
        "openai-compatibility:\n",
        "  - name: fixture\n",
        f"    base-url: {quote(provider_base)}\n",
        "    api-key-entries:\n",
        "      - api-key: config-only-placeholder\n        proxy-url: direct\n",
        "    headers:\n      User-Agent: cpa-four-plugin-e2e\n",
        "    models:\n",
        "      - name: keep\n",
        "        alias: keep-public\n",
        "        display-name: Keep Display\n",
        "        max-context-length: 100001\n",
        "        max-input-tokens: 90001\n",
        "        max-output-tokens: 10001\n",
        "        thinking:\n          levels: [medium]\n",
        "        input-modalities: [text]\n",
        "        output-modalities: [text, image]\n",
        "        retained-opaque: keep-stable\n",
        "      - name: remove\n        alias: remove-public\n        retained-opaque: remove-stable\n",
        "plugins:\n  enabled: true\n",
        f"  dir: {quote(runtime / 'plugins')}\n",
        "  configs:\n",
        "    auto-pull-models:\n      enabled: true\n      priority: 20\n",
        plugin_store("auto-pull-models"),
        "      worker_token_env: CPA_WRITER_WORKER_TOKEN\n",
        "      channels:\n",
        "        - enabled: true\n",
        "          selector:\n            name: fixture\n",
        f"            base_url: {quote(provider_base)}\n",
        "          mode: include\n          patterns: ['.*']\n",
        "    model-metadata-sync:\n      enabled: true\n      priority: 30\n",
        plugin_store("model-metadata-sync"),
        "      worker_token_env: CPA_WRITER_WORKER_TOKEN\n",
        "      channels:\n",
        "        - enabled: true\n          kind: openai-compatibility\n",
        "          selector:\n            name: fixture\n",
        f"            base_url: {quote(provider_base)}\n",
        "          upstream_meta: true\n          codex_manifest: true\n",
        "          overrides:\n",
        "            keep:\n              max_context_length: 210001\n              max_input_tokens: 190001\n              max_output_tokens: 21001\n",
        "            new:\n              max_context_length: 210002\n              max_input_tokens: 190002\n              max_output_tokens: 21002\n",
        "    model-info:\n      enabled: true\n      priority: 40\n",
        plugin_store("model-info"),
        "      worker_token_env: CPA_WRITER_WORKER_TOKEN\n",
        "    sync-config-write:\n      enabled: true\n      priority: 10\n",
        plugin_store("sync-config-write"),
        f"      core_origin: http://127.0.0.1:{core_port}\n",
        "      management_key_env: MANAGEMENT_PASSWORD\n",
        f"      model_info_proxy_api_key_sha256: {fingerprint}\n",
        "      worker_token_env: CPA_WRITER_WORKER_TOKEN\n",
        "      auto_pull_interval: 0s\n      metadata_sync_interval: 0s\n      model_info_interval: 0s\n",
        "      max_version_retries: 2\n",
    ]
    return "".join(parts).encode()


def start_core(
    server: Path,
    runtime: Path,
    config: Path,
    ca_cert: Path,
    management_key: str,
    worker_token: str,
) -> tuple[subprocess.Popen[bytes], Any]:
    log_handle = (runtime / "core.log").open("ab", buffering=0)
    runtime_tmp = runtime / "tmp"
    runtime_tmp.mkdir(exist_ok=True)
    env = os.environ.copy()
    env.update(
        {
            "HOME": str(runtime / "home"),
            "MANAGEMENT_PASSWORD": management_key,
            "CPA_WRITER_WORKER_TOKEN": worker_token,
            "SSL_CERT_FILE": str(ca_cert),
            "HTTP_PROXY": "",
            "HTTPS_PROXY": "",
            "ALL_PROXY": "",
            "NO_PROXY": "127.0.0.1,::1",
            "TMPDIR": str(runtime_tmp),
        }
    )
    process = subprocess.Popen(
        [str(server), "-config", str(config), "-local-model"],
        cwd=runtime,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=log_handle,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    return process, log_handle


def stop_core(process: subprocess.Popen[bytes] | None, log_handle: Any | None) -> None:
    if process is not None and process.poll() is None:
        stop_process_group(process, grace=10)
    if log_handle is not None:
        log_handle.close()


def wait_until(description: str, check: Callable[[], Any], timeout: float = 30) -> Any:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except (ConnectionError, urllib.error.URLError, TimeoutError, OSError) as error:
            last_error = error
        time.sleep(0.1)
    if last_error:
        fail(f"timeout: {description}")
    fail(f"timeout: {description}")


def wait_plugins(client: CoreClient, process: subprocess.Popen[bytes]) -> list[dict[str, Any]]:
    def check() -> list[dict[str, Any]] | None:
        if process.poll() is not None:
            fail("Core exited before plugin registration")
        status, body = client.json("GET", "/v0/management/plugins")
        if status != 200:
            return None
        entries = {entry["id"]: entry for entry in body.get("plugins", [])}
        if all(entries.get(plugin_id, {}).get("registered") and entries[plugin_id].get("effective_enabled") for plugin_id in PLUGIN_IDS):
            return [entries[plugin_id] for plugin_id in PLUGIN_IDS]
        return None

    return wait_until("four plugins registered", check, timeout=45)


def writer_status(client: CoreClient, run_id: str | None = None) -> dict[str, Any]:
    suffix = "" if run_id is None else "?run_id=" + run_id
    status, body = client.json("GET", "/v0/management/plugins/sync-config-write/status" + suffix)
    if status != 200:
        fail("Writer status unavailable")
    return body


def trigger_and_wait(client: CoreClient, path: str, expected_state: str = "succeeded", timeout: float = 45) -> dict[str, Any]:
    status, body = client.json("POST", path, payload={})
    if status != 202 or not body.get("run_id"):
        fail(f"Writer trigger rejected: {path}")
    run_id = body["run_id"]

    def check() -> dict[str, Any] | None:
        current = writer_status(client, run_id)
        if current.get("state") in ACTIVE_STATES:
            return None
        return current

    result = wait_until("Writer run completion", check, timeout=timeout)
    if result.get("state") != expected_state:
        fail(f"Writer run ended as {result.get('state')}:{result.get('error_code', '')}")
    return result


def worker_statuses(client: CoreClient, worker_token: str) -> dict[str, dict[str, Any]]:
    result: dict[str, dict[str, Any]] = {}
    for plugin_id in ("auto-pull-models", "model-metadata-sync", "model-info"):
        status, body = client.json(
            "GET",
            f"/v0/management/plugins/{plugin_id}/writer-status",
            worker_token=worker_token,
        )
        if status != 200:
            fail(f"worker status unavailable: {plugin_id}")
        result[plugin_id] = body
    result["sync-config-write"] = writer_status(client)
    return result


def assert_private_routes(client: CoreClient) -> None:
    requests = (
        ("POST", "/v0/management/plugins/auto-pull-models/plan"),
        ("GET", "/v0/management/plugins/auto-pull-models/writer-status"),
        ("POST", "/v0/management/plugins/model-metadata-sync/plan"),
        ("GET", "/v0/management/plugins/model-metadata-sync/writer-status"),
        ("POST", "/v0/management/plugins/model-info/ingest"),
        ("GET", "/v0/management/plugins/model-info/writer-status"),
    )
    for method, path in requests:
        status, _ = client.request(method, path, payload={} if method == "POST" else None)
        if status != 401:
            fail(f"private worker route did not reject missing worker token: {path}")


def get_config(client: CoreClient) -> tuple[bytes, dict[str, Any]]:
    raw_status, raw = client.request("GET", "/v0/management/config.yaml")
    json_status, decoded = client.json("GET", "/v0/management/config")
    if raw_status != 200 or json_status != 200:
        fail("authoritative config unavailable")
    return raw, decoded


def configured_models(config: dict[str, Any]) -> list[dict[str, Any]]:
    channels = config.get("openai-compatibility") or config.get("openai_compatibility")
    if not isinstance(channels, list) or len(channels) != 1:
        fail("unexpected OpenAI-compatible config")
    models = channels[0].get("models")
    if not isinstance(models, list):
        fail("configured model list unavailable")
    return models


def model_block(raw: bytes, name: str) -> str:
    lines = raw.decode().splitlines()
    start = -1
    indent = 0
    target = f"- name: {name}"
    for index, line in enumerate(lines):
        if line.strip() == target:
            start, indent = index, len(line) - len(line.lstrip())
            break
    if start < 0:
        fail(f"model block missing: {name}")
    end = len(lines)
    for index in range(start + 1, len(lines)):
        line = lines[index]
        if not line.strip():
            continue
        current_indent = len(line) - len(line.lstrip())
        if current_indent < indent or current_indent == indent and line.lstrip().startswith("- "):
            end = index
            break
    return "\n".join(lines[start:end]) + "\n"


def normalized_block(block: str) -> str:
    lines = block.splitlines()
    indents = [len(line) - len(line.lstrip()) for line in lines if line.strip()]
    margin = min(indents) if indents else 0
    return "\n".join(line[margin:] for line in lines) + "\n"


def scalar_from_block(block: str, key: str) -> str | None:
    match = re.search(rf"^\s*{re.escape(key)}:\s*([^#\n]+)", block, re.MULTILINE)
    return None if match is None else match.group(1).strip().strip('"\'')


def assert_membership(
    raw: bytes,
    decoded: dict[str, Any],
    *,
    expect_initial_metadata: bool = True,
    before_raw: bytes | None = None,
    before_decoded: dict[str, Any] | None = None,
) -> None:
    models = configured_models(decoded)
    if [model.get("name") for model in models] != ["keep", "new"]:
        fail("membership order mismatch")
    keep, new = models
    if keep.get("alias") != "keep-public" or keep.get("display-name") != "Keep Display":
        fail("retained keep mapping changed")
    if new.get("name") != "new" or new.get("alias") not in (None, ""):
        fail("new model gained a non-minimal public identity")
    keep_raw, new_raw = model_block(raw, "keep"), model_block(raw, "new")
    if expect_initial_metadata and [line.strip() for line in new_raw.splitlines() if line.strip()] != ["- name: new"]:
        fail("new raw model node is not minimal")
    if before_raw is not None and before_decoded is not None:
        before_models = {model["name"]: model for model in configured_models(before_decoded)}
        before_keep_raw = model_block(before_raw, "keep")
        if "keep" not in before_models or normalized_block(keep_raw) != normalized_block(before_keep_raw):
            fail("membership did not retain the complete keep mapping")
    markers = [
        "alias: keep-public",
        "display-name: Keep Display",
        "retained-opaque: keep-stable",
    ]
    if expect_initial_metadata:
        markers.append("max-input-tokens: 90001")
    for marker in markers:
        if marker not in keep_raw:
            fail("retained keep node incomplete")
    if "alias:" in new_raw or "retained-opaque:" in new_raw or b"name: remove" in raw:
        fail("minimal-new/removal membership invariant failed")
    if b"marker: preserve-root" not in raw:
        fail("unrelated root mutation detected")


def assert_metadata(before_raw: bytes, before: dict[str, Any], after_raw: bytes, after: dict[str, Any]) -> None:
    before_models = configured_models(before)
    after_models = configured_models(after)
    if [model.get("name") for model in after_models] != ["keep", "new"]:
        fail("metadata changed membership/order")
    before_by_name = {model["name"]: model for model in before_models}
    after_by_name = {model["name"]: model for model in after_models}
    expected_raw = {
        "keep": ("210001", "190001", "21001", "levels: [low, high]"),
        "new": ("210002", "190002", "21002", "levels: [medium, high]"),
    }
    expected_json_changes = OWNED_MODEL_KEYS - {"max-input-tokens"}
    for name in ("keep", "new"):
        before_model, after_model = before_by_name[name], after_by_name[name]
        changed = {
            key
            for key in set(before_model) | set(after_model)
            if before_model.get(key) != after_model.get(key)
        }
        if changed != expected_json_changes:
            fail(f"metadata changed fields outside owned set: {name}")
        block = model_block(after_raw, name)
        context, max_input, max_output, levels = expected_raw[name]
        if scalar_from_block(block, "max-context-length") != context or scalar_from_block(block, "max-input-tokens") != max_input or scalar_from_block(block, "max-output-tokens") != max_output:
            fail(f"metadata token limits mismatch: {name}")
        if levels not in block or "input-modalities: [text, image]" not in block or "output-modalities: [text]" not in block:
            fail(f"rich metadata fields mismatch: {name}")
    after_keep, after_new = after_by_name["keep"], after_by_name["new"]
    if after_keep.get("alias") != "keep-public" or after_keep.get("display-name") != "Keep Display" or after_new.get("alias") not in (None, ""):
        fail("metadata changed name/alias/unrelated supported keys")
    if "retained-opaque: keep-stable" not in model_block(after_raw, "keep") or b"marker: preserve-root" not in after_raw:
        fail("metadata changed unrelated opaque nodes")


def assert_epoch_and_convergence(
    client: CoreClient,
    worker_token: str,
    before_statuses: dict[str, dict[str, Any]],
    temp: Path,
    config_raw: bytes,
) -> dict[str, dict[str, Any]]:
    epochs: set[str] = set()
    for plugin_id in PLUGIN_IDS:
        status, body = client.json("GET", f"/v0/management/plugins/{plugin_id}/config")
        if status != 200 or not body.get("sync_epoch"):
            fail("plugin sync_epoch unavailable")
        epochs.add(body["sync_epoch"])
        store = body.get("store", {})
        if store.get("version") != "0.1.0" or not isinstance(store.get("manifest"), dict):
            fail("opaque plugin store config did not survive")
    if len(epochs) != 1:
        fail("plugins do not share one fresh sync_epoch")
    after = worker_statuses(client, worker_token)
    expected_hashes = independent_runtime_hashes(temp, config_raw)
    for plugin_id in PLUGIN_IDS:
        previous, current = before_statuses[plugin_id], after[plugin_id]
        if previous.get("instance_id") != current.get("instance_id"):
            fail("plugin process instance changed during commit")
        if int(current.get("reconfigure_seq", 0)) <= int(previous.get("reconfigure_seq", 0)):
            fail("plugin reconfigure sequence did not advance")
        if current.get("config_sha256") != expected_hashes[plugin_id]:
            fail("plugin runtime config hash does not match independently reproduced ConfigYAML")
    return after


def assert_model_info(client: CoreClient, secrets_to_hide: tuple[str, ...]) -> None:
    views: dict[str, dict[str, Any]] = {}
    for name, path in (
        ("catalog", "/v0/management/plugins/model-info/catalog"),
        ("effective", "/v0/management/plugins/model-info/effective"),
    ):
        status, body = client.json("GET", path)
        if status != 200 or body.get("count") != 2 or not isinstance(body.get("models"), list):
            fail(f"model-info {name} view is not exact")
        views[name] = body
    expected = {
        "keep-public": ("keep-public", "Keep Display", 210001, 21001, ["low", "high"]),
        "new": ("new", "new", 210002, 21002, ["medium", "high"]),
    }
    for view_name, body in views.items():
        rows = {row.get("id"): row for row in body["models"]}
        if set(rows) != set(expected):
            fail(f"model-info {view_name} identities are wrong")
        for model_id, (slug, display, context, output, levels) in expected.items():
            row = rows[model_id]
            if row.get("slug") != slug or row.get("display_name", "") != display:
                fail(f"model-info {view_name} identity metadata is wrong: id={model_id} slug={row.get('slug', '')!r} display={row.get('display_name', '')!r}")
            if row.get("context_window") != context or row.get("max_tokens") != output:
                fail(f"model-info {view_name} token limits are wrong")
            if row.get("reasoning_levels") != levels or row.get("input_modalities") != ["text", "image"] or row.get("output_modalities") not in (None, []):
                safe = {key: row.get(key) for key in ("reasoning_levels", "input_modalities", "output_modalities")}
                fail(f"model-info {view_name} capabilities are wrong: id={model_id} values={safe}")
            if view_name == "catalog" and row.get("max_input_tokens") != context:
                fail(f"model-info catalog max input fallback is wrong: id={model_id} value={row.get('max_input_tokens')}")
            if view_name == "effective" and (row.get("max_input_tokens") != context or row.get("max_source") != "upstream"):
                fail("model-info effective fallback/source is wrong")
    serialized = json.dumps(views, separators=(",", ":"))
    if any(secret and secret in serialized for secret in secrets_to_hide):
        fail("generated secret leaked into model-info views")
    post_status, _ = client.request("POST", "/v0/management/plugins/model-info/catalog", payload={})
    if post_status != 404:
        fail("model-info catalog POST unexpectedly available")


def assert_startup_block(client: CoreClient) -> None:
    status = writer_status(client)
    blocking_run_id = status.get("blocking_run_id")
    if not status.get("writer_blocked") or status.get("error_code") != "startup_reconcile_required" or not blocking_run_id:
        fail("Writer did not start blocked")
    reconcile_runs = [run for run in status.get("runs", []) if run.get("operation") == "reconcile"]
    if len(reconcile_runs) != 1:
        fail("automatic startup reconcile record exists")
    blocker = reconcile_runs[0]
    if blocker.get("run_id") != blocking_run_id or blocker.get("state") != "blocked" or blocker.get("error_code") != "startup_reconcile_required":
        fail("startup reconcile blocker evidence is invalid")
    blocked_status, blocked = client.json("POST", "/v0/management/plugins/sync-config-write/run/auto-pull-models", payload={})
    if blocked_status != 409 or blocked.get("error_code") != "writer_blocked" or blocked.get("blocking_error_code") != "startup_reconcile_required":
        fail("startup block did not reject write")


def sanitize_and_check_evidence(runtime: Path, secrets_to_hide: tuple[str, ...], statuses: list[dict[str, Any]]) -> None:
    log_raw = (runtime / "core.log").read_text(encoding="utf-8", errors="replace")
    serialized = json.dumps(statuses, separators=(",", ":"))
    for secret in secrets_to_hide:
        if secret and (secret in log_raw or secret in serialized):
            fail("generated secret leaked into logs/status")


def cleanup_temp(temp: Path) -> None:
    try:
        shutil.rmtree(temp)
    except Exception as error:
        print(f"E2E_TMP {temp}", file=sys.stderr, flush=True)
        raise RuntimeError("failed to remove E2E temporary directory") from error
    if temp.exists():
        print(f"E2E_TMP {temp}", file=sys.stderr, flush=True)
        fail("E2E temporary directory still exists after cleanup")


def main() -> int:
    source = Path(os.environ.get("CPA_CORE_SOURCE", str(ROOT.parent / "CLIProxyAPI")))
    keep = os.environ.get("CPA_KEEP_E2E_TMP") == "1"
    cache_parent = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))) / "pi-agent"
    cache_parent.mkdir(parents=True, exist_ok=True)
    os.chmod(cache_parent, 0o700)

    temp: Path | None = None
    process: subprocess.Popen[bytes] | None = None
    log_handle: Any | None = None
    provider: ProviderFixture | None = None
    for signum in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(signum, exit_on_signal)
    try:
        temp = Path(tempfile.mkdtemp(prefix="cpa-four-plugin-e2e-", dir=cache_parent))
        os.chmod(temp, 0o700)
        statuses_for_scan: list[dict[str, Any]] = []
        management_key = secrets.token_urlsafe(32)
        worker_token = secrets.token_urlsafe(32)
        root_key = secrets.token_urlsafe(32)
        provider_token = secrets.token_urlsafe(32)

        core, core_head = compose_core(source, temp)
        phase("selected Core composition")
        server, hashes = build_all(core, temp)
        phase("Core tests/build and reproducible plugin builds")

        ca_cert, server_cert = generate_certificates(temp)
        provider_state = ProviderState(provider_token)
        provider = ProviderFixture(provider_state, server_cert, server_cert.with_name("server.key"))
        provider.start()
        provider_base = f"https://127.0.0.1:{provider.port}/v1"

        runtime = temp / "runtime"
        (runtime / "auth").mkdir(parents=True)
        (runtime / "home").mkdir()
        shutil.copytree(temp / "plugins", runtime / "plugins")
        auth_file = runtime / "auth" / "fixture.json"
        auth_file.write_text(
            json.dumps(
                {
                    "type": "openai-compatibility",
                    "base_url": provider_base,
                    "token": provider_token,
                    "proxy_url": "direct",
                    "email": "fixture@example.invalid",
                },
                separators=(",", ":"),
            ),
            encoding="utf-8",
        )
        os.chmod(auth_file, 0o600)
        core_port = free_port()
        config_path = runtime / "config.yaml"
        config_path.write_bytes(generate_config(runtime, core_port, provider_base, root_key))
        client = CoreClient(core_port, management_key)

        process, log_handle = start_core(server, runtime, config_path, ca_cert, management_key, worker_token)
        wait_plugins(client, process)
        assert_private_routes(client)
        phase("four plugins registered and worker routes private")

        assert_startup_block(client)
        trigger_and_wait(client, "/v0/management/plugins/sync-config-write/reconcile")
        initial_statuses = worker_statuses(client, worker_token)
        statuses_for_scan.extend(initial_statuses.values())
        initial_raw, initial_config = get_config(client)
        phase("explicit startup reconcile only")

        membership_before = provider_state.count("/v1/models")
        api_calls_before = log_endpoint_count(runtime, "/v0/management/api-call")
        puts_before = log_method_endpoint_count(runtime, "PUT", "/v0/management/config.yaml")
        trigger_and_wait(client, "/v0/management/plugins/sync-config-write/run/auto-pull-models")
        if provider_state.count("/v1/models") != membership_before + 1 or log_endpoint_count(runtime, "/v0/management/api-call") != api_calls_before + 1:
            fail("membership did not use stock /api-call provider path")
        if log_method_endpoint_count(runtime, "PUT", "/v0/management/config.yaml") != puts_before + 1:
            fail("membership did not perform exactly one official config PUT")
        after_auto_raw, after_auto = get_config(client)
        assert_membership(after_auto_raw, after_auto, before_raw=initial_raw, before_decoded=initial_config)
        converged = assert_epoch_and_convergence(client, worker_token, initial_statuses, temp, after_auto_raw)
        statuses_for_scan.extend(converged.values())
        phase("membership commit and runtime convergence")

        rich_path = "/v1/models?client_version=1.0.0"
        rich_before = provider_state.count(rich_path)
        api_calls_before = log_endpoint_count(runtime, "/v0/management/api-call")
        puts_before = log_method_endpoint_count(runtime, "PUT", "/v0/management/config.yaml")
        trigger_and_wait(client, "/v0/management/plugins/sync-config-write/run/model-metadata-sync")
        if provider_state.count(rich_path) != rich_before + 1 or log_endpoint_count(runtime, "/v0/management/api-call") != api_calls_before + 1:
            fail("metadata did not use rich provider path through stock /api-call")
        if log_method_endpoint_count(runtime, "PUT", "/v0/management/config.yaml") != puts_before + 1:
            fail("metadata did not perform exactly one official config PUT")
        after_metadata_raw, after_metadata = get_config(client)
        assert_metadata(after_auto_raw, after_auto, after_metadata_raw, after_metadata)
        converged = assert_epoch_and_convergence(client, worker_token, converged, temp, after_metadata_raw)
        statuses_for_scan.extend(converged.values())
        phase("six-field metadata ownership and runtime convergence")

        model_info_gets_before = log_endpoint_count(runtime, "/v1/models?client_version=1.0.0")
        model_info_run = trigger_and_wait(client, "/v0/management/plugins/sync-config-write/model-info/catalog")
        assert_run_status_sanitized(model_info_run)
        if log_endpoint_count(runtime, "/v1/models?client_version=1.0.0") != model_info_gets_before + 1:
            fail("model-info did not fetch exact Core catalog path")
        statuses_for_scan.append(model_info_run)
        assert_model_info(client, (management_key, worker_token, root_key, provider_token))
        phase("read-only model-info refresh")

        stop_core(process, log_handle)
        process, log_handle = None, None
        persisted_before_restart = config_path.read_bytes()
        process, log_handle = start_core(server, runtime, config_path, ca_cert, management_key, worker_token)
        wait_plugins(client, process)
        assert_startup_block(client)
        restarted_raw, restarted = get_config(client)
        if restarted_raw != persisted_before_restart:
            fail("restart changed persisted YAML before reconcile")
        assert_membership(restarted_raw, restarted, expect_initial_metadata=False)
        assert_metadata(after_auto_raw, after_auto, restarted_raw, restarted)
        trigger_and_wait(client, "/v0/management/plugins/sync-config-write/reconcile")
        restart_model_info = trigger_and_wait(client, "/v0/management/plugins/sync-config-write/model-info/catalog")
        statuses_for_scan.append(restart_model_info)
        assert_model_info(client, (management_key, worker_token, root_key, provider_token))
        phase("restart persistence and explicit reconcile")

        wait_until(
            "file auth visible",
            lambda: any(
                item.get("name") == auth_file.name
                for item in client.json("GET", "/v0/management/auth-files")[1].get("files", [])
            ),
        )
        auth_file.unlink()
        wait_until(
            "file auth removal",
            lambda: not any(
                item.get("name") == auth_file.name
                for item in client.json("GET", "/v0/management/auth-files")[1].get("files", [])
            ),
        )
        before_negative_raw, _ = get_config(client)
        provider_requests_before = provider_state.total
        puts_before = log_method_endpoint_count(runtime, "PUT", "/v0/management/config.yaml")
        negative = trigger_and_wait(
            client,
            "/v0/management/plugins/sync-config-write/run/auto-pull-models",
            expected_state="failed",
        )
        statuses_for_scan.append(negative)
        if negative.get("error_code") != "provider_credential_unavailable":
            fail("config-only credential did not fail closed")
        after_negative_raw, _ = get_config(client)
        if provider_state.total != provider_requests_before or after_negative_raw != before_negative_raw:
            fail("credential-unavailable run made provider/config changes")
        if log_method_endpoint_count(runtime, "PUT", "/v0/management/config.yaml") != puts_before:
            fail("credential-unavailable run attempted config PUT")
        phase("config-only credential negative gate")

        sanitize_and_check_evidence(runtime, (management_key, worker_token, root_key, provider_token), statuses_for_scan)
        phase("secret-free statuses/logs")
        stop_core(process, log_handle)
        process, log_handle = None, None
        if provider is not None:
            provider.close()
            provider = None
        print(f"CORE_HEAD {core_head}")
        for name in ("core", *PLUGIN_IDS):
            print(f"SHA256 {name} {hashes[name]}")
        print("PASS selected-Core four-plugin E2E", flush=True)
        return 0
    except Exception as error:
        print(f"FAIL {error}", file=sys.stderr, flush=True)
        return 1
    finally:
        for signum in (signal.SIGTERM, signal.SIGHUP):
            signal.signal(signum, signal.SIG_IGN)
        stop_core(process, log_handle)
        if provider is not None:
            provider.close()
        if temp is not None:
            if keep:
                print(f"E2E_TMP {temp}", file=sys.stderr, flush=True)
            else:
                cleanup_temp(temp)


if __name__ == "__main__":
    raise SystemExit(main())
