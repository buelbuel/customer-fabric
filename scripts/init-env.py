#!/usr/bin/env python3
"""Create a stack's ignored env file with random secrets, never printing or overwriting it."""
import json
import os
import secrets
import sys
from pathlib import Path


def core():
    tokens = json.dumps({"console": secrets.token_hex(32), "importer": secrets.token_hex(32)})
    return f"POSTGRES_PASSWORD={secrets.token_hex(24)}\nCF_DEV_TOKENS='{tokens}'\n"


def identity():
    return (
        f"CF_DEV_OPERATOR_TOKEN={secrets.token_hex(32)}\n"
        f"CF_KRATOS_POSTGRES_PASSWORD={secrets.token_hex(24)}\n"
        f"CF_KRATOS_COOKIE_SECRET={secrets.token_hex(32)}\n"
        f"CF_KRATOS_CIPHER_SECRET={secrets.token_hex(16)}\n"
    )


def linking():
    issuers = json.dumps({"importer": secrets.token_hex(32)}, separators=(",", ":"))
    return f"CF_DEV_LINKING_ISSUERS='{issuers}'\n"


def operator():
    sizes = (("POSTGRES_PASSWORD", 24), ("COOKIE_SECRET", 32), ("CIPHER_SECRET", 16))
    return "".join(f"CF_OPERATOR_{name}={secrets.token_hex(size)}\n" for name, size in sizes)


def oidc():
    return f"CF_HYDRA_POSTGRES_PASSWORD={secrets.token_hex(24)}\nCF_HYDRA_SYSTEM_SECRET={secrets.token_hex(32)}\n"


STACKS = {
    "core": (".env", core),
    "identity": (".env.m1", identity),
    "linking": (".env.linking", linking),
    "operator": (".env.operator", operator),
    "oidc": (".env.oidc", oidc),
}

if len(sys.argv) != 2 or sys.argv[1] not in STACKS:
    raise SystemExit("Usage: init-env.py " + "|".join(STACKS))
name, contents = STACKS[sys.argv[1]]
path = Path(__file__).resolve().parents[1] / name
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print(f"{name} already exists; left unchanged")
else:
    with os.fdopen(fd, "w") as file:
        file.write(contents())
    print(f"Created {name} with local development credentials (mode 0600)")
