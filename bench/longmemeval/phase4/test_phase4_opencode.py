"""Tests for the standalone Phase 4 OpenCode CLI adapter."""

import os
import subprocess
import sys
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import phase4_run  # noqa: E402


def _completed(cmd, *, stdout="", stderr="", returncode=0):
    return subprocess.CompletedProcess(cmd, returncode, stdout, stderr)


def _run_with_version(version_output):
    run_calls = []
    envs = []

    def fake_run(cmd, **kwargs):
        if cmd[1:] == ["--version"]:
            return _completed(cmd, stdout=version_output)
        run_calls.append(cmd)
        envs.append(kwargs["env"])
        return _completed(
            cmd,
            stdout='{"type":"text","part":{"type":"text","text":"ok"}}\n',
        )

    return run_calls, envs, fake_run


def _clear_version_cache():
    version_probe = getattr(phase4_run, "opencode_major_version", None)
    if hasattr(version_probe, "cache_clear"):
        version_probe.cache_clear()


def test_opencode_v2_uses_standalone_and_not_pure():
    _clear_version_cache()
    run_calls, _, fake_run = _run_with_version("opencode v2.0.14\n")

    with mock.patch("subprocess.run", side_effect=fake_run), mock.patch.object(
        phase4_run.time, "sleep"
    ):
        result = phase4_run.chat_opencode("opencode/big-pickle", "hello", 64)

    assert result == "ok"
    assert len(run_calls) == 1
    assert "--standalone" in run_calls[0]
    assert "--pure" not in run_calls[0]


def test_opencode_v1_uses_pure_and_not_standalone():
    _clear_version_cache()
    run_calls, _, fake_run = _run_with_version("1.18.30\n")

    with mock.patch("subprocess.run", side_effect=fake_run), mock.patch.object(
        phase4_run.time, "sleep"
    ):
        result = phase4_run.chat_opencode("opencode/big-pickle", "hello", 64)

    assert result == "ok"
    assert len(run_calls) == 1
    assert "--pure" in run_calls[0]
    assert "--standalone" not in run_calls[0]


def test_opencode_child_keeps_only_opencode_provider_credentials():
    _clear_version_cache()
    run_calls, envs, fake_run = _run_with_version("opencode v2.0.14\n")
    env = {
        "ANTHROPIC_API_KEY": "anthropic-upper",
        "anthropic_api_key": "anthropic-lower",
        "OPENAI_API_KEY": "openai",
        "GOOSE_PROVIDER__API_KEY": "goose",
        "OPENCODE_API_KEY": "opencode",
        "OPENCODE_CONFIG": "project-config",
    }

    with mock.patch.dict(os.environ, env, clear=True), mock.patch(
        "subprocess.run", side_effect=fake_run
    ), mock.patch.object(phase4_run.time, "sleep"):
        result = phase4_run.chat_opencode("opencode/big-pickle", "hello", 64)

    assert result == "ok"
    assert len(run_calls) == 1
    child_env = envs[0]
    assert child_env["OPENCODE_API_KEY"] == "opencode"
    assert not any("ANTHROPIC_API_KEY" in key.upper() for key in child_env)
    assert "OPENAI_API_KEY" not in child_env
    assert "GOOSE_PROVIDER__API_KEY" not in child_env
    assert "OPENCODE_CONFIG" not in child_env
