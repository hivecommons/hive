#!/bin/bash
# Isolated regression tests: UNIX socket fixtures and command stubs, no live tmux.
set -euo pipefail
export ATTACH_SCRIPT="$(cd "$(dirname "$0")" && pwd)/ttyd-tmux.sh"
python3 - <<'PY'
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import unittest


class SocketDiscovery(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="ttyd sockets ")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.log = self.root / "calls"
        self.sessions = {}
        self.sockets = []
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}",
                        TMUX_TMPDIR=str(self.root), HIVE_TTYD_WAIT_SEC="0",
                        CALL_LOG=str(self.log), CURRENT_UID="1000840000",
                        DEV_UID="1001", OWNER_UID="1000840000")
        self.stub("id", '''import os, sys
if sys.argv[1:] == ['-u', 'dev']:
    if os.environ['DEV_UID'] == 'missing':
        sys.exit(1)
    print(os.environ['DEV_UID'])
else:
    print(os.environ['CURRENT_UID'])
''')
        self.stub("stat", '''import os, sys
print(os.environ['OWNER_UID'] if sys.argv[2] == '%u' else '2000')
''')
        self.stub("su-exec", '''import os, sys
with open(os.environ['CALL_LOG'], 'a') as f:
    f.write('su-exec ' + sys.argv[1] + '\\n')
os.execvp(sys.argv[2], sys.argv[2:])
''')
        self.stub("tmux", '''import json, os, sys
args = sys.argv[1:]
with open(os.environ['CALL_LOG'], 'a') as f:
    f.write(json.dumps(args) + '\\n')
sessions = json.loads(os.environ['SESSIONS']).get(args[1], [])
if args[2] == 'has-session':
    sys.exit(0 if args[4] in sessions else 1)
if args[2] == 'list-sessions':
    print('\\n'.join(sessions))
''')

    def stub(self, name, code):
        path = self.bin / name
        path.write_text("#!/usr/bin/env python3\n" + code)
        path.chmod(0o755)

    def server(self, uid, sessions, name="default"):
        path = self.root / f"tmux-{uid}" / name
        path.parent.mkdir(exist_ok=True)
        sock = socket.socket(socket.AF_UNIX)
        sock.bind(str(path))
        self.addCleanup(sock.close)
        self.sockets.append(sock)
        self.sessions[str(path)] = sessions
        return str(path)

    def run_attach(self, session="hive-target"):
        self.env['SESSIONS'] = json.dumps(self.sessions)
        args = [] if session is None else [session]
        result = subprocess.run(['bash', os.environ['ATTACH_SCRIPT'], *args],
                                env=self.env, capture_output=True, text=True,
                                timeout=10)
        calls = self.log.read_text().splitlines() if self.log.exists() else []
        self.calls = [json.loads(c) for c in calls if c.startswith('[')]
        self.su_calls = [c for c in calls if c.startswith('su-exec ')]
        return result

    def assert_attaches(self, expected):
        result = self.run_attach()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([c[1] for c in self.calls if c[2] == 'attach-session'],
                         [expected])
        self.assertNotIn('waiting for agent', result.stderr)

    def test_arbitrary_current_uid_precedes_passwd_and_glob(self):
        self.server('1001', ['hive-target'])
        expected = self.server('1000840000', ['hive-target'])
        self.server('42', ['hive-target'])
        self.assert_attaches(expected)

    def test_skip_default_without_requested_session(self):
        self.server('1000840000', ['hive-other'])
        expected = self.server('1001', ['hive-target'])
        self.assert_attaches(expected)

    def test_other_uid_fallback_uses_su_exec(self):
        self.server('1000840000', [])
        expected = self.server('42', ['hive-target'])
        self.env['OWNER_UID'] = '42'
        self.assert_attaches(expected)
        self.assertEqual(self.su_calls, ['su-exec 42:2000'] * len(self.calls))

    def test_missing_dev_passwd_entry_falls_back_to_1001(self):
        self.env['DEV_UID'] = 'missing'
        expected = self.server('1001', ['hive-target'])
        self.assert_attaches(expected)

    def test_deduplicate_and_list_actual_sessions_on_failure(self):
        self.env['DEV_UID'] = self.env['CURRENT_UID']
        first = self.server('1000840000', ['hive-other'])
        second = self.server('42', ['hive-another'])
        result = self.run_attach()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('available sessions: hive-other hive-another', result.stderr)
        self.assertEqual([c[1] for c in self.calls if c[2] == 'has-session'],
                         [first, second])
        self.assertEqual([c[1] for c in self.calls if c[2] == 'list-sessions'],
                         [first, second])
        self.assertFalse(any(c[2] == 'attach-session' for c in self.calls))

    def test_non_socket_candidate_is_skipped(self):
        path = self.root / 'tmux-1000840000' / 'default'
        path.parent.mkdir()
        path.write_text('stale file')
        expected = self.server('1001', ['hive-target'])
        self.assert_attaches(expected)
        self.assertFalse(any(c[1] == str(path) for c in self.calls))

    def test_per_agent_socket_retains_priority(self):
        self.server('1000840000', ['hive-target'])
        expected = self.server('42', ['hive-target'], 'hive-target')
        self.assert_attaches(expected)

    def test_no_argument_diagnostics_use_owner_for_other_default(self):
        self.server('42', ['hive-other'])
        self.env['OWNER_UID'] = '42'
        result = self.run_attach(None)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('available sessions: hive-other', result.stderr)
        self.assertEqual(self.su_calls, ['su-exec 42:2000'])


unittest.main()
PY
