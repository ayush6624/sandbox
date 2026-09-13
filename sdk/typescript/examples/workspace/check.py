"""Run the pinned repository's actual suite and emit a machine-checked result."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import xml.etree.ElementTree as ET

root = Path('/home/sandbox/workspace')
repo = root / 'repo'
expected_revision = '096c8d42545d3b68ea21a4f890fb2b2d8979c0bd'
revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
if revision != expected_revision:
    raise RuntimeError(f'unexpected revision: {revision}')
dependencies = subprocess.check_output([str(root / '.venv/bin/python'), '-m', 'pip', 'freeze', '--all'])
if dependencies != (root / 'dependencies.txt').read_bytes():
    raise RuntimeError('installed dependencies differ from prepared workspace')
subprocess.run([str(root / '.venv/bin/python'), '-m', 'pytest', '-q',
                '--junitxml=/home/sandbox/workspace/results.xml'], cwd=repo,
               env={**os.environ, 'PYTEST_DISABLE_PLUGIN_AUTOLOAD': '1'},
               check=True, stdout=sys.stderr, stderr=sys.stderr)
suites = ET.parse(root / 'results.xml').getroot()
totals = {key: sum(int(suite.attrib[key]) for suite in suites.iter('testsuite'))
          for key in ('tests', 'failures', 'errors', 'skipped')}
if totals['tests'] != 297 or totals['failures'] or totals['errors'] or totals['skipped']:
    raise RuntimeError(f'incomplete or failed suite: {totals}')
digest = hashlib.sha256()
for filename in sorted(subprocess.check_output(['git', 'ls-files'], cwd=repo, text=True).splitlines()):
    digest.update(filename.encode() + b'\0' + (repo / filename).read_bytes() + b'\0')
print(json.dumps({'revision': revision, **totals, 'files_sha256': digest.hexdigest(),
                  'dependencies_sha256': hashlib.sha256((root / 'dependencies.txt').read_bytes()).hexdigest(),
                  'workspace_allocated_bytes': int(subprocess.check_output(['du', '-s', '-B1', str(root)]).split()[0])}))
