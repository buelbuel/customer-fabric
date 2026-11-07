#!/usr/bin/env python3
"""Initialize the separate local operator database and identity secrets."""
import os
import secrets
from pathlib import Path

path = Path(__file__).resolve().parents[1] / ".env.operator"
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print(".env.operator already exists. Left unchanged.")
else:
    with os.fdopen(fd, "w") as file:
        for name, size in (("POSTGRES_PASSWORD", 24), ("COOKIE_SECRET", 32), ("CIPHER_SECRET", 16)):
            file.write(f"CF_OPERATOR_{name}={secrets.token_hex(size)}\n")
    print("Created .env.operator with local identity credentials (mode 0600).")
