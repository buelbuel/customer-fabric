#!/usr/bin/env python3
"""Return the Compose project for this checkout without reading credentials."""
from hashlib import sha256
from pathlib import Path
from subprocess import check_output

root = Path(__file__).resolve().parents[1]
git_dir, common_dir = check_output(
    ["git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"],
    cwd=root,
    text=True,
).splitlines()

if Path(git_dir).resolve() == Path(common_dir).resolve():
    print("customer-fabric")
else:
    suffix = sha256(str(root).encode()).hexdigest()[:20]
    print(f"customer-fabric-wt-{suffix}")
