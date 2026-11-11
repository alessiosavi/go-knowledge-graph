#!/bin/sh
# Certbot already supplies Python and cryptography; never print key material.
set -eu
exec python - "$@" <<'PY'
import os
import re
import sys
import tempfile
from pathlib import Path
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization

def install():
    if len(sys.argv) != 2 or not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?", sys.argv[1]):
        raise ValueError()
    live = Path('/etc/letsencrypt/live') / sys.argv[1]
    dest = Path('/etc/nginx/edge-certs')
    chain = live / 'fullchain.pem'
    if not chain.exists():
        print('unchanged')
        return
    # Root can read mode 000; reject that explicitly as a damaged lineage.
    key_path = live / 'privkey.pem'
    if not key_path.stat().st_mode & 0o444:
        raise ValueError()
    cert_bytes, key_bytes = chain.read_bytes(), key_path.read_bytes()
    cert = x509.load_pem_x509_certificate(cert_bytes)
    key = serialization.load_pem_private_key(key_bytes, password=None)
    def public(k):
        return k.public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
    if public(cert.public_key()) != public(key.public_key()):
        raise ValueError()
    current = dest / 'current.pem'
    if current.exists() and current.stat().st_size:
        old_bytes = current.read_bytes()
        old = x509.load_pem_x509_certificate(old_bytes)
        old_key = serialization.load_pem_private_key(old_bytes, password=None)
        if old.fingerprint(hashes.SHA256()) == cert.fingerprint(hashes.SHA256()) and public(old_key.public_key()) == public(key.public_key()):
            print('unchanged')
            return
    pending = None
    try:
        fd, pending = tempfile.mkstemp(prefix='.install-', dir=dest)
        with os.fdopen(fd, 'wb') as f:
            f.write(cert_bytes)
            if not cert_bytes.endswith(b'\n'):
                f.write(b'\n')
            f.write(key_bytes)
            os.fchown(f.fileno(), 0, 101)
            os.fchmod(f.fileno(), 0o640)
            f.flush()
            os.fsync(f.fileno())
        # nginx opens this same generation for both its chain and private key.
        os.replace(pending, current)
    finally:
        if pending is not None and os.path.exists(pending):
            os.unlink(pending)
    print('installed')

try:
    install()
except Exception:
    print('edge: certificate install failed', file=sys.stderr)
    sys.exit(1)
PY
