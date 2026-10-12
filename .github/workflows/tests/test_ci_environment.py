"""Exercise CI shell setup and its real Make test command with synthetic paths."""
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".github/workflows/ci.yml"


def step(name):
    text = WORKFLOW.read_text()
    marker = "      - name: " + name + "\n"
    if marker not in text:
        raise AssertionError("missing workflow step: " + name)
    section = text.split(marker, 1)[1]
    section = re.split(r"^      - (?:name:|uses:)", section, maxsplit=1, flags=re.M)[0]
    return section


def shell(section):
    value = section.split("        run: ", 1)[1]
    if value.startswith("|\n"):
        return "\n".join(line[10:] for line in value.splitlines()[1:] if line.startswith("          "))
    return value.splitlines()[0]


def environment(section):
    if "        env:\n" not in section:
        return {}
    values = section.split("        env:\n", 1)[1].split("        run:", 1)[0]
    return {key: value.strip().strip("'\"") for key, value in re.findall(r"^          ([A-Z_]+): (.+)$", values, flags=re.M)}


class CIEnvironmentTests(unittest.TestCase):
    def test_symlink_runner_temp_becomes_private_physical_go_temp(self):
        with tempfile.TemporaryDirectory(prefix="wa-ci-env-") as temporary:
            root = Path(temporary).resolve()
            runner = root / "physical-runner-temp"
            runner.mkdir(mode=0o700)
            alias = root / "runner-temp-alias"
            alias.symlink_to(runner, target_is_directory=True)
            github_env = root / "github-env"
            env = os.environ.copy()
            env.update(RUNNER_TEMP=str(alias), GITHUB_ENV=str(github_env))
            subprocess.run(["bash", "-e", "-c", shell(step("Prepare private test directory"))], env=env, check=True)
            exported = dict(line.split("=", 1) for line in github_env.read_text().splitlines())
            go_temp = Path(exported["TMPDIR"])
            self.assertEqual(str(go_temp), str(go_temp.resolve()))
            self.assertEqual(go_temp.parent, runner)
            self.assertEqual(go_temp.stat().st_mode & 0o777, 0o700)
            child = go_temp / "synthetic-private-profile"
            child.mkdir(mode=0o700)
            self.assertEqual(str(child), str(child.resolve()))

    def test_actual_make_test_receives_bounded_package_and_runtime_parallelism(self):
        with tempfile.TemporaryDirectory(prefix="wa-ci-command-") as temporary:
            root = Path(temporary).resolve()
            capture = root / "go-invocation.json"
            fake_go = root / "fake-go"
            fake_go.write_text("#!/usr/bin/env python3\nimport json,os,sys\nfrom pathlib import Path\nPath(os.environ['WA_CI_TEST_CAPTURE']).write_text(json.dumps({'argv':sys.argv[1:], 'GOFLAGS':os.environ.get('GOFLAGS'), 'GOMAXPROCS':os.environ.get('GOMAXPROCS'), 'CGO_ENABLED':os.environ.get('CGO_ENABLED')}))\n")
            fake_go.chmod(0o700)
            test = step("Test")
            env = os.environ.copy()
            env.pop("GOFLAGS", None)
            env.pop("GOMAXPROCS", None)
            env.update(environment(test), GO=str(fake_go), WA_CI_TEST_CAPTURE=str(capture))
            subprocess.run(["bash", "-e", "-c", shell(test)], cwd=ROOT, env=env, check=True, stdout=subprocess.PIPE, text=True)
            actual = json.loads(capture.read_text())
            self.assertEqual(actual["GOFLAGS"], "-p=1")
            self.assertEqual(actual["GOMAXPROCS"], "2")
            self.assertEqual(actual["CGO_ENABLED"], "1")
            self.assertEqual(actual["argv"], ["test", "-tags", "fts5,sqlite_fts5,ts_omit_logtail", "./..."])


if __name__ == "__main__":
    unittest.main()
