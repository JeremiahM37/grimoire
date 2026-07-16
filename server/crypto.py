"""Authenticated encryption for the secret vault.

Key derivation: PBKDF2-HMAC-SHA256 (high iteration count) from the passphrase +
a per-vault random salt. Encryption: Fernet (AES-128-CBC + HMAC-SHA256). The
passphrase is never stored; the derived key lives only in memory while unlocked.
"""
import base64
import os

from cryptography.fernet import Fernet, InvalidToken
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC

ITERATIONS = 240_000


def new_salt() -> bytes:
    return os.urandom(16)


def derive_key(passphrase: str, salt: bytes) -> bytes:
    kdf = PBKDF2HMAC(algorithm=hashes.SHA256(), length=32, salt=salt,
                     iterations=ITERATIONS)
    return base64.urlsafe_b64encode(kdf.derive(passphrase.encode("utf-8")))


def seal(key: bytes, plaintext: bytes) -> bytes:
    return Fernet(key).encrypt(plaintext)


def unseal(key: bytes, token: bytes) -> bytes:
    try:
        return Fernet(key).decrypt(token)
    except InvalidToken as e:
        raise ValueError("wrong passphrase or corrupted data") from e
