#!/usr/bin/env bash
set -euo pipefail

cd /home/sandbox/workspace
git init -q repo
git -C repo remote add origin https://github.com/pallets/itsdangerous.git
git -C repo fetch -q --depth 1 origin 096c8d42545d3b68ea21a4f890fb2b2d8979c0bd
git -C repo checkout -q --detach FETCH_HEAD
test "$(git -C repo rev-parse HEAD)" = 096c8d42545d3b68ea21a4f890fb2b2d8979c0bd
python3 -m venv .venv
.venv/bin/python -m pip install --disable-pip-version-check \
  pip==25.0.1 flit_core==3.10.1 -r repo/requirements/tests.txt
.venv/bin/python -m pip install --no-deps --no-build-isolation -e ./repo
.venv/bin/python -m pip check
.venv/bin/python -m pip freeze --all > dependencies.txt
