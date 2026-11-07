#!/usr/bin/env python3
"""Create additive local identity secrets without printing or overwriting them."""
import os
import secrets
from pathlib import Path

path = Path(__file__).resolve().parents[1] / ".env.m1"
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print(".env.m1 already exists. Left unchanged.")
else:
    with os.fdopen(fd, "w") as file:
        file.write("CF_DEV_OPERATOR_TOKEN=" + secrets.token_hex(32) + "\n")
        file.write("CF_KRATOS_POSTGRES_PASSWORD=" + secrets.token_hex(24) + "\n")
        file.write("CF_KRATOS_COOKIE_SECRET=" + secrets.token_hex(32) + "\n")
        file.write("CF_KRATOS_CIPHER_SECRET=" + secrets.token_hex(16) + "\n")
    print("Created .env.m1 with local identity credentials (mode 0600).")
