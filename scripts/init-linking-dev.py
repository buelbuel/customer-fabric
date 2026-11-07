#!/usr/bin/env python3
"""Enable the synthetic linking authority without touching existing credentials."""
import json
import os
import secrets
from pathlib import Path

path = Path(__file__).resolve().parents[1] / ".env.linking"
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print(".env.linking already exists. Left unchanged.")
else:
    with os.fdopen(fd, "w") as file:
        issuers = json.dumps({"importer": secrets.token_hex(32)}, separators=(",", ":"))
        file.write(f"CF_DEV_LINKING_ISSUERS='{issuers}'\n")
    print("Created .env.linking with a separate synthetic issuer credential (mode 0600).")
