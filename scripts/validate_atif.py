#!/usr/bin/env python3
"""Validate generated synthetic fixtures with pinned official Harbor ATIF models.

Development/CI only. Requires pydantic==2.12.3 and network access to GitHub raw.
Only use controlled synthetic fixture directories, never private event histories.
The service and offline Go exporter do not use Python or download schemas.
Remote image URLs are never fetched; local references are checked by Harbor.
"""

import argparse
import hashlib
import importlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import sys
import tempfile
import urllib.request
import zipfile

HARBOR_COMMIT = "0533a59c41ce435d9e59ff8c82da67f6e5b6edc7"
MODULES = (
    "__init__", "agent", "content", "final_metrics", "metrics", "observation",
    "observation_result", "step", "subagent_trajectory_ref", "tool_call", "trajectory",
)
IMAGE_KEY = re.compile(r"images/([a-f0-9]{64})\.(png|jpg|gif|webp)\Z")
MAX_IMAGE_BYTES = 16 << 20
MAX_BUNDLE_BYTES = 512 << 20
MAX_BUNDLE_ENTRIES = 4096


def synthetic_fixtures(directory: Path) -> list[Path]:
    """Recurse without following symlinks into unrelated local data."""
    fixtures = []
    for parent, directories, files in os.walk(directory, followlinks=False):
        for name in directories + files:
            path = Path(parent) / name
            if path.is_symlink():
                raise ValueError(f"fixture tree contains a symlink: {path.name}")
        for name in files:
            path = Path(parent) / name
            if path.suffix in (".json", ".zip"):
                if not path.is_file():
                    raise ValueError(f"fixture is not a regular file: {path.name}")
                fixtures.append(path)
    return sorted(fixtures)


def safe_extract_zip(fixture: Path, destination: Path) -> Path:
    """Extract only canonical export members into a fresh private directory."""
    with zipfile.ZipFile(fixture) as archive:
        members = archive.infolist()
        if not members or len(members) > MAX_BUNDLE_ENTRIES:
            raise ValueError("invalid ZIP member count")
        seen = set()
        total = 0
        for member in members:
            name = member.filename
            components = name.split("/")
            mode = member.external_attr >> 16
            file_type = stat.S_IFMT(mode)
            if (PurePosixPath(name).is_absolute() or "\\" in name
                    or any(part in ("", ".", "..") for part in components)
                    or name != member.orig_filename or ":" in name
                    or name in seen or member.is_dir()
                    or file_type not in (0, stat.S_IFREG)
                    or member.flag_bits & 1):
                raise ValueError("unsafe, duplicate, or non-regular ZIP member")
            if name != "trajectory.json" and IMAGE_KEY.fullmatch(name) is None:
                raise ValueError("ZIP contains an unrelated member")
            limit = MAX_BUNDLE_BYTES if name == "trajectory.json" else MAX_IMAGE_BYTES
            if member.file_size < 0 or member.file_size > limit:
                raise ValueError("ZIP member exceeds size limit")
            total += member.file_size
            if total > MAX_BUNDLE_BYTES:
                raise ValueError("ZIP exceeds total size limit")
            seen.add(name)
        if "trajectory.json" not in seen:
            raise ValueError("ZIP has no trajectory.json")
        for member in members:
            with archive.open(member) as source:
                data = source.read(member.file_size + 1)
            if len(data) != member.file_size:
                raise ValueError("ZIP member size mismatch")
            match = IMAGE_KEY.fullmatch(member.filename)
            if match and hashlib.sha256(data).hexdigest() != match.group(1):
                raise ValueError("ZIP image hash mismatch")
            target = destination.joinpath(*member.filename.split("/"))
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            with target.open("xb") as output:
                os.chmod(target, 0o600)
                output.write(data)
    return destination / "trajectory.json"


def check_local_image_scope(fixture: Path) -> None:
    """Reject local image references outside the synthetic fixture directory."""
    data = json.loads(fixture.read_text())
    contents = []
    for step in data.get("steps", []):
        contents.append(step.get("message"))
        for result in (step.get("observation") or {}).get("results", []):
            contents.append(result.get("content"))
    for content in contents:
        if not isinstance(content, list):
            continue
        for part in content:
            if not isinstance(part, dict) or part.get("type") != "image":
                continue
            location = (part.get("source") or {}).get("path")
            if not isinstance(location, str) or "://" in location:
                continue  # Harbor validates structure; remote references are not fetched.
            if (PurePosixPath(location).is_absolute() or "\\" in location
                    or ":" in location
                    or any(p in ("", ".", "..") for p in location.split("/"))):
                raise ValueError("unsafe local image reference")
            current = fixture.parent
            for component in location.split("/"):
                current = current / component
                if current.is_symlink():
                    raise ValueError("local image reference traverses a symlink")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("fixture_directory", type=Path)
    args = parser.parse_args()
    if args.fixture_directory.is_symlink() or not args.fixture_directory.is_dir():
        parser.error("fixture directory must be a real directory")
    directory = args.fixture_directory.resolve()
    try:
        fixtures = synthetic_fixtures(directory)
    except ValueError as error:
        parser.error(str(error))
    if not fixtures:
        parser.error("fixture directory contains no JSON or ZIP files")
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
        for index, fixture in enumerate(fixtures):
            label = fixture.relative_to(directory)
            try:
                if fixture.suffix == ".zip":
                    extracted = root / "extracted" / str(index)
                    extracted.mkdir(mode=0o700, parents=True)
                    target = safe_extract_zip(fixture, extracted)
                else:
                    target = fixture
                check_local_image_scope(target)
                validator = validator_type()
                if not validator.validate(target, validate_images=True):
                    print(f"FAIL {label}: {validator.get_errors()}", file=sys.stderr)
                    return 1
            except (OSError, ValueError, zipfile.BadZipFile, RuntimeError) as error:
                print(f"FAIL {label}: {error}", file=sys.stderr)
                return 1
            print(f"PASS {label}")
        print(f"Validated {len(fixtures)} JSON/ZIP fixtures against Harbor {HARBOR_COMMIT} (ATIF v1.7)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
