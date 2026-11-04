#!/usr/bin/env python
"""Extract compatibility vectors from the SPlusthon reference implementation.

Treats the Python library as the immutable behavioral reference: every vector
below is produced by executing reference code, with deterministic randomness
(seed-fixed PRNG standing in for os.urandom) and a fixed clock.

Run:
    py -3.12 tools/vectors/generate.py

Outputs JSON fixtures into tests/vectors/. Never modifies reference/SPlusthon.
"""

import base64
import io
import json
import hashlib
import logging
import os
import struct
import sys
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
REF = ROOT / "reference" / "SPlusthon"
OUT = ROOT / "tests" / "vectors"
SEED = int(os.environ.get("SPLUSGO_VECTOR_SEED", "0x5EEDC0DE"), 16)
FIXED_TIME = 1700000000.0

sys.path.insert(0, str(REF))

# ---------------------------------------------------------------------------
# Deterministic randomness: os.urandom is the reference implementation's only
# entropy source on the paths we exercise. Replace it with a keyed
# sha256-counter stream so every vector is reproducible.
# ---------------------------------------------------------------------------


class DetRandom:
    def __init__(self, seed: int):
        self.seed = seed.to_bytes(8, "big")
        self.counter = 0
        self.buf = b""

    def bytes(self, n: int) -> bytes:
        while len(self.buf) < n:
            self.buf += hashlib.sha256(
                self.seed + self.counter.to_bytes(8, "big")
            ).digest()
            self.counter += 1
        out, self.buf = self.buf[:n], self.buf[n:]
        return out


_rng = DetRandom(SEED)
os.urandom = _rng.bytes  # noqa: A110 - deliberate, scoped to this process

import time as _time  # noqa: E402

_time.time = lambda: FIXED_TIME  # fixed clock for msg_id generation

import splusthon  # noqa: E402
from splusthon.crypto import AES, AuthKey  # noqa: E402
from splusthon.network import mtprotostate as mts  # noqa: E402
from splusthon.network.connection import tcpabridged  # noqa: E402
from splusthon.network.connection import websocket as wsconn  # noqa: E402
from splusthon.extensions import BinaryReader  # noqa: E402
from splusthon.tl.alltlobjects import tlobjects, LAYER  # noqa: E402
from splusthon.tl.core import core_objects, GzipPacked  # noqa: E402
from splusthon.tl.core.messagecontainer import MessageContainer  # noqa: E402
from splusthon.tl.types import (  # noqa: E402
    LabeledPrice, MaskCoords, Contact,
    ResPQ, PQInnerData, FutureSalt, FutureSalts,
    User, UserTypeNormal, UserEmpty,
    InputPeerUser, InputPeerChat,
    Message, MessageEmpty, MessageMediaDocument, Document, DocumentAttributeFilename, PeerChat, PeerUser,
    UpdateNewMessage, Updates as UpdatesObj,
    RpcError,
)
# NOTE: the reference implementation does not generate classes for
# boolTrue/boolFalse/true — boxed Bools are inline Python bools (the Go port
# keeps struct objects for object-position decoding; see the compatibility
# test's documented superset).
from splusthon.tl.functions import ReqPqMultiRequest, PingRequest  # noqa: E402
from splusthon.tl.functions.messages import (  # noqa: E402
    SendMessageRequest, GetHistoryRequest,
)
from splusthon.tl.functions.upload import SaveFilePartRequest  # noqa: E402
from splusthon.tl.functions import InvokeAfterMsgRequest, InitConnectionRequest  # noqa: E402
from splusthon.tl.functions import InvokeWithLayerRequest  # noqa: E402
from splusthon.tl.functions.help import GetConfigRequest  # noqa: E402
from splusthon.sessions.memory import MemorySession  # noqa: E402
from splusthon.sessions.string import StringSession  # noqa: E402

# gzip bytes are compressor-specific; the wire only guarantees decompression.
# Disable the gzip-if-smaller transform so envelope vectors are byte-stable
# across zlib / isal / Go flate. Round-trip coverage of GzipPacked itself is
# handled separately (decode direction only).
GzipPacked.gzip_if_smaller = staticmethod(lambda content_related, data: data)

loggers = defaultdict(lambda: logging.getLogger("vectors"))

META = {
    "generator": "tools/vectors/generate.py",
    "seed": SEED,
    "fixed_time": FIXED_TIME,
    "layer": LAYER,
    "reference_version": splusthon.version.__version__,
    "python": sys.version.split()[0],
}


def hexx(b: bytes) -> str:
    return b.hex()


def emit(name: str, cases: list) -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    doc = {"meta": META, "cases": cases}
    (OUT / name).write_text(json.dumps(doc, indent=1, sort_keys=True) + "\n")
    print(f"  {name}: {len(cases)} cases")


def obj_bytes(o) -> str:
    return hexx(bytes(o))


# ---------------------------------------------------------------------------
# 1. Constructor IDs
# ---------------------------------------------------------------------------


def v_constructors():
    OUT.mkdir(parents=True, exist_ok=True)
    # Key by hex ID; class names can collide across namespaces (32 cases),
    # so the value records the module-qualified Python class instead.
    generated = {}
    for cid, cls in sorted(tlobjects.items(), key=lambda kv: kv[0]):
        mod = cls.__module__.rsplit(".", 1)[-1]
        generated[f"{cid:08x}"] = f"{mod}.{cls.__name__}"
    core = {cls.__name__: f"{cid:08x}" for cid, cls in sorted(core_objects.items(), key=lambda kv: kv[0])}
    (OUT / "constructors.json").write_text(
        json.dumps({"meta": META, "generated": generated, "core": core}, indent=1, sort_keys=True) + "\n"
    )
    print(f"  constructors.json: {len(generated)} generated + {len(core)} core")


# ---------------------------------------------------------------------------
# 2-6. Primitive / string / bytes / vector / flag encode vectors
# ---------------------------------------------------------------------------


def v_primitives():
    nonce128 = int.from_bytes(_rng.bytes(16), "big", signed=True)
    nonce256 = int.from_bytes(_rng.bytes(32), "little", signed=True)
    cases = [
        # NOTE: error#c4b9f9bb is not generated by the reference; LabeledPrice
        # exercises int32/long/string fields instead.
        {"name": "int32_long_string", "ctor": "labeledPrice#cb296bf8",
         "obj": LabeledPrice(label="x", amount=-123456)},
        {"name": "long", "ctor": "labeledPrice#cb296bf8", "obj": LabeledPrice(label="p", amount=1234567890123)},
        {"name": "double", "ctor": "maskCoords#aed6dbb2", "obj": MaskCoords(n=1, x=1.5, y=-2.25, zoom=3.75)},
        {"name": "bool_boxed_true", "ctor": "contact#145ade0b", "obj": Contact(user_id=42, mutual=True)},
        {"name": "bool_boxed_false", "ctor": "contact#145ade0b", "obj": Contact(user_id=42, mutual=False)},
        {"name": "int128", "ctor": "req_pq_multi#be7e8ef1", "obj": ReqPqMultiRequest(nonce=nonce128)},
        {"name": "int256", "ctor": "p_q_inner_data#83c95aec",
         "obj": PQInnerData(pq=b"\x01\x02", p=b"\x03", q=b"\x05", nonce=nonce128,
                            server_nonce=nonce128, new_nonce=nonce256)},
    ]
    emit("primitives.json", [
        {**{k: v for k, v in c.items() if k != "obj"}, "bytes": obj_bytes(c["obj"])} for c in cases
    ])


def v_strings():
    samples = ["", "a", "abc", "abcd", "hello world", "سلام سروش",
               "x" * 252, "x" * 253, "x" * 254, "x" * 255, "x" * 256, "x" * 4096]
    cases = []
    for s in samples:
        raw = s.encode("utf-8")
        framed = bytes.__new__(bytes)  # placeholder to keep struct clear
        framed = TL_serialize_bytes(raw)
        cases.append({"name": f"str_len_{len(raw)}", "utf8": hexx(raw), "framed": hexx(framed)})
    # Embedded in a real object so the ctor path is exercised too.
    for s in ("hello", "سروش"):
        o = LabeledPrice(label=s, amount=1)
        cases.append({"name": "str_in_object", "ctor": "labeledPrice#cb296bf8", "bytes": obj_bytes(o)})
    emit("strings.json", cases)


def TL_serialize_bytes(data: bytes) -> bytes:
    """Reference framing (tl/tlobject.py serialize_bytes)."""
    r = []
    if len(data) < 254:
        padding = (len(data) + 1) % 4
        if padding != 0:
            padding = 4 - padding
        r.append(bytes([len(data)]))
        r.append(data)
    else:
        padding = len(data) % 4
        if padding != 0:
            padding = 4 - padding
        r.append(bytes([254, len(data) % 256, (len(data) >> 8) % 256, (len(data) >> 16) % 256]))
        r.append(data)
    r.append(bytes(padding))
    return b"".join(r)


def v_bytes():
    samples = [b"", b"\x00", b"\xef", b"\xef" * 4, bytes(range(64)),
               b"\x00" * 252, b"\x00" * 253, b"\x00" * 254, b"\x00" * 255, b"\x00" * 1024]
    cases = []
    for i, b in enumerate(samples):
        cases.append({"name": f"bytes_{i}_len_{len(b)}", "raw": hexx(b), "framed": hexx(TL_serialize_bytes(b))})
    o = SaveFilePartRequest(file_id=123456789, file_part=3, bytes=b"\x01\x02\x03\x04\x05")
    cases.append({"name": "bytes_in_request", "ctor": "upload.saveFilePart#b304a621", "bytes": obj_bytes(o)})
    emit("bytes_vectors.json", cases)


def v_vectors():
    cases = [
        {"name": "vector_long_boxed",
         "obj": ResPQ(nonce=int.from_bytes(_rng.bytes(16), "big", signed=True),
                      server_nonce=int.from_bytes(_rng.bytes(16), "big", signed=True),
                      pq=b"\x17", server_public_key_fingerprints=[1, -2, 3, 2**62])},
        {"name": "vector_future_salt_bare",
         "obj": FutureSalts(req_msg_id=123, now=456,
                            salts=[FutureSalt(valid_since=i, valid_until=i + 100, salt=i * 7) for i in range(3)])},
    ]
    emit("vectors.json", [{"name": c["name"], "ctor": type(c["obj"]).__name__, "bytes": obj_bytes(c["obj"])} for c in cases])


def v_flags():
    def user(**kw):
        base = dict(id=1234567, user_type=UserTypeNormal())
        base.update(kw)
        return User(**base)

    cases = [
        {"name": "user_minimal", "obj": user()},
        {"name": "user_flags0", "obj": user(access_hash=987654321, first_name="Ali", username="ali")},
        {"name": "user_flags_mixed", "obj": user(bot=True, bot_info_version=7, verified=True, premium=True)},
        {"name": "user_flags2", "obj": user(bot_can_edit=True, close_friend=True, usernames=[])},
        {"name": "user_self_flag", "obj": user(is_self=True)},
        {"name": "user_empty", "obj": UserEmpty(id=1)},
    ]
    emit("flags.json", [{"name": c["name"], "ctor": type(c["obj"]).__name__, "bytes": obj_bytes(c["obj"])} for c in cases])


def v_nested():
    doc = Document(
        id=2**61, access_hash=42, file_reference=b"\xde\xad", date=1700000000,
        mime_type="video/mp4", size=1048576,
        thumbs=[], dc_id=2, attributes=[DocumentAttributeFilename(file_name="clip.mp4")],
    )
    msg = Message(
        id=7, peer_id=PeerChat(chat_id=555), date=1700000000,
        message="hi", media=MessageMediaDocument(document=doc, ttl_seconds=60),
    )
    upd = UpdateNewMessage(message=msg, pts=10, pts_count=1)
    obj = UpdatesObj(updates=[upd], users=[User(id=9, user_type=UserTypeNormal())], chats=[], date=1700000000, seq=3)
    emit("nested.json", [{"name": "updates_tree", "ctor": "updates#74ae4240", "bytes": obj_bytes(obj)}])


def v_requests():
    peer = InputPeerUser(user_id=111, access_hash=222)
    cases = [
        {"name": "ping", "obj": PingRequest(ping_id=2**62)},
        {"name": "send_message",
         "obj": SendMessageRequest(peer=peer, message="سلام", random_id=987654321,
                                   no_webpage=True)},
        {"name": "get_history",
         "obj": GetHistoryRequest(peer=peer, offset_id=0, offset_date=0, add_offset=0,
                                  limit=20, max_id=0, min_id=0, hash=0)},
        {"name": "invoke_after_msg",
         "obj": InvokeAfterMsgRequest(msg_id=555, query=GetConfigRequest())},
        {"name": "invoke_with_layer_init",
         "obj": InvokeWithLayerRequest(
             layer=LAYER,
             query=InitConnectionRequest(
                 api_id=1030400, device_model="PC 64bit", system_version="10",
                 app_version="3.9.2 A", system_lang_code="fa", lang_pack="", lang_code="fa",
                 query=GetConfigRequest(),
             ),
         )},
        {"name": "upload_bytes",
         "obj": SaveFilePartRequest(file_id=2**60, file_part=0, bytes=_rng.bytes(128))},
    ]
    emit("requests.json", [
        {"name": c["name"], "ctor": type(c["obj"]).__name__, "bytes": obj_bytes(c["obj"])} for c in cases
    ])


def v_responses():
    upd = UpdatesObj(updates=[], users=[User(id=5, user_type=UserTypeNormal())], chats=[], date=1700000000, seq=1)
    rpc_result = struct.pack("<Iq", 0xF35C6D01, 2**40) + bytes(upd)
    rpc_err = bytes(RpcError(error_code=420, error_message="FLOOD_WAIT_7"))
    cases = [
        {"name": "rpc_result_updates", "ctor": "rpc_result#f35c6d01", "bytes": hexx(rpc_result)},
        {"name": "rpc_error", "ctor": "rpc_error#2144ca19", "bytes": hexx(rpc_err)},
        {"name": "updates", "ctor": "updates#74ae4240", "bytes": obj_bytes(upd)},
    ]
    emit("responses.json", cases)


def v_errors():
    from splusthon.errors import rpc_message_to_error
    from splusthon.tl.types import RpcError as RpcErrT
    samples = ["FLOOD_WAIT_5", "PHONE_NUMBER_INVALID", "TIMEOUT", "NO_WORKERS_RUNNING",
               "CHAT_ID_INVALID", "PEER_ID_INVALID", "SESSION_REVOKED", "AUTH_KEY_UNREGISTERED",
               "MSG_ID_INVALID", "FILE_PARTS_INVALID"]
    cases = []
    for msg in samples:
        err = rpc_message_to_error(RpcErrT(error_code=420, error_message=msg), None)
        cases.append({"message": msg, "python_class": type(err).__name__,
                      "code": getattr(err, "code", None)})
    emit("errors.json", cases)


def v_string_session():
    key = _rng.bytes(256)
    # Current format
    mem = MemorySession()
    mem.set_dc(2, "149.154.167.51", 443)
    mem.auth_key = AuthKey(key)
    current = StringSession()
    current.set_dc(2, "149.154.167.51", 443)
    current.auth_key = AuthKey(key)
    s_cur = current.save()
    parsed = StringSession(s_cur)
    # Legacy 352-byte payload: the reference's legacy branch is dead code —
    # its guard requires 352 bytes but the unpack needs 263, so parsing
    # raises struct.error. Record the failure as the observable behavior.
    legacy_payload = struct.pack(">B4sH256s", 3, bytes([149, 154, 167, 51]), 443, key)
    s_legacy = "1" + base64.urlsafe_b64encode(legacy_payload).decode("ascii")
    legacy_error = None
    try:
        StringSession(s_legacy)
    except Exception as e:  # struct.error expected
        legacy_error = type(e).__name__
    # Domain-address format
    mem2 = StringSession()
    mem2.set_dc(1, "im-server.splus.ir", 443)
    mem2.auth_key = AuthKey(key)
    s_domain = mem2.save()
    cases = [
        {"name": "current_format", "session": s_cur, "dc_id": parsed.dc_id,
         "address": parsed.server_address, "port": parsed.port, "key": hexx(parsed.auth_key.key)},
        {"name": "legacy_format_rejected", "session": s_legacy, "python_error": legacy_error},
        {"name": "domain_format", "session": s_domain, "dc_id": mem2.dc_id,
         "address": mem2.server_address, "port": mem2.port, "key": hexx(mem2.auth_key.key)},
    ]
    emit("string_session.json", cases)


# ---------------------------------------------------------------------------
# 12-16. Crypto vectors
# ---------------------------------------------------------------------------


def v_ige():
    key = _rng.bytes(32)
    iv = _rng.bytes(32)
    cases = []
    for n in (16, 32, 512, 1008):
        pt = _rng.bytes(n)
        ct = AES.encrypt_ige(pt, key, iv)
        rt = AES.decrypt_ige(ct, key, iv)
        assert rt == pt, "reference ige round-trip failed"
        cases.append({"name": f"ige_{n}", "key": hexx(key), "iv": hexx(iv),
                      "plain": hexx(pt), "cipher": hexx(ct)})
    emit("ige.json", cases)


def v_msg_key():
    auth = _rng.bytes(256)
    cases = []
    for x in (0, 8):
        mk = _rng.bytes(16)
        aes_key, aes_iv = mts.MTProtoState._calc_key(auth, mk, client=(x == 0))
        cases.append({"name": f"calc_key_x{x}", "auth_key": hexx(auth), "msg_key": hexx(mk),
                      "aes_key": hexx(aes_key), "aes_iv": hexx(aes_iv)})
    # msg_key derivation: SHA256(auth_key[88:120] || plaintext)[8:24] (client x=0)
    pt = _rng.bytes(160)
    mk = hashlib.sha256(auth[88:120] + pt).digest()[8:24]
    cases.append({"name": "msg_key_derive", "auth_key": hexx(auth), "plain": hexx(pt), "msg_key": hexx(mk)})
    emit("msg_key.json", cases)


def make_state(key: bytes) -> mts.MTProtoState:
    st = mts.MTProtoState(AuthKey(key), loggers=loggers)
    return st


def build_messages(state, requests, content_related=True):
    """Replicates MessagePacker's packing for a batch (container if >1)."""
    buf = io.BytesIO()
    msg_ids = []
    for req in requests:
        msg_ids.append(state.write_data_as_message(buf, bytes(req), content_related))
    if len(requests) > 1:
        inner = struct.pack("<Ii", MessageContainer.CONSTRUCTOR_ID, len(requests)) + buf.getvalue()
        buf = io.BytesIO()
        container_id = state.write_data_as_message(buf, inner, content_related=False)
        return buf.getvalue(), container_id
    return buf.getvalue(), msg_ids[0]


def key_id(key: bytes) -> int:
    return struct.unpack("<Q", hashlib.sha1(key).digest()[12:20])[0]


def server_encrypt(key: bytes, plain: bytes) -> bytes:
    """Build a server->client envelope (x=8 keys), as the real server would.

    msg_key = SHA256(auth_key[96:128] || data || padding)[8:24]
    """
    pad = os.urandom(-(len(plain) + 12) % 16 + 12)
    data = plain + pad
    mk = hashlib.sha256(key[96:128] + data).digest()[8:24]
    ak, ai = mts.MTProtoState._calc_key(key, mk, client=False)
    ct = AES.encrypt_ige(data, ak, ai)
    return struct.pack("<Q", key_id(key)) + mk + ct


def v_envelope():
    cases = []
    key = _rng.bytes(256)
    st = make_state(key)
    salt, session = 0x1122334455667788, -0x0FEDCBA987654321
    st.salt = salt
    st.id = session

    peer = InputPeerUser(user_id=111, access_hash=222)
    # Single request envelope
    inner, mid = build_messages(st, [SendMessageRequest(peer=peer, message="ok", random_id=1)])
    payload = struct.pack("<qq", salt, session) + inner
    # NOTE: the state prepends salt|session itself; pass message bytes only.
    env = st.encrypt_message_data(inner)
    # Recover padding for byte-exact Go reproduction.
    msg_key = env[8:24]
    aes_key, aes_iv = mts.MTProtoState._calc_key(key, msg_key, client=True)
    plain = AES.decrypt_ige(env[24:], aes_key, aes_iv)
    pad = plain[len(payload):]
    cases.append({"name": "single_request", "auth_key": hexx(key), "salt": salt, "session_id": session,
                  "payload": hexx(payload), "padding": hexx(pad), "envelope": hexx(env), "msg_id": mid})

    # Container of three requests
    reqs = [PingRequest(ping_id=1), PingRequest(ping_id=2),
            SendMessageRequest(peer=peer, message="m", random_id=3)]
    inner2, cid = build_messages(st, reqs)
    payload2 = struct.pack("<qq", salt, session) + inner2
    env2 = st.encrypt_message_data(inner2)
    msg_key2 = env2[8:24]
    ak2, ai2 = mts.MTProtoState._calc_key(key, msg_key2, client=True)
    plain2 = AES.decrypt_ige(env2[24:], ak2, ai2)
    pad2 = plain2[len(payload2):]
    cases.append({"name": "container_request", "auth_key": hexx(key), "salt": salt, "session_id": session,
                  "payload": hexx(payload2), "padding": hexx(pad2), "envelope": hexx(env2), "msg_id": cid})
    # Server->client envelope carrying an rpc_result: exercises the decrypt
    # path (x=8 keys) exactly as the client will receive it.
    upd = UpdatesObj(updates=[], users=[User(id=5, user_type=UserTypeNormal())],
                     chats=[], date=1700000000, seq=1)
    rpc_body = struct.pack("<Iq", 0xF35C6D01, 987) + bytes(upd)
    inner3, sid = build_messages(st, [None], content_related=False) if False else (None, None)
    buf3 = io.BytesIO()
    resp_mid = st.write_data_as_message(buf3, rpc_body, content_related=False)
    resp_payload = struct.pack("<qq", salt, session) + buf3.getvalue()
    env3 = server_encrypt(key, resp_payload)
    pad3 = os.urandom  # placeholder, replaced below
    mk3 = env3[8:24]
    ak3, ai3 = mts.MTProtoState._calc_key(key, mk3, client=False)
    plain3 = AES.decrypt_ige(env3[24:], ak3, ai3)
    pad3 = plain3[len(resp_payload):]
    cases.append({"name": "server_response", "auth_key": hexx(key), "salt": salt,
                  "session_id": session, "payload": hexx(resp_payload),
                  "padding": hexx(pad3), "envelope": hexx(env3), "msg_id": resp_mid})

    emit("envelope.json", cases)


def v_obfuscated():
    key = _rng.bytes(256)
    st = make_state(key)
    salt, session = 7, 9
    st.salt, st.id = salt, session
    inner, _ = build_messages(st, [PingRequest(ping_id=42)])
    payload = struct.pack("<qq", salt, session) + inner
    envelope = st.encrypt_message_data(inner)

    packet = tcpabridged.AbridgedPacketCodec.encode_packet(None, envelope)
    codec = tcpabridged.AbridgedPacketCodec(None)
    random64 = _rng.bytes(64)
    # Force the exact random bytes through _init_header.
    saved = os.urandom
    os.urandom = lambda n: random64 if n == 64 else _rng.bytes(n)
    try:
        header, encryptor, decryptor = wsconn.WebSocketObfuscatedIO._init_header(codec)
    finally:
        os.urandom = saved
    stream = header + encryptor.encrypt(packet)
    # The inbound (decryptor) keystream is independent; record what a
    # server-originated frame looks like so Go can verify its decrypt path.
    inbound = decryptor.encrypt(packet)
    cases = [{"name": "ws_header_and_stream", "random64": hexx(random64), "header": hexx(header),
              "abridged_packet": hexx(packet), "stream": hexx(stream),
              "inbound_stream": hexx(inbound)}]
    emit("obfuscated.json", cases)


def v_roundtrip():
    """Full representative request/response path at the envelope level."""
    key = _rng.bytes(256)
    st = make_state(key)
    salt, session = 0x5566778899AABBCC, 0x0102030405060708
    st.salt, st.id = salt, session
    peer = InputPeerUser(user_id=1, access_hash=2)

    # ---- request direction: container of two content-related requests
    reqs = [SendMessageRequest(peer=peer, message="سلام", random_id=11),
            GetHistoryRequest(peer=peer, offset_id=0, offset_date=0, add_offset=0,
                              limit=1, max_id=0, min_id=0, hash=0)]
    payload_msg, _ = build_messages(st, reqs)
    payload = struct.pack("<qq", salt, session) + payload_msg
    env = st.encrypt_message_data(payload_msg)
    mk = env[8:24]
    ak, ai = mts.MTProtoState._calc_key(key, mk, client=True)
    pad = AES.decrypt_ige(env[24:], ak, ai)[len(payload):]
    req_case = {"name": "request_path", "auth_key": hexx(key), "salt": salt, "session_id": session,
                "payload": hexx(payload), "padding": hexx(pad), "envelope": hexx(env)}

    # ---- response direction: rpc_result carrying updates
    upd = UpdatesObj(updates=[UpdateNewMessage(
        message=Message(id=1, peer_id=PeerUser(user_id=1), date=1700000000,
                        message="reply"), pts=1, pts_count=1)],
        users=[User(id=1, user_type=UserTypeNormal())], chats=[], date=1700000000, seq=0)
    rpc_body = struct.pack("<Iq", 0xF35C6D01, 4242) + bytes(upd)
    resp_msg, _ = build_messages(st, [None], content_related=False) if False else (None, None)
    buf = io.BytesIO()
    resp_mid = st.write_data_as_message(buf, rpc_body, content_related=False)
    resp_payload = struct.pack("<qq", salt, session) + buf.getvalue()
    resp_env = server_encrypt(key, resp_payload)
    mk2 = resp_env[8:24]
    ak2, ai2 = mts.MTProtoState._calc_key(key, mk2, client=False)
    pad2 = AES.decrypt_ige(resp_env[24:], ak2, ai2)[len(resp_payload):]
    resp_case = {"name": "response_path", "auth_key": hexx(key), "salt": salt, "session_id": session,
                 "payload": hexx(resp_payload), "padding": hexx(pad2), "envelope": hexx(resp_env)}

    emit("roundtrip.json", [req_case, resp_case])



# ---------------------------------------------------------------------------
# Phase 5: RSA / DH key-exchange vectors
# ---------------------------------------------------------------------------


def v_rsa():
    """Fingerprints of every embedded server key (reference oracle), plus a
    deterministic RSA block for a generated test keypair. The private parts
    are included so Go fake servers can decrypt; this is TEST material."""
    from splusthon.crypto import rsa as ref_rsa
    global rsa, rsa_lib
    import rsa as rsa_mod
    rsa = rsa_mod
    rsa_lib = rsa_mod

    fps = {}
    for fp, (key, old) in ref_rsa._server_keys.items():
        fps[f"{fp & 0xFFFFFFFFFFFFFFFF:016x}"] = f"{key.n.bit_length()}bit"

    # Test keypair via the same rsa module the reference uses.
    (pub, priv) = rsa.newkeys(2048, poolsize=1)
    pub_pem = pub.save_pkcs1().decode()
    priv_pem = priv.save_pkcs1().decode()
    ref_rsa.add_key(pub_pem, old=False)
    fp = next(iter({k for k, v in ref_rsa._server_keys.items() if v[0] == pub}))

    # Deterministic padded block through the reference encrypt path.
    data = bytes(range(255 - 235))  # short payload like pq_inner_data
    data = bytes(20) + data  # shape: like 20-byte sha prefix + payload? keep simple below
    # Use a realistic pq_inner_data-sized payload (p_q_inner_data is 2+... ~ 320 bytes but RSA
    # input max is 255-11 padding? The reference pads to exactly 255: sha1(20)+data+rand(235-len)
    payload = bytes(range(64))
    block = ref_rsa.encrypt(fp, payload)
    assert block is not None and len(block) == 256

    # Record the plaintext (SHA1 || data || pad) via the reference private
    # op so the Go test can verify the padded construction byte-for-byte
    # without deriving the pad from ciphertext.
    priv = rsa_lib.PrivateKey.load_pkcs1(priv_pem.encode())
    m = pow(int.from_bytes(block, "big"), priv.d, pub.n)
    plain = m.to_bytes(256, "big")

    cases = [
        {"name": "embedded_fingerprints", "fingerprints": fps},
        {"name": "test_key_block",
         "public_pem": pub_pem, "private_pem": priv_pem,
         "fingerprint": fp & 0xFFFFFFFFFFFFFFFF,
         "payload": hexx(payload), "plain": hexx(plain), "block": hexx(block)},
    ]
    emit("rsa_exchange.json", cases)


def v_dh_helpers():
    """Deterministic vectors for the DH exchange primitives, all computed by
    the reference implementation: nonce-derived AES key/iv, new_nonce_hashN,
    pq factorization, and full inner-data serializations."""
    from splusthon.crypto import Factorization, AuthKey
    from splusthon import helpers as ref_helpers
    from splusthon.tl.types import PQInnerData, ClientDHInnerData

    nonce = int.from_bytes(_rng.bytes(16), "big", signed=True)
    server_nonce = int.from_bytes(_rng.bytes(16), "big", signed=True)
    new_nonce = int.from_bytes(_rng.bytes(32), "little", signed=True)

    key, iv = ref_helpers.generate_key_data_from_nonce(server_nonce, new_nonce)

    auth = AuthKey(_rng.bytes(256))
    nnh1 = auth.calc_new_nonce_hash(new_nonce, 1)
    nnh2 = auth.calc_new_nonce_hash(new_nonce, 2)

    # Factorization: semiprimes assembled from two ~32-bit primes.
    facts = []
    small_primes = [1000003, 1000033, 1000037, 1000039, 1000081,
                    1000099, 1000117, 1000121, 1000133, 1000151]
    import itertools
    for p, q in itertools.combinations(small_primes, 2):
        pq = p * q
        rp, rq = Factorization.factorize(pq)
        assert rp * rq == pq
        facts.append({"pq": str(pq), "p": str(min(rp, rq)), "q": str(max(rp, rq))})
        if len(facts) >= 10:
            break

    pq_inner = bytes(PQInnerData(
        pq=rsa_get_byte_array(int(facts[0]["pq"])),
        p=rsa_get_byte_array(int(facts[0]["p"])),
        q=rsa_get_byte_array(int(facts[0]["q"])),
        nonce=nonce, server_nonce=server_nonce, new_nonce=new_nonce))

    g_b = int.from_bytes(_rng.bytes(256), "big", signed=False) >> 16
    client_inner = bytes(ClientDHInnerData(
        nonce=nonce, server_nonce=server_nonce, retry_id=0,
        g_b=rsa_get_byte_array(g_b)))
    client_hashed = hashlib.sha1(client_inner).digest() + client_inner
    from splusthon.crypto import AES
    pad_n = (16 - len(client_hashed) % 16) % 16
    client_padded = client_hashed + _rng.bytes(pad_n if pad_n else 0)
    client_encrypted = AES.encrypt_ige(client_padded, key, iv)

    cases = [{
        "name": "dh_helpers",
        "nonce": str(nonce), "server_nonce": str(server_nonce), "new_nonce": str(new_nonce),
        "aes_key": hexx(key), "aes_iv": hexx(iv),
        "auth_key": hexx(auth.key), "nnh1": str(nnh1), "nnh2": str(nnh2),
        "factorizations": facts,
        "pq_inner_data": hexx(pq_inner),
        "client_dh_inner": hexx(client_inner),
        "client_dh_padded": hexx(client_padded),
        "client_dh_encrypted": hexx(client_encrypted),
    }]
    emit("dh_helpers.json", cases)


def rsa_get_byte_array(integer):
    from splusthon.crypto import rsa as ref_rsa
    return ref_rsa.get_byte_array(integer)


def v_string_session_malformed():
    """Malformed StringSession inputs with the reference behavior recorded:
    every one of these must be rejected (never silently accepted)."""
    import base64 as b64mod
    key = _rng.bytes(256)
    good = "1" + b64mod.urlsafe_b64encode(
        struct.pack(">BH256sH".replace("256s", ""), 2, 14) + b"im-server.splu".ljust(14)[:14] + struct.pack(">H", 443) + key
    ).decode() if False else None

    def build(dc, addr, port, k):
        a = addr.encode()
        return "1" + b64mod.urlsafe_b64encode(
            struct.pack(">BH", dc, len(a)) + a + struct.pack(">H", port) + k).decode()

    cases = []
    # unsupported version
    bad_ver = "2" + build(1, "im-server.splus.ir", 443, key)[1:]
    err = ref_session_error(bad_ver)
    cases.append({"name": "bad_version", "session": bad_ver, "python_error": err})
    # truncated payload
    trunc = build(1, "im-server.splus.ir", 443, key)
    trunc = "1" + trunc[1:20]
    cases.append({"name": "truncated", "session": trunc, "python_error": ref_session_error(trunc)})
    # corrupted base64
    cases.append({"name": "bad_base64", "session": "1!!!!not-base64!!!!", "python_error": ref_session_error("1!!!!not-base64!!!!")})
    # addr_len pointing past the payload
    corrupt = "1" + b64mod.urlsafe_b64encode(struct.pack(">BH", 1, 60000) + b"x" * 8).decode()
    cases.append({"name": "addr_len_overflow", "session": corrupt, "python_error": ref_session_error(corrupt)})
    # empty
    cases.append({"name": "empty", "session": "", "python_error": ref_session_error("")})
    # missing key entirely (payload too short)
    short = "1" + b64mod.urlsafe_b64encode(struct.pack(">BH", 1, 0) + struct.pack(">H", 443)).decode()
    cases.append({"name": "no_key", "session": short, "python_error": ref_session_error(short)})
    # valid reference points (Go must parse these)
    cases.append({"name": "valid_domain", "session": build(1, "im-server.splus.ir", 443, key), "dc": 1,
                  "address": "im-server.splus.ir", "port": 443, "key": hexx(key)})
    cases.append({"name": "valid_ipv4", "session": build(4, "149.154.167.51", 443, key), "dc": 4,
                  "address": "149.154.167.51", "port": 443, "key": hexx(key)})
    cases.append({"name": "valid_zero_key", "session": build(2, "a.io", 80, bytes(256)), "dc": 2,
                  "address": "a.io", "port": 80, "key": hexx(bytes(256))})
    emit("string_session_malformed.json", cases)


def _auth_algo(salt1, salt2, g, p_bytes):
    from splusthon.tl.types import PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow
    return PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow(
        salt1=salt1, salt2=salt2, g=g, p=p_bytes)


def _auth_acct_password(algo, srp_b, srp_id):
    # The reference names account.Password's class `Password`
    # (splusthon/tl/types/account.py:369, ctor 0x957b50fb).
    from splusthon.tl.types.account import Password
    from splusthon.tl.types import PasswordKdfAlgoUnknown, SecurePasswordKdfAlgoUnknown
    return Password(
        new_algo=PasswordKdfAlgoUnknown(), new_secure_algo=SecurePasswordKdfAlgoUnknown(),
        secure_random=b"", has_recovery=False, has_secure_values=False,
        has_password=True, current_algo=algo, srp_B=srp_b, srp_id=srp_id,
        hint=None, email_unconfirmed_pattern=None, pending_reset_date=None)


def ast_literal(src):
    """Evaluates a tuple-of-ints literal (the reference's prime bytes)."""
    import ast
    # strip leading indentation so the literal parses standalone
    lines = [ln.strip() for ln in src.strip().splitlines() if ln.strip()]
    return ast.literal_eval(" ".join(lines))


def v_auth():
    """Phase 9 authentication vectors, all produced by reference code.

    Sources (reference/SPlusthon):
      - utils.parse_phone                (splusthon/utils.py:916-923)
      - password.compute_hash            (splusthon/password.py:113-118)
      - password.compute_check           (splusthon/password.py:136-194)
      - password.check_prime_and_good    (splusthon/password.py:45-68)

    compute_check draws `a` from os.urandom (password.py:166); with the
    seeded _rng installed the unmodified reference function is fully
    deterministic, so both cases below run the REAL compute_check. The
    "server_verified" field proves the produced (A, M1) pair satisfies the
    MTProto SRP relation S = (A · v^u)^b mod p with B = (g^b + k·v) mod p
    (independent big-int math), i.e. the vector is a valid protocol run.
    """
    from splusthon import password as pwdmod
    from splusthon.utils import parse_phone

    cases = {"parse_phone": [], "pbkdf2_sha512": [], "srp": {}}

    # -- phone normalization (utils.py:916-923) --------------------------
    phone_inputs = [
        "+34 (123) 123-1234", "+341231231234", "34 123 123 1234",
        "34-123-123-1234", "(34)1231231234", 341231231234,
        "+0", "12a34", "", "  ", "+34_123", "abc",
    ]
    for raw in phone_inputs:
        out = parse_phone(raw)
        cases["parse_phone"].append({
            "input": str(raw),
            "valid": out is not None,
            "parsed": out,
        })

    # -- PBKDF2-HMAC-SHA512 primitive (password.py:109-110) --------------
    salt1 = _rng.bytes(16)
    salt2 = _rng.bytes(16)
    pw = "golden-2fa-password"
    hash2 = pwdmod.sha256(salt2, pwdmod.sha256(salt1, pw.encode(), salt1), salt2)
    dk = pwdmod.pbkdf2sha512(hash2, salt1, 100000)
    cases["pbkdf2_sha512"].append({
        "password_hash2": hexx(hash2), "salt": hexx(salt1),
        "iterations": 100000, "dklen": 64, "dk": hexx(dk),
    })

    # -- SRP (password.py:136-194) ---------------------------------------
    g = 3
    # Extract the reference's pre-validated 2048-bit prime literal
    # (password.py:46-62, local `good_prime` in check_prime_and_good) from
    # the reference source itself — never transcribed by hand.
    import inspect, re as _re
    src = inspect.getsource(pwdmod.check_prime_and_good)
    m = _re.search(r"good_prime = bytes\(\((.*?)\)\)", src, _re.S)
    assert m, "good_prime literal not found in reference source"
    p_bytes = bytes(byte for byte in ast_literal(m.group(1)))
    assert len(p_bytes) == 2048 // 8
    pwdmod.check_prime_and_good(p_bytes, g)  # must not raise (reference check)

    srp_id = 0x1122334455667788
    pw_int = int.from_bytes(p_bytes, "big")

    # Deterministic SRP run: the reference's compute_check with the seeded
    # os.urandom; record the intermediate x, the produced (A, M1), and the
    # raw 256-byte `a` draw so Go can replay the identical run bit-for-bit.
    srp_B = _rng.bytes(256)
    assert pwdmod.is_good_large(int.from_bytes(srp_B, "big"), pw_int)
    algo = _auth_algo(salt1, salt2, g, p_bytes)
    x_int = int.from_bytes(pwdmod.compute_hash(algo, pw), "big")
    req = _auth_acct_password(algo, srp_B, srp_id)
    saved_urandom = os.urandom
    draws = []

    def _capturing_urandom(n):
        b = saved_urandom(n)
        if n == 256:
            draws.append(b)
        return b

    os.urandom = _capturing_urandom
    try:
        srp = pwdmod.compute_check(req, pw)
    finally:
        os.urandom = saved_urandom
    cases["srp"] = {
        "salt1": hexx(salt1), "salt2": hexx(salt2), "g": g,
        "p": "standard", "password": pw, "srp_id": srp_id,
        "srp_B": hexx(srp_B),
        "x": hexx(x_int.to_bytes(32, "big")),
        "a_raw": hexx(draws[0]), "a_draw_count": len(draws),
        "A": hexx(srp.A), "M1": hexx(srp.M1),
    }

    # Server-side verification of a second, independent reference run:
    # draw b, derive B = (g^b + k·v) mod p, hand (p, g, B, srp_id) to the
    # reference client math, then recompute M1 from S = (A · v^u)^b mod p.
    p_for_hash = pwdmod.num_bytes_for_hash(p_bytes)
    g_for_hash = pwdmod.big_num_for_hash(g)
    k_int = int.from_bytes(pwdmod.sha256(p_for_hash, g_for_hash), "big")
    v = pow(g, x_int, pw_int)
    b_draw = int.from_bytes(_rng.bytes(256), "big")
    B2 = (pow(g, b_draw, pw_int) + k_int * v) % pw_int
    req2 = _auth_acct_password(algo, B2.to_bytes(256, "big"), srp_id)
    srp2 = pwdmod.compute_check(req2, pw)
    A2 = int.from_bytes(srp2.A, "big")
    b2_for_hash = pwdmod.num_bytes_for_hash(B2.to_bytes(256, "big"))
    u2 = int.from_bytes(pwdmod.sha256(pwdmod.big_num_for_hash(A2), b2_for_hash), "big")
    S_server = pow(A2 % pw_int * pow(v, u2, pw_int) % pw_int, b_draw, pw_int)
    M1_server = pwdmod.sha256(
        pwdmod.xor(pwdmod.sha256(p_for_hash), pwdmod.sha256(g_for_hash)),
        pwdmod.sha256(salt1), pwdmod.sha256(salt2),
        pwdmod.big_num_for_hash(A2), b2_for_hash,
        pwdmod.sha256(pwdmod.big_num_for_hash(S_server)))
    cases["srp"]["server_verified"] = M1_server == srp2.M1  # M1 attr is lowercase in the reference
    cases["srp"]["srp_id_echo"] = srp2.srp_id == srp_id

    emit("auth.json", [cases])


def ref_session_error(s):
    from splusthon.sessions.string import StringSession
    try:
        StringSession(s)
        return None
    except Exception as e:
        return type(e).__name__



def v_first_rpc():
    """Phase 22: byte-exact vector for THE first encrypted RPC.

    Reproduces the exact client->server message SuroMesh sends right after
    the DH key exchange: invokeWithLayer(LAYER, initConnection(...,
    help.getConfig)) with content_related=True (first seq_no = 1) and
    salt=0 (the pre-salt-adoption state; the server corrects via
    new_session_created / bad_server_salt). Differential target for the
    Phase 21 live observation (first encrypted RPC connection drop).
    """
    cases = []
    key = _rng.bytes(256)
    st = make_state(key)
    session = -0x0EADBEEFCAFEBAB  # fixed, arbitrary (int64)
    st.salt = 0  # salt state after DH, before server correction
    st.id = session

    req = InvokeWithLayerRequest(LAYER, InitConnectionRequest(
        api_id=1030400,
        device_model='PC 64bit',
        system_version='go',
        app_version='suromesh',
        lang_code='en',
        system_lang_code='en',
        lang_pack='',
        query=GetConfigRequest(),
    ))
    inner, mid = build_messages(st, [req])
    payload = struct.pack("<qq", 0, session) + inner
    env = st.encrypt_message_data(inner)
    msg_key = env[8:24]
    ak, ai = mts.MTProtoState._calc_key(key, msg_key, client=True)
    plain = AES.decrypt_ige(env[24:], ak, ai)
    pad = plain[len(payload):]
    body = bytes(req)
    cases.append({
        "name": "first_rpc_salt0",
        "auth_key": hexx(key), "salt": 0, "session_id": session,
        "request_body": hexx(body),
        "payload": hexx(payload), "padding": hexx(pad),
        "envelope": hexx(env), "msg_id": mid,
        "seq_no": 1, "layer": LAYER,
    })
    emit("first_rpc.json", cases)


def main():
    print(f"generating vectors (seed {SEED:#x}) -> {OUT}")
    v_constructors()
    v_primitives()
    v_strings()
    v_bytes()
    v_vectors()
    v_flags()
    v_nested()
    v_requests()
    v_responses()
    v_errors()
    v_string_session()
    v_ige()
    v_msg_key()
    v_envelope()
    v_obfuscated()
    v_roundtrip()
    v_rsa()
    v_dh_helpers()
    v_string_session_malformed()
    v_auth()
    v_first_rpc()
    print("done")


if __name__ == "__main__":
    main()
