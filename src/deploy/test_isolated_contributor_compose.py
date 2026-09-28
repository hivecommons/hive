"""Validate the example's isolation boundary and execute its startup shell.

Run: python3 src/deploy/test_isolated_contributor_compose.py (requires PyYAML).
External CLIs are stubbed; this does not claim to exercise a container runtime.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / 'src/examples/contributor-isolated'
COMPOSE = yaml.safe_load((EXAMPLE / 'compose.yaml').read_text())
SERVICE = COMPOSE['services']['contributor']


class IsolatedContributor(unittest.TestCase):
    def test_container_boundary(self):
        self.assertNotIn('build', SERVICE)
        self.assertIn('ghcr.io/hivecommons/hive-contributor:', SERVICE['image'])
        self.assertEqual(SERVICE['user'], '1000:1000')
        # Every mount is a declared named volume, never a bind or runtime socket.
        for mount in SERVICE['volumes']:
            source, target = mount.split(':')
            self.assertIn(source, COMPOSE['volumes'])
            self.assertEqual(target, '/home/dev')
        self.assertTrue(SERVICE['volumes'])
        self.assertNotIn('env_file', SERVICE)
        for name in SERVICE['environment']:
            self.assertNotIn(name, ('GH_TOKEN', 'GITHUB_TOKEN', 'OPENAI_API_KEY',
                                    'ANTHROPIC_API_KEY', 'CODEX_HOME'))
        self.assertFalse(SERVICE.get('privileged', False))
        self.assertNotIn('cap_add', SERVICE)
        self.assertEqual(SERVICE['cap_drop'], ['ALL'])
        self.assertEqual(SERVICE['security_opt'], ['no-new-privileges:true'])
        self.assertEqual(SERVICE['environment']['CONTRIBUTOR_MODE'], 'headless')
        self.assertEqual(SERVICE['mem_limit'], SERVICE['memswap_limit'])
        self.assertIn('cpus', SERVICE)

    def run_entrypoint(self, nice='10', token='volume-token', status='0'):
        with tempfile.TemporaryDirectory() as directory:
            tmp = Path(directory)
            gh = tmp / 'gh'
            gh.write_text('#!/bin/bash\nprintf "%s" "$TEST_TOKEN"\nexit "$TEST_STATUS"\n')
            agent = tmp / 'agent'
            agent.write_text('#!/bin/bash\nprintf "%s\\n" "$GH_TOKEN" > "$TEST_OUTPUT"\n'
                             "python3 -c 'import os; print(os.getpriority(os.PRIO_PROCESS, 0))' >> \"$TEST_OUTPUT\"\n")
            gh.chmod(0o755)
            agent.chmod(0o755)
            script = SERVICE['entrypoint'][2].replace('$$', '$')
            # Substitute only absolute image executables; run the actual logic
            # and real nice, including its inherited scheduling priority.
            script = script.replace('/usr/bin/gh', str(gh)).replace(
                '/usr/local/bin/contributor-agent.sh', str(agent))
            output = tmp / 'result'
            env = dict(os.environ, HIVE_CONTRIBUTOR_NICE=nice,
                       TEST_TOKEN=token, TEST_STATUS=status, TEST_OUTPUT=str(output),
                       GH_TOKEN='host-token-must-not-win')
            result = subprocess.run(SERVICE['entrypoint'][:2] + [script], env=env,
                                    text=True, capture_output=True, timeout=5)
            self.assertNotIn(token or 'unused-secret', result.stdout + result.stderr)
            return result, output.read_text().splitlines() if output.exists() else None

    def test_saved_token_and_niceness_reach_agent(self):
        for priority in ('0', '10', '19'):
            with self.subTest(priority=priority):
                result, output = self.run_entrypoint(nice=priority)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(output[0], 'volume-token')
                self.assertGreaterEqual(int(output[1]), int(priority))

    def test_missing_login_never_starts_agent(self):
        for token, status in (('', '0'), ('', '1'), ('partial-secret', '1')):
            with self.subTest(status=status, token=bool(token)):
                result, output = self.run_entrypoint(token=token, status=status)
                self.assertNotEqual(result.returncode, 0)
                self.assertIsNone(output)
                self.assertIn('Sign in first', result.stderr)

    def test_invalid_priority_never_starts_agent(self):
        for priority in ('-1', '20', 'abc', '', '1; echo unsafe'):
            with self.subTest(priority=priority):
                result, output = self.run_entrypoint(nice=priority)
                self.assertNotEqual(result.returncode, 0)
                self.assertIsNone(output)

    def test_documented_registration(self):
        readme = (EXAMPLE / 'README.md').read_text()
        script = readme.split("<<'REGISTER'\n", 1)[1].split('\nREGISTER', 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            tmp = Path(directory)
            gh = tmp / 'gh'
            gh.write_text('#!/bin/sh\necho bot-account\n')
            gh.chmod(0o755)
            curl = tmp / 'curl'
            curl.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$TEST_ARGS"\n'
                            'printf "%s" "$TEST_RESPONSE"\n')
            curl.chmod(0o755)
            env = dict(os.environ, HOME=directory, PATH=directory + ':' + os.environ['PATH'],
                       HIVE_HUB='wss://example.test/contribute', TEST_ARGS=str(tmp / 'args'),
                       TEST_RESPONSE='{"registration_token":"secret-token", "contributor_id":"c1"}')
            script = script.replace('/usr/bin/gh', str(gh))
            result = subprocess.run(['bash', '-c', script], env=env, capture_output=True,
                                    text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertNotIn('secret-token', result.stdout + result.stderr)
            config = tmp / '.config/hive/contributor.env'
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            self.assertIn('CONTRIBUTOR_USERNAME=bot-account', config.read_text())
            args = (tmp / 'args').read_text()
            self.assertIn('https://example.test/api/contribute/register', args)
            self.assertNotIn('Authorization', args)
            config.unlink()
            env['TEST_RESPONSE'] = '{"message":"already registered"}'
            result = subprocess.run(['bash', '-c', script], env=env, capture_output=True,
                                    text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(config.exists())


if __name__ == '__main__':
    unittest.main()
