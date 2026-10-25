#!/usr/bin/env python3
"""Validate Go-generated fixtures using the pinned official Harbor ATIF models.

Development/CI only. Requires pydantic==2.12.3 and network access to GitHub raw.
The service and offline Go exporter do not use Python or download schemas.
"""

import argparse
import importlib
from pathlib import Path
import sys
import tempfile
import urllib.request

HARBOR_COMMIT = "0533a59c41ce435d9e59ff8c82da67f6e5b6edc7"
MODULES = (
    "__init__", "agent", "content", "final_metrics", "metrics", "observation",
    "observation_result", "step", "subagent_trajectory_ref", "tool_call", "trajectory",
)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("fixture_directory", type=Path)
    args = parser.parse_args()
    fixtures = sorted(args.fixture_directory.glob("*.json"))
    if not fixtures:
        parser.error("fixture directory contains no JSON files")
    with tempfile.TemporaryDirectory(prefix="atif-harbor-") as temporary:
        root = Path(temporary)
        for package in ("harbor", "harbor/models", "harbor/utils"):
            path = root / package
            path.mkdir(parents=True, exist_ok=True)
            (path / "__init__.py").write_text("")
        paths = [f"harbor/models/trajectories/{name}.py" for name in MODULES]
        paths.append("harbor/utils/trajectory_validator.py")
        for relative in paths:
            url = f"https://raw.githubusercontent.com/harbor-framework/harbor/{HARBOR_COMMIT}/src/{relative}"
            with urllib.request.urlopen(url, timeout=30) as response:
                data = response.read()
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        sys.path.insert(0, temporary)
        validator_type = importlib.import_module("harbor.utils.trajectory_validator").TrajectoryValidator
        for fixture in fixtures:
            validator = validator_type()
            if not validator.validate(fixture):
                print(f"FAIL {fixture.name}: {validator.get_errors()}", file=sys.stderr)
                return 1
            print(f"PASS {fixture.name}")
        print(f"Validated {len(fixtures)} fixtures against Harbor {HARBOR_COMMIT} (ATIF v1.7)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
