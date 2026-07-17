"""Authenticated encryption for the secret vault.

Key derivation: **Argon2id** (memory-hard, GPU/ASIC-resistant) from the passphrase
+ a per-vault random salt. Legacy vaults created with PBKDF2-HMAC-SHA256 still
unlock (the KDF is recorded per-vault). Encryption: Fernet (AES-128-CBC +
HMAC-SHA256). The passphrase is never stored; the derived key lives only in
memory while unlocked.
"""
import base64
import os

from argon2.low_level import Type, hash_secret_raw
from cryptography.fernet import Fernet, InvalidToken
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC

# legacy PBKDF2 (only used to unlock pre-existing vaults)
ITERATIONS = 240_000

# Argon2id parameters (OWASP-aligned: 64 MiB, t=3, p=4)
ARGON_TIME = 3
ARGON_MEMORY_KIB = 64 * 1024
ARGON_PARALLELISM = 4

DEFAULT_KDF = "argon2id"


def new_salt() -> bytes:
    return os.urandom(16)


def derive_key(passphrase: str, salt: bytes, kdf: str = DEFAULT_KDF) -> bytes:
    """Derive a 32-byte Fernet key. `kdf` selects the algorithm so old vaults
    (pbkdf2) keep working while new ones use argon2id."""
    pw = passphrase.encode("utf-8")
    if kdf == "argon2id":
        raw = hash_secret_raw(pw, salt, time_cost=ARGON_TIME,
                              memory_cost=ARGON_MEMORY_KIB,
                              parallelism=ARGON_PARALLELISM, hash_len=32, type=Type.ID)
    elif kdf == "pbkdf2":
        raw = PBKDF2HMAC(algorithm=hashes.SHA256(), length=32, salt=salt,
                         iterations=ITERATIONS).derive(pw)
    else:
        raise ValueError(f"unknown kdf: {kdf!r}")
    return base64.urlsafe_b64encode(raw)


def seal(key: bytes, plaintext: bytes) -> bytes:
    return Fernet(key).encrypt(plaintext)


def unseal(key: bytes, token: bytes) -> bytes:
    try:
        return Fernet(key).decrypt(token)
    except InvalidToken as e:
        raise ValueError("wrong passphrase or corrupted data") from e
