#!/usr/bin/env python3
"""Exercise the real shell gate without executing installation or media scripts."""
from pathlib import Path
import subprocess
import tempfile
import unittest
import shlex

ROOT = Path(__file__).resolve().parents[4]
SOURCE = (ROOT / '服务器一键离线部署.sh').read_text()
FUNCTION = 'release_integrity_gate() {' + SOURCE.split('release_integrity_gate() {',1)[1].split('\n}\nrelease_integrity_gate',1)[0] + '\n}'

class GateResponseTest(unittest.TestCase):
    def test_only_top_level_verified_json_is_success(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);media=root/'media';trust=root/'trust'
            media.mkdir();trust.mkdir()
            for response,allowed in [('{"status":"VERIFIED"}',True),
                                     ('{"status":"FAILED","nested":{"status":"VERIFIED"}}',False),
                                     ('prefix {"status":"VERIFIED"}',False)]:
                verifier=root/'verifier'
                verifier.write_text('#!/bin/sh\nprintf %s '+shlex.quote(response)+'\n')
                verifier.chmod(0o700)
                script='\n'.join(['set -e', 'die() { exit 9; }', 'info() { :; }',
                    'require_release_integrity=true', 'media_input='+shlex.quote(str(media)),
                    'script_dir='+shlex.quote(str(media)), 'release_verifier='+shlex.quote(str(verifier)),
                    'release_trust_dir='+shlex.quote(str(trust)), FUNCTION, 'release_integrity_gate'])
                result=subprocess.run(['bash','-c',script],capture_output=True,text=True)
                self.assertEqual(allowed,result.returncode==0,response)

if __name__=='__main__':unittest.main()
