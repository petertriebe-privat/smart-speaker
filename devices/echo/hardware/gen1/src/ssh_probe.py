#!/usr/bin/env python
"""Small Python 2/3 compatible remote-development smoke test."""
import hashlib
import os
import sys
assert os.geteuid() == 0
with open(__file__, 'rb') as source:
    digest = hashlib.sha256(source.read()).hexdigest()
sys.stdout.write('ECHO_REMOTE_DEVELOPMENT_OK\nSOURCE_SHA256=' + digest + '\n')
