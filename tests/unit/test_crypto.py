from server import crypto


def test_seal_unseal_roundtrip():
    key = crypto.derive_key("correct horse battery", crypto.new_salt())
    ct = crypto.seal(key, b"a secret token")
    assert ct != b"a secret token"          # actually encrypted
    assert crypto.unseal(key, ct) == b"a secret token"


def test_wrong_key_fails():
    salt = crypto.new_salt()
    k1 = crypto.derive_key("passphrase one", salt)
    k2 = crypto.derive_key("passphrase two", salt)
    ct = crypto.seal(k1, b"data")
    import pytest
    with pytest.raises(ValueError):
        crypto.unseal(k2, ct)


def test_same_passphrase_different_salt_differs():
    s1, s2 = crypto.new_salt(), crypto.new_salt()
    assert crypto.derive_key("p", s1) != crypto.derive_key("p", s2)


def test_derivation_is_stable():
    salt = crypto.new_salt()
    assert crypto.derive_key("p", salt) == crypto.derive_key("p", salt)
