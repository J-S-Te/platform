#!/usr/bin/env python3
"""uip-credential/v1 封套辅助工具测试。

仅使用本地测试密钥与固定向量；不访问运行中的平台、不读写运行配置。
"""
import base64
import hashlib
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "scripts" / "lib" / "credential-envelope.py"
SPEC = importlib.util.spec_from_file_location("credential_envelope", HELPER)
ce = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ce)

# 仅用于本地测试的 RSA-2048 密钥对，禁止用于任何运行环境。
TEST_PRIVATE_PEM = """-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAwtcVdHoUiOKWXzKIE17JEH53B1GNTFVcnnDGK/q7Vh6qqWM6
z0wU7NzcUASWg3klYLxMFtPOtQGD1jHU0Wl+/h00Foksa2NTLMiq5eLGcDUR6oA6
28ZYCDG+ZssTgiRowuNVxBgi2Qcd/x6Zyjz6fW/sKkgUK856sTkbRX2jBDK/G25r
j/s394aVO0yOdDtyhcyFy6FZ+zdalGU4zUV4rwoy57vO24WsaHE+KzUhjPhrstnq
JUQRdKUBnvoHnfBdNcTAFfDSkoOqDtrGDpMmbJKBANCngletuCe9wsIcU1RM55NW
JrFjxTNJ0MixSJ4nflXXYWKzC4Wu+1ru85j+6wIDAQABAoIBAGEKgPwXRZz7xcs6
Pr3vCpN1eVEi1A49wbiEEJ+c7Ksy8OsWqhbPHOjwA6ay8kgfEXfBsN/5tE1AqOoI
vadjNMXZw7Sps3705Fs8wqlmrlD6jabPIQGPIw9JvzwxTtEV0zulwHS/VLPlMJJ9
Kjmo71WEZgqVBb7fwwn7mFKfBY8z3K4XlMToDs6Svm3fQS57FMhTqzJYx6xQc6P+
BwCmJNMQ4e8KGty9e3JX6QMkxRi5TxMTJ/bA67ML6lbihNycl3N8YekUXHQn/Qji
Uzb8GwcDtWJv4lI2aLnBq9fiVK6K4n6WMRjloc0F4whdp0FTL8A/fv83ch5t8Pi2
G0ujTNECgYEA4roBV6FV/jmYjuC8lG5jNK3wsqmJBKQv+eZ4v+CTTuzPBNBPTUqr
1tF4BR3/b708u1l7sCQeymMVuRGhA5tDsIoYKLIvJtY6KbhbpwOAj9PcaQuBG15F
jvyr8j1qo0I0fR15n/l+nPQ4YMXCpVof6GLDvY9ozcaU/BmBL3OYHHMCgYEA2/8j
CL6R3xy+/7C4F7fWUWh0Y3VC5Yfnn0MhLTo8zl8jcot/mLQeIvwc27J6GzVRq4/Z
0o6v8b74G8lU0FknknuJKXG/AgbenegW22VVgPcCLqeJ2HebizTHWUjjJIN8JVQH
MG5R7lTgZqEfmguKLsyFLIEWwTtvJYpOu46aLakCgYEAqTAWWlcZfZlrP4tzDeyF
+USnbhtXFPCUoI4co0qpgmbyYOw1kT7clnQ/dMoa2D5FuKSgySNbGrnv9uex2Kgf
Tfju6Ed3KisIwiNBuZLCeBGn03dzQ4IItBfXE/BCyEtaMdcx8G/UzLjt7UEF7DAh
yV8SJD2E/rFmce1yWn+PDgMCgYB4GOtGPcqiUy4NpaqKACj2niBTn1MavLt8XQhC
iXVib9uJD5Bji4icLqeOY/t22P+xucD+yjhQr6EAgkNTziflNp1lS5wrfhaPe4Dm
2oX39mBxmbKNwpXeQ7zECB8/8quWMS4PiVoe0qUCQdlfngccjzfwWLRUXp7Jjpx2
lV2jqQKBgHgGxRvP5tMocDUHHX1bSllLfvlaGdqFJ5mIN+ksTEwH8nx2bcq5HyxK
PO1f1tUU+XsadUvm6Jk5xg9K4amiI2qlvot+gzOhx1dwTN8zrvUA/uPXOVkjhL2A
anshfmC1lOVo3Ol/JKUNmQd4MYswgGx984R2eCj6njcD2mQV2Nk9
-----END RSA PRIVATE KEY-----"""

TEST_PUBLIC_PEM = """-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAwtcVdHoUiOKWXzKIE17J
EH53B1GNTFVcnnDGK/q7Vh6qqWM6z0wU7NzcUASWg3klYLxMFtPOtQGD1jHU0Wl+
/h00Foksa2NTLMiq5eLGcDUR6oA628ZYCDG+ZssTgiRowuNVxBgi2Qcd/x6Zyjz6
fW/sKkgUK856sTkbRX2jBDK/G25rj/s394aVO0yOdDtyhcyFy6FZ+zdalGU4zUV4
rwoy57vO24WsaHE+KzUhjPhrstnqJUQRdKUBnvoHnfBdNcTAFfDSkoOqDtrGDpMm
bJKBANCngletuCe9wsIcU1RM55NWJrFjxTNJ0MixSJ4nflXXYWKzC4Wu+1ru85j+
6wIDAQAB
-----END PUBLIC KEY-----"""

PROTOCOL = "uip-credential/v1"
CONTENT_TYPE = "application/json"


def private_numbers():
    der = ce.pem_to_der(TEST_PRIVATE_PEM)
    tag, sequence, _ = ce.der_tlv(der, 0)
    assert tag == 0x30
    tag, _version, offset = ce.der_tlv(sequence, 0)
    tag, modulus, offset = ce.der_tlv(sequence, offset)
    tag, exponent, offset = ce.der_tlv(sequence, offset)
    tag, private_exponent, _ = ce.der_tlv(sequence, offset)
    return int.from_bytes(modulus, "big"), int.from_bytes(exponent, "big"), int.from_bytes(private_exponent, "big")


def rsa_oaep_decrypt(ciphertext, message_bytes):
    modulus, _exponent, private_exponent = private_numbers()
    size = (modulus.bit_length() + 7) // 8
    encoded = pow(int.from_bytes(ciphertext, "big"), private_exponent, modulus).to_bytes(size, "big")
    h_len = ce.SHA256_SIZE
    masked_seed, masked_db = encoded[1:1 + h_len], encoded[1 + h_len:]
    seed = ce.xor_bytes(masked_seed, ce.mgf1(masked_db, h_len))
    data_block = ce.xor_bytes(masked_db, ce.mgf1(seed, size - h_len - 1))
    if data_block[:h_len] != hashlib.sha256(ce.OAEP_LABEL).digest():
        raise AssertionError("oaep label hash mismatch")
    remainder = data_block[h_len:]
    index = remainder.index(b"\x01")
    if any(byte != 0 for byte in remainder[:index]):
        raise AssertionError("oaep padding invalid")
    return remainder[index + 1:]


def credential_challenge(purpose="LOGIN", resource_id=""):
    return {
        "data": {
            "protocol": PROTOCOL,
            "kid": "test-kid",
            "public_key": TEST_PUBLIC_PEM,
            "challenge_id": "test-challenge-id",
            "purpose": purpose,
            "resource_id": resource_id,
            "expires_at": "2026-10-27T00:00:00Z",
        }
    }


class CredentialEnvelopeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def write(self, name, payload):
        path = self.dir / name
        path.write_text(payload if isinstance(payload, str) else json.dumps(payload), encoding="utf-8")
        return path

    def run_helper(self, *arguments):
        return subprocess.run(
            [sys.executable, str(HELPER)] + [str(argument) for argument in arguments],
            capture_output=True, text=True,
        )

    def test_aes_256_fips_197_block_vector(self):
        key = bytes.fromhex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
        plaintext = bytes.fromhex("00112233445566778899aabbccddeeff")
        expected = "8ea2b7ca516745bfeafc49904b496089"
        self.assertEqual(ce.aes_encrypt_block(ce.aes_expand_key(key), plaintext).hex(), expected)

    def test_aes_gcm_round_trip_and_tamper_rejection(self):
        key, iv, aad = bytes(range(32)), bytes(range(12)), b"uip-credential/v1\nrequest"
        plaintext = b'{"account":"admin"}'
        ciphertext, tag = ce.gcm_seal(key, iv, plaintext, aad)
        self.assertEqual(ce.gcm_open(key, iv, ciphertext, tag, aad), plaintext)
        with self.assertRaises(ce.EnvelopeError):
            ce.gcm_open(key, iv, ciphertext, tag, aad + b"x")
        tampered = bytes([ciphertext[0] ^ 1]) + ciphertext[1:]
        with self.assertRaises(ce.EnvelopeError):
            ce.gcm_open(key, iv, tampered, tag, aad)

    def test_rsa_oaep_labeled_key_wrap(self):
        material = os.urandom(ce.KEY_MATERIAL_SIZE)
        encrypted = ce.rsa_oaep_encrypt(TEST_PUBLIC_PEM, material)
        self.assertEqual(len(encrypted), 256)
        self.assertEqual(rsa_oaep_decrypt(encrypted, material), material)

    def test_cli_encrypt_produces_server_compatible_envelope(self):
        challenge = self.write("challenge.json", credential_challenge())
        plaintext = self.write("plaintext.json", '{"account":"admin","password":"fixture","login_type":"password","replace_existing_session":false}')
        request_out = self.dir / "request.json"
        response_key_out = self.dir / "response-key.txt"
        meta_out = self.dir / "meta.json"
        result = self.run_helper(
            "encrypt", "--challenge", challenge, "--plaintext", plaintext,
            "--purpose", "LOGIN", "--resource-id", "", "--content-type", CONTENT_TYPE,
            "--request-out", request_out, "--response-key-out", response_key_out, "--meta-out", meta_out,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        request = json.loads(request_out.read_text(encoding="utf-8"))
        envelope = request["credential_envelope"]
        self.assertEqual(envelope["protocol"], PROTOCOL)
        self.assertEqual(envelope["kid"], "test-kid")
        self.assertEqual(envelope["challenge_id"], "test-challenge-id")
        self.assertEqual(envelope["purpose"], "LOGIN")
        self.assertEqual(envelope["content_type"], CONTENT_TYPE)
        material = rsa_oaep_decrypt(ce.b64u_decode(envelope["encrypted_key"], "encrypted_key"), b"")
        self.assertEqual(len(material), ce.KEY_MATERIAL_SIZE)
        aad = ce.build_aad("request", "test-kid", "test-challenge-id", "LOGIN", "", CONTENT_TYPE)
        decrypted = ce.gcm_open(
            material[:32], ce.b64u_decode(envelope["iv"], "iv"),
            ce.b64u_decode(envelope["ciphertext"], "ciphertext"),
            ce.b64u_decode(envelope["tag"], "tag"), aad,
        )
        self.assertEqual(decrypted.decode("utf-8"), plaintext.read_text(encoding="utf-8"))
        self.assertEqual(len(ce.b64u_decode(response_key_out.read_text(encoding="ascii").strip(), "response_key")), 32)
        self.assertEqual(json.loads(meta_out.read_text(encoding="utf-8"))["kid"], "test-kid")
        for path in (request_out, response_key_out, meta_out):
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600, path.name)

    def test_cli_decrypt_sealed_response_and_plaintext_passthrough(self):
        response_key = os.urandom(32)
        response_key_file = self.dir / "response-key.txt"
        response_key_file.write_text(ce.b64u_encode(response_key), encoding="ascii")
        meta = self.write("meta.json", {
            "kid": "test-kid", "challenge_id": "test-challenge-id",
            "purpose": "LOGIN", "resource_id": "", "content_type": CONTENT_TYPE,
        })
        body = json.dumps({"data": {"must_change_password": False}}, separators=(",", ":"))
        iv = os.urandom(12)
        aad = ce.build_aad("response", "test-kid", "test-challenge-id", "LOGIN", "", CONTENT_TYPE)
        ciphertext, tag = ce.gcm_seal(response_key, iv, body.encode("utf-8"), aad)
        sealed = self.write("sealed-response.json", {
            "credential_envelope": {
                "protocol": PROTOCOL, "kid": "test-kid", "challenge_id": "test-challenge-id",
                "purpose": "LOGIN", "resource_id": "", "content_type": CONTENT_TYPE,
                "iv": ce.b64u_encode(iv), "ciphertext": ce.b64u_encode(ciphertext), "tag": ce.b64u_encode(tag),
            }
        })
        output = self.dir / "decrypted.json"
        result = self.run_helper("decrypt", "--response", sealed, "--response-key", response_key_file,
                                 "--meta-file", meta, "--output", output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(output.read_text(encoding="utf-8")), {"data": {"must_change_password": False}})

        plain = self.write("plain-response.json", '{"code":"AUTH_INVALID","message":"no"}')
        output_plain = self.dir / "plain-output.json"
        result = self.run_helper("decrypt", "--response", plain, "--response-key", response_key_file,
                                 "--meta-file", meta, "--output", output_plain)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output_plain.read_text(encoding="utf-8"), plain.read_text(encoding="utf-8"))

    def test_cli_encrypt_rejects_challenge_mismatch(self):
        challenge = self.write("challenge.json", credential_challenge(purpose="CHANGE_PASSWORD"))
        plaintext = self.write("plaintext.json", "{}")
        result = self.run_helper(
            "encrypt", "--challenge", challenge, "--plaintext", plaintext,
            "--purpose", "LOGIN", "--resource-id", "", "--content-type", CONTENT_TYPE,
            "--request-out", self.dir / "request.json", "--response-key-out", self.dir / "key.txt",
            "--meta-out", self.dir / "meta.json",
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("CREDENTIAL_ENVELOPE_INVALID", result.stderr)

    def test_cli_decrypt_rejects_metadata_or_ciphertext_tampering(self):
        response_key = os.urandom(32)
        response_key_file = self.dir / "response-key.txt"
        response_key_file.write_text(ce.b64u_encode(response_key), encoding="ascii")
        meta = self.write("meta.json", {
            "kid": "test-kid", "challenge_id": "test-challenge-id",
            "purpose": "LOGIN", "resource_id": "", "content_type": CONTENT_TYPE,
        })
        body = b'{"data":{}}'
        iv = os.urandom(12)
        aad = ce.build_aad("response", "test-kid", "test-challenge-id", "LOGIN", "", CONTENT_TYPE)
        ciphertext, tag = ce.gcm_seal(response_key, iv, body, aad)
        wrong = self.write("wrong.json", {
            "credential_envelope": {
                "protocol": PROTOCOL, "kid": "test-kid", "challenge_id": "other-challenge",
                "purpose": "LOGIN", "resource_id": "", "content_type": CONTENT_TYPE,
                "iv": ce.b64u_encode(iv), "ciphertext": ce.b64u_encode(ciphertext), "tag": ce.b64u_encode(tag),
            }
        })
        result = self.run_helper("decrypt", "--response", wrong, "--response-key", response_key_file,
                                 "--meta-file", meta, "--output", self.dir / "out.json")
        self.assertEqual(result.returncode, 2)
        self.assertIn("CREDENTIAL_DECRYPT_FAILED", result.stderr)


if __name__ == "__main__":
    unittest.main()
