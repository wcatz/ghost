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


def test_missing_opencode_binary_is_not_retried():
    """A missing or non-executable opencode binary is not transient.

    Catching bare OSError made FileNotFoundError/PermissionError burn three
    attempts with exponential backoff (~7s) before surfacing an error that will
    never resolve on its own.
    """
    _clear_version_cache()
    attempts = []

    def fake_run(cmd, **kwargs):
        # The version probe is a separate subprocess call; only the `run`
        # invocations are the thing under test.
        if cmd[:1] == ["opencode"] and "run" in cmd:
            attempts.append(cmd)
        raise FileNotFoundError(2, "No such file or directory", "opencode")

    with mock.patch("subprocess.run", side_effect=fake_run), mock.patch.object(
        phase4_run.time, "sleep"
    ) as sleep:
        try:
            phase4_run.chat_opencode("opencode/big-pickle", "hello", 64)
        except RuntimeError as e:
            assert "not found or not executable" in str(e)
        else:
            raise AssertionError("missing opencode binary must raise, not return")

    # One attempt only: the error is terminal, so no retry loop and no sleeps.
    assert len(attempts) == 1, f"expected 1 attempt, got {len(attempts)}"
    assert sleep.call_count == 0, "terminal OSError must not sleep between retries"


def test_opencode_nonzero_exit_is_still_retried():
    """The narrowing must not disable the retry the loop exists for.

    A non-zero exit from a real binary is the transient case (provider hiccup,
    rate limit), so RuntimeError keeps its three attempts.
    """
    _clear_version_cache()
    run_calls, _, fake_run = _run_with_version("opencode v2.0.14\n")
    flaky_calls = []

    def always_fail(cmd, **kwargs):
        if cmd[:1] == ["opencode"] and "run" in cmd:
            flaky_calls.append(cmd)
        return _completed(cmd, stderr="rate limited", returncode=1)

    with mock.patch("subprocess.run", side_effect=always_fail), mock.patch.object(
        phase4_run.time, "sleep"
    ) as sleep:
        try:
            phase4_run.chat_opencode("opencode/big-pickle", "hello", 64)
        except RuntimeError as e:
            assert "exhausted retries" in str(e)
        else:
            raise AssertionError("persistent non-zero exit must raise")

    assert len(flaky_calls) == 3, f"expected 3 attempts, got {len(flaky_calls)}"
    # One backoff sleep per failed attempt, including the last (the loop has no
    # "is there another attempt left" check, so the final sleep is unconditional).
    assert sleep.call_count == 3, (
        f"expected a backoff sleep per failed attempt, got {sleep.call_count}"
    )


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
