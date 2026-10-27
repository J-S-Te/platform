#!/usr/bin/env python3
"""uip-credential/v1 凭据封套 CLI 辅助工具（仅 Python 3 标准库）。

用于 host 侧运维脚本（如 subsystem.sh）在平台启用 CREDENTIAL_ENCRYPTION_MODE=required
时完成加密登录：RSA-2048 OAEP(SHA-256, label=uip-credential-v1) 封装
K_req(32)||K_resp(32)，正文使用 AES-256-GCM，AAD 由协议字段按行拼接。

不依赖 cryptography/node；不替代 HTTPS。仅由受信脚本调用，不打印口令/明文。
"""
import argparse
import base64
import hashlib
import hmac
import json
import os
import secrets
import sys
from pathlib import Path

PROTOCOL = "uip-credential/v1"
OAEP_LABEL = b"uip-credential-v1"
KEY_MATERIAL_SIZE = 64
AES_KEY_SIZE = 32
IV_SIZE = 12
TAG_SIZE = 16
SHA256_SIZE = 32


class EnvelopeError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code


def fail(code, message):
    raise EnvelopeError(code, message)


# ---------------------------------------------------------------- base64url
def b64u_encode(value):
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


def b64u_decode(value, what):
    if not isinstance(value, str) or not value or "=" in value:
        fail("CREDENTIAL_ENVELOPE_INVALID", what + " 编码无效")
    try:
        raw = base64.urlsafe_b64decode(value + "=" * ((4 - len(value) % 4) % 4))
    except Exception:
        fail("CREDENTIAL_ENVELOPE_INVALID", what + " 编码无效")
    return raw


# ---------------------------------------------------------------- PEM / DER
def pem_to_der(text):
    lines = [line.strip() for line in text.strip().splitlines()]
    body = [line for line in lines if line and not line.startswith("-----")]
    if not body:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "公钥不是 PEM")
    try:
        return base64.b64decode("".join(body), validate=True)
    except Exception:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "公钥 PEM 无法解析")


def der_tlv(data, offset):
    if offset >= len(data):
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "DER 数据不完整")
    tag = data[offset]
    offset += 1
    if offset >= len(data):
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "DER 长度缺失")
    length_byte = data[offset]
    offset += 1
    if length_byte & 0x80:
        count = length_byte & 0x7F
        if count == 0 or count > 8 or offset + count > len(data):
            fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "DER 长度非法")
        length = int.from_bytes(data[offset:offset + count], "big")
        offset += count
    else:
        length = length_byte
    end = offset + length
    if end > len(data):
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "DER 内容越界")
    return tag, data[offset:end], end


def parse_spki_public_key(pem_text):
    der = pem_to_der(pem_text)
    tag, content, _ = der_tlv(der, 0)
    if tag != 0x30:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "公钥不是 SPKI")
    _, _, offset = der_tlv(content, 0)
    tag, bits, _ = der_tlv(content, offset)
    if tag != 0x03 or not bits or bits[0] != 0:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "公钥 BIT STRING 非法")
    tag, rsa_sequence, _ = der_tlv(bits[1:], 0)
    if tag != 0x30:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "公钥不是 RSA")
    tag, modulus, offset = der_tlv(rsa_sequence, 0)
    if tag != 0x02:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "RSA 模数非法")
    tag, exponent, _ = der_tlv(rsa_sequence, offset)
    if tag != 0x02:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "RSA 指数非法")
    n = int.from_bytes(modulus, "big")
    e = int.from_bytes(exponent, "big")
    if n.bit_length() < 2048 or e < 3:
        fail("CREDENTIAL_CRYPTO_UNAVAILABLE", "RSA 公钥强度不足")
    return n, e


# ---------------------------------------------------------------- RSA-OAEP
def mgf1(seed, length):
    output = bytearray()
    counter = 0
    while len(output) < length:
        output.extend(hashlib.sha256(seed + counter.to_bytes(4, "big")).digest())
        counter += 1
    return bytes(output[:length])


def xor_bytes(left, right):
    return bytes(a ^ b for a, b in zip(left, right))


def rsa_oaep_encrypt(public_key_pem, message):
    n, e = parse_spki_public_key(public_key_pem)
    k = (n.bit_length() + 7) // 8
    h_len = SHA256_SIZE
    if len(message) > k - 2 * h_len - 2:
        fail("CREDENTIAL_ENCRYPTION_FAILED", "凭据封装内容过长")
    l_hash = hashlib.sha256(OAEP_LABEL).digest()
    padding = b"\x00" * (k - len(message) - 2 * h_len - 2)
    data_block = l_hash + padding + b"\x01" + message
    seed = secrets.token_bytes(h_len)
    masked_db = xor_bytes(data_block, mgf1(seed, k - h_len - 1))
    masked_seed = xor_bytes(seed, mgf1(masked_db, h_len))
    encoded = b"\x00" + masked_seed + masked_db
    value = int.from_bytes(encoded, "big")
    if value >= n:
        fail("CREDENTIAL_ENCRYPTION_FAILED", "RSA 编码越界")
    return pow(value, e, n).to_bytes(k, "big")


# ---------------------------------------------------------------- AES-256
def _gf_mul(left, right):
    result = 0
    for _ in range(8):
        if right & 1:
            result ^= left
        high = left & 0x80
        left = (left << 1) & 0xFF
        if high:
            left ^= 0x1B
        right >>= 1
    return result


def _build_aes_tables():
    inverse = [0] * 256
    for value in range(1, 256):
        for candidate in range(1, 256):
            if _gf_mul(value, candidate) == 1:
                inverse[value] = candidate
                break
    def rotate(value, amount):
        return ((value << amount) | (value >> (8 - amount))) & 0xFF
    sbox = []
    for value in range(256):
        base = inverse[value]
        sbox.append(
            base ^ rotate(base, 1) ^ rotate(base, 2) ^ rotate(base, 3) ^ rotate(base, 4) ^ 0x63
        )
    return sbox


_SBOX = _build_aes_tables()
_RCON = [0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80, 0x1B, 0x36]


def _sub_word(word):
    return [_SBOX[byte] for byte in word]


def _rot_word(word):
    return word[1:] + word[:1]


def aes_expand_key(key):
    if len(key) != AES_KEY_SIZE:
        fail("CREDENTIAL_ENCRYPTION_FAILED", "AES 密钥长度非法")
    words = [list(key[index:index + 4]) for index in range(0, AES_KEY_SIZE, 4)]
    total = 4 * 15
    for index in range(8, total):
        temp = list(words[index - 1])
        if index % 8 == 0:
            temp = _sub_word(_rot_word(temp))
            temp[0] ^= _RCON[index // 8 - 1]
        elif index % 8 == 4:
            temp = _sub_word(temp)
        words.append([words[index - 8][position] ^ temp[position] for position in range(4)])
    return words


def _add_round_key(state, words, round_index):
    for column in range(4):
        word = words[round_index * 4 + column]
        for row in range(4):
            state[row + 4 * column] ^= word[row]


def _sub_bytes(state):
    for index in range(16):
        state[index] = _SBOX[state[index]]


def _shift_rows(state):
    original = list(state)
    for row in range(4):
        for column in range(4):
            state[row + 4 * column] = original[row + 4 * ((column + row) % 4)]


def _mix_columns(state):
    for column in range(4):
        index = 4 * column
        a0, a1, a2, a3 = state[index:index + 4]
        state[index] = _gf_mul(a0, 2) ^ _gf_mul(a1, 3) ^ a2 ^ a3
        state[index + 1] = a0 ^ _gf_mul(a1, 2) ^ _gf_mul(a2, 3) ^ a3
        state[index + 2] = a0 ^ a1 ^ _gf_mul(a2, 2) ^ _gf_mul(a3, 3)
        state[index + 3] = _gf_mul(a0, 3) ^ a1 ^ a2 ^ _gf_mul(a3, 2)


def aes_encrypt_block(words, block):
    if len(block) != 16:
        fail("CREDENTIAL_ENCRYPTION_FAILED", "AES 分组长度非法")
    state = list(block)
    _add_round_key(state, words, 0)
    for round_index in range(1, 14):
        _sub_bytes(state)
        _shift_rows(state)
        _mix_columns(state)
        _add_round_key(state, words, round_index)
    _sub_bytes(state)
    _shift_rows(state)
    _add_round_key(state, words, 14)
    return bytes(state)


# ---------------------------------------------------------------- AES-GCM
def _gf128_mul(left, right):
    result = 0
    product = right
    for shift in range(127, -1, -1):
        if (left >> shift) & 1:
            result ^= product
        if product & 1:
            product = (product >> 1) ^ 0xE1000000000000000000000000000000
        else:
            product >>= 1
    return result


def _ghash(hash_key, data):
    value = 0
    for offset in range(0, len(data), 16):
        block = int.from_bytes(data[offset:offset + 16], "big")
        value = _gf128_mul(value ^ block, hash_key)
    return value


def _counter_stream_crypt(words, initial_counter, payload):
    output = bytearray()
    prefix = initial_counter[:12]
    counter = int.from_bytes(initial_counter[12:], "big")
    for offset in range(0, len(payload), 16):
        counter = (counter + 1) % (1 << 32)
        keystream = aes_encrypt_block(words, prefix + counter.to_bytes(4, "big"))
        chunk = payload[offset:offset + 16]
        output.extend(a ^ b for a, b in zip(chunk, keystream))
    return bytes(output)


def _pad16(data):
    remainder = len(data) % 16
    return data if remainder == 0 else data + b"\x00" * (16 - remainder)


def gcm_seal(key, iv, plaintext, aad):
    if len(key) != AES_KEY_SIZE or len(iv) != IV_SIZE:
        fail("CREDENTIAL_ENCRYPTION_FAILED", "AES-GCM 参数非法")
    words = aes_expand_key(key)
    hash_key = int.from_bytes(aes_encrypt_block(words, b"\x00" * 16), "big")
    initial_counter = iv + b"\x00\x00\x00\x01"
    ciphertext = _counter_stream_crypt(words, initial_counter, plaintext)
    lengths = (len(aad) * 8).to_bytes(8, "big") + (len(ciphertext) * 8).to_bytes(8, "big")
    digest = _ghash(hash_key, _pad16(aad) + _pad16(ciphertext) + lengths)
    tag = (digest ^ int.from_bytes(aes_encrypt_block(words, initial_counter), "big")).to_bytes(16, "big")
    return ciphertext, tag


def gcm_open(key, iv, ciphertext, tag, aad):
    if len(key) != AES_KEY_SIZE or len(iv) != IV_SIZE or len(tag) != TAG_SIZE:
        fail("CREDENTIAL_ENVELOPE_INVALID", "AES-GCM 参数非法")
    words = aes_expand_key(key)
    hash_key = int.from_bytes(aes_encrypt_block(words, b"\x00" * 16), "big")
    initial_counter = iv + b"\x00\x00\x00\x01"
    lengths = (len(aad) * 8).to_bytes(8, "big") + (len(ciphertext) * 8).to_bytes(8, "big")
    digest = _ghash(hash_key, _pad16(aad) + _pad16(ciphertext) + lengths)
    expected = (digest ^ int.from_bytes(aes_encrypt_block(words, initial_counter), "big")).to_bytes(16, "big")
    if not hmac.compare_digest(expected, tag):
        fail("CREDENTIAL_DECRYPT_FAILED", "凭据响应校验失败")
    return _counter_stream_crypt(words, initial_counter, ciphertext)


# ---------------------------------------------------------------- protocol
def build_aad(direction, kid, challenge_id, purpose, resource_id, content_type):
    fields = [PROTOCOL, direction, kid, challenge_id, purpose, resource_id, content_type]
    for field in fields:
        if "\r" in field or "\n" in field:
            fail("CREDENTIAL_ENVELOPE_INVALID", "凭据字段包含换行")
    return "\n".join(fields).encode("utf-8")


def envelope_from_challenge(challenge, plaintext, purpose, resource_id, content_type):
    data = challenge.get("data") if isinstance(challenge, dict) else None
    if not isinstance(data, dict):
        fail("CREDENTIAL_ENVELOPE_INVALID", "凭据挑战响应结构无效")
    if data.get("protocol") != PROTOCOL:
        fail("CREDENTIAL_ENVELOPE_INVALID", "凭据协议不匹配")
    if data.get("purpose") != purpose or (data.get("resource_id") or "") != resource_id:
        fail("CREDENTIAL_ENVELOPE_INVALID", "凭据挑战用途或资源不匹配")
    kid = data.get("kid") or ""
    challenge_id = data.get("challenge_id") or ""
    public_key = data.get("public_key") or ""
    if not kid or not challenge_id or not public_key:
        fail("CREDENTIAL_ENVELOPE_INVALID", "凭据挑战字段缺失")
    material = secrets.token_bytes(KEY_MATERIAL_SIZE)
    request_key, response_key = material[:AES_KEY_SIZE], material[AES_KEY_SIZE:]
    encrypted_key = rsa_oaep_encrypt(public_key, material)
    iv = secrets.token_bytes(IV_SIZE)
    aad = build_aad("request", kid, challenge_id, purpose, resource_id, content_type)
    ciphertext, tag = gcm_seal(request_key, iv, plaintext, aad)
    request_body = {
        "credential_envelope": {
            "protocol": PROTOCOL,
            "kid": kid,
            "challenge_id": challenge_id,
            "purpose": purpose,
            "resource_id": resource_id,
            "content_type": content_type,
            "encrypted_key": b64u_encode(encrypted_key),
            "iv": b64u_encode(iv),
            "ciphertext": b64u_encode(ciphertext),
            "tag": b64u_encode(tag),
        }
    }
    meta = {
        "kid": kid,
        "challenge_id": challenge_id,
        "purpose": purpose,
        "resource_id": resource_id,
        "content_type": content_type,
    }
    return request_body, response_key, meta


def decrypt_envelope_response(response, response_key, meta):
    if not isinstance(response, dict) or "credential_envelope" not in response:
        return None
    envelope = response.get("credential_envelope")
    if not isinstance(envelope, dict):
        fail("CREDENTIAL_DECRYPT_FAILED", "凭据响应封套结构无效")
    if envelope.get("protocol") != PROTOCOL:
        fail("CREDENTIAL_DECRYPT_FAILED", "凭据响应协议不匹配")
    for key in ("kid", "challenge_id", "purpose", "resource_id", "content_type"):
        if (envelope.get(key) or "") != (meta.get(key) or ""):
            fail("CREDENTIAL_DECRYPT_FAILED", "凭据响应元数据不匹配")
    aad = build_aad(
        "response", meta["kid"], meta["challenge_id"], meta["purpose"],
        meta["resource_id"], meta["content_type"],
    )
    plaintext = gcm_open(
        response_key,
        b64u_decode(envelope.get("iv"), "iv"),
        b64u_decode(envelope.get("ciphertext"), "ciphertext"),
        b64u_decode(envelope.get("tag"), "tag"),
        aad,
    )
    try:
        json.loads(plaintext.decode("utf-8"))
    except Exception:
        fail("CREDENTIAL_DECRYPT_FAILED", "凭据响应解密后格式无效")
    return plaintext


# ---------------------------------------------------------------- CLI
def read_file(path, what):
    try:
        return Path(path).read_bytes()
    except OSError:
        fail("CREDENTIAL_ENVELOPE_INVALID", what + " 文件不可读")


def write_private(path, data):
    target = Path(path)
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "wb") as handle:
        handle.write(data)


def read_json_file(path, what):
    try:
        document = json.loads(read_file(path, what).decode("utf-8"))
    except EnvelopeError:
        raise
    except Exception:
        fail("CREDENTIAL_ENVELOPE_INVALID", what + " 不是合法 JSON")
    return document


def command_encrypt(args):
    challenge = read_json_file(args.challenge, "challenge")
    plaintext = read_file(args.plaintext, "plaintext")
    request_body, response_key, meta = envelope_from_challenge(
        challenge, plaintext, args.purpose, args.resource_id, args.content_type,
    )
    write_private(args.request_out, json.dumps(request_body, separators=(",", ":")).encode("utf-8"))
    write_private(args.response_key_out, b64u_encode(response_key).encode("ascii"))
    write_private(args.meta_out, json.dumps(meta, separators=(",", ":")).encode("utf-8"))
    return 0


def command_decrypt(args):
    response_raw = read_file(args.response, "response")
    try:
        response = json.loads(response_raw.decode("utf-8"))
    except Exception:
        response = None
    meta = read_json_file(args.meta_file, "meta")
    response_key = b64u_decode(read_file(args.response_key, "response-key").decode("ascii").strip(), "response-key")
    if isinstance(response, dict) and "credential_envelope" in response:
        plaintext = decrypt_envelope_response(response, response_key, meta)
        write_private(args.output, plaintext)
        return 0
    # Protocol/auth rejections may be plaintext by contract; preserve the body for diagnostics.
    write_private(args.output, response_raw)
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description="uip-credential/v1 envelope helper")
    subparsers = parser.add_subparsers(dest="command", required=True)

    encrypt = subparsers.add_parser("encrypt")
    encrypt.add_argument("--challenge", required=True)
    encrypt.add_argument("--plaintext", required=True)
    encrypt.add_argument("--purpose", required=True)
    encrypt.add_argument("--resource-id", default="")
    encrypt.add_argument("--content-type", default="application/json")
    encrypt.add_argument("--request-out", required=True)
    encrypt.add_argument("--response-key-out", required=True)
    encrypt.add_argument("--meta-out", required=True)
    encrypt.set_defaults(handler=command_encrypt)

    decrypt = subparsers.add_parser("decrypt")
    decrypt.add_argument("--response", required=True)
    decrypt.add_argument("--response-key", required=True)
    decrypt.add_argument("--meta-file", required=True)
    decrypt.add_argument("--output", required=True)
    decrypt.set_defaults(handler=command_decrypt)

    args = parser.parse_args(argv)
    try:
        return args.handler(args)
    except EnvelopeError as error:
        print("[credential-envelope] ERROR: " + error.code + ": " + str(error), file=sys.stderr)
        return 2
    except Exception:
        print("[credential-envelope] ERROR: CREDENTIAL_ENCRYPTION_FAILED", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
