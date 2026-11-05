#!/usr/bin/env python3
"""Generate local secrets without printing or overwriting them."""
import json
import os
import secrets
from pathlib import Path
root = Path(__file__).resolve().parents[1]
path = root / '.env'
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print('.env already exists; left unchanged')
else:
    with os.fdopen(fd, 'w') as f:
        f.write('POSTGRES_PASSWORD=' + secrets.token_hex(24) + '\n')
        f.write("CF_DEV_TOKENS='" + json.dumps({'console': secrets.token_hex(32), 'importer': secrets.token_hex(32)}) + "'\n")
    print('Created .env with local development credentials (mode 0600)')
