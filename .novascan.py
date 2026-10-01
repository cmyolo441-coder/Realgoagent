#!/usr/bin/env python3
"""
novascan - advanced asynchronous web security scanner.

A single-file, dependency-free (Python 3.9+ standard library only) web
application security scanner.  It combines a crawler, a fingerprinter, a
TLS/X.509 analyser, a hand-rolled asynchronous DNS client, a passive and
active vulnerability engine and several report writers.

    ./novascan.py https://example.com --authorized -o report.html

Design notes
------------
* Everything is asyncio.  One event loop drives HTTP, DNS, TLS and port
  scanning.  Concurrency is bounded globally and per host, so a scan stays
  polite and does not exhaust file descriptors.
* The HTTP stack is implemented directly on ``asyncio.open_connection``
  (HTTP/1.1 keep-alive, chunked decoding, gzip/deflate, redirect chains,
  proxy CONNECT) because no third-party client is available.
* DNS is spoken on the wire: query construction, name compression, AXFR
  over TCP, TXT/MX/NS/SOA/CAA/SRV parsing.
* X.509 certificates are parsed from DER by hand, so the scanner reports
  signature algorithms, key sizes, SANs and extension problems without a
  crypto library.

Only scan systems you are authorised to test.  Checks that send attack
payloads, open sockets to arbitrary ports or attempt zone transfers are
gated behind ``--authorized``.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import concurrent.futures
import contextlib
import csv
import dataclasses
import datetime
import difflib
import gzip
import hashlib
import html as html_mod
import importlib.util
import inspect
import ipaddress
import json
import os
import random
import re
import socket
import ssl
import string
import struct
import sys
import time
import urllib.parse
import zlib
from collections import defaultdict, deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable, Iterator, Sequence

__version__ = "1.4.0"
__all__ = ["main", "Scanner", "HttpClient", "DnsClient", "Finding", "ScanConfig"]

# ---------------------------------------------------------------------------
# ANSI colour / terminal helpers
# ---------------------------------------------------------------------------


class C:
    """ANSI escape codes.  ``C.enabled`` is flipped off for --no-color."""

    enabled = True
    RESET = "\033[0m"
    BOLD = "\033[1m"
    DIM = "\033[2m"
    ITALIC = "\033[3m"
    UNDER = "\033[4m"
    BLINK = "\033[5m"
    RED = "\033[31m"
    GREEN = "\033[32m"
    YELLOW = "\033[33m"
    BLUE = "\033[34m"
    MAGENTA = "\033[35m"
    CYAN = "\033[36m"
    WHITE = "\033[37m"
    GRAY = "\033[90m"
    BG_RED = "\033[41m"
    BG_GREEN = "\033[42m"
    BG_YELLOW = "\033[43m"
    BG_BLUE = "\033[44m"

    @classmethod
    def wrap(cls, text: str, *codes: str) -> str:
        if not cls.enabled or not codes:
            return text
        return "".join(codes) + text + cls.RESET

    @classmethod
    def strip(cls, text: str) -> str:
        return re.sub(r"\033\[[0-9;]*m", "", text)


SEV_ORDER = ["critical", "high", "medium", "low", "info"]
SEV_RANK = {s: i for i, s in enumerate(SEV_ORDER)}
SEV_COLOR = {
    "critical": C.BG_RED + C.WHITE + C.BOLD,
    "high": C.RED + C.BOLD,
    "medium": C.YELLOW,
    "low": C.CYAN,
    "info": C.GRAY,
}
SEV_SCORE = {"critical": 9.5, "high": 7.5, "medium": 5.0, "low": 3.0, "info": 0.0}


def sev_at_least(sev: str, floor: str) -> bool:
    return SEV_RANK.get(sev, 99) <= SEV_RANK.get(floor, 99)


# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

_LEVELS = {"debug": 10, "info": 20, "warn": 30, "error": 40, "quiet": 100}


class Log:
    """Tiny level-aware logger writing to stderr (keeps stdout clean)."""

    def __init__(
        self,
        level: str = "info",
        color: bool = True,
        stream=None,
        show_time: bool = False,
    ) -> None:
        self.threshold = _LEVELS.get(level, 20)
        self.color = color
        self.stream = stream or sys.stderr
        self.show_time = show_time
        self.counts: dict[str, int] = defaultdict(int)

    # -- plumbing ---------------------------------------------------------
    def _emit(self, tag: str, text: str, codes: tuple[str, ...] = ()) -> None:
        if self.threshold >= 100:
            return
        stamp = ""
        if self.show_time:
            stamp = C.wrap(
                datetime.datetime.now().strftime("%H:%M:%S "), C.GRAY
            ) if self.color else datetime.datetime.now().strftime("%H:%M:%S ")
        head = C.wrap(tag, *codes) if self.color else tag
        line = f"{stamp}{head} {C.strip(text) if not self.color else text}"
        self.stream.write(line.rstrip("\n") + "\n")
        self.stream.flush()

    def _pass(self, level: str) -> bool:
        return _LEVELS[level] >= self.threshold

    # -- levels -----------------------------------------------------------
    def debug(self, text: str) -> None:
        if self._pass("debug"):
            self._emit("…", text, (C.GRAY,))

    def info(self, text: str) -> None:
        if self._pass("info"):
            self._emit("•", text, (C.CYAN,))

    def ok(self, text: str) -> None:
        if self._pass("info"):
            self._emit("✔", text, (C.GREEN,))
            self.counts["ok"] += 1

    def warn(self, text: str) -> None:
        if self._pass("warn"):
            self._emit("!", text, (C.YELLOW,))
            self.counts["warn"] += 1

    def error(self, text: str) -> None:
        if self._pass("error"):
            self._emit("✖", text, (C.RED, C.BOLD))
            self.counts["error"] += 1

    def vuln(self, text: str) -> None:
        if self._pass("info"):
            self._emit("▲", text, (C.RED, C.BOLD))
            self.counts["vuln"] += 1

    # -- structure --------------------------------------------------------
    def rule(self, title: str) -> None:
        if self.threshold >= 100:
            return
        bar = "─" * max(3, 66 - len(C.strip(title)))
        self.stream.write(
            (C.wrap("── ", C.BLUE) + C.wrap(title, C.BOLD, C.BLUE) + " " + C.wrap(bar, C.BLUE) + "\n")
            if self.color
            else f"-- {title} {bar}\n"
        )
        self.stream.flush()

    def blank(self) -> None:
        if self.threshold < 100:
            self.stream.write("\n")
            self.stream.flush()


class Progress:
    """Single-line progress bar with rate and ETA."""

    def __init__(self, log: Log, total: int = 0, label: str = "", width: int = 28) -> None:
        self.log = log
        self.total = max(0, total)
        self.label = label
        self.width = width
        self.done = 0
        self.started = time.monotonic()
        self._last = 0.0
        self.active = self.log.threshold < 100 and self.log.stream.isatty()

    def advance(self, n: int = 1) -> None:
        self.done += n
        self.render(force=False)

    def render(self, force: bool = True) -> None:
        if not self.active:
            return
        now = time.monotonic()
        if not force and now - self._last < 0.12:
            return
        self._last = now
        elapsed = max(1e-6, now - self.started)
        rate = self.done / elapsed
        frac = (self.done / self.total) if self.total else 0.0
        frac = min(1.0, frac)
        filled = int(self.width * frac)
        bar = "█" * filled + "░" * (self.width - filled)
        if self.total:
            eta = (self.total - self.done) / rate if rate > 0 else 0
            tail = f"{self.done}/{self.total}  {rate:5.1f}/s  eta {eta:4.0f}s"
        else:
            tail = f"{self.done}  {rate:5.1f}/s  {elapsed:4.0f}s"
        line = f"\r  {C.wrap(bar, C.CYAN)} {C.wrap(self.label, C.DIM)} {tail}   "
        self.log.stream.write(line)
        self.log.stream.flush()

    def close(self) -> None:
        if self.active:
            self.log.stream.write("\r" + " " * 100 + "\r")
            self.log.stream.flush()


# ---------------------------------------------------------------------------
# Small utilities
# ---------------------------------------------------------------------------


def sha256_hex(data: bytes | str) -> str:
    if isinstance(data, str):
        data = data.encode("utf-8", "replace")
    return hashlib.sha256(data).hexdigest()


def md5_hex(data: bytes | str) -> str:
    if isinstance(data, str):
        data = data.encode("utf-8", "replace")
    return hashlib.md5(data).hexdigest()  # noqa: S324 - fingerprinting only


def human_duration(seconds: float) -> str:
    seconds = int(seconds)
    if seconds < 60:
        return f"{seconds}s"
    m, s = divmod(seconds, 60)
    if m < 60:
        return f"{m}m{s:02d}s"
    h, m = divmod(m, 60)
    return f"{h}h{m:02d}m"


def human_bytes(n: int) -> str:
    step = 1024.0
    for unit in ("B", "KB", "MB", "GB"):
        if n < step:
            return f"{n:.0f}{unit}" if unit == "B" else f"{n:.1f}{unit}"
        n /= step
    return f"{n:.1f}TB"


def shannon_entropy(text: str) -> float:
    if not text:
        return 0.0
    counts: dict[str, int] = defaultdict(int)
    for ch in text:
        counts[ch] += 1
    n = len(text)
    ent = 0.0
    for c in counts.values():
        p = c / n
        ent -= p * (p and __import__("math").log2(p))
    return ent


def is_ip_literal(value: str) -> bool:
    try:
        ipaddress.ip_address(value.strip("[]"))
        return True
    except ValueError:
        return False


def wildcard_match(pattern: str, value: str) -> bool:
    """Glob match where ``*`` spans any characters (including dots)."""
    regex = "^" + re.escape(pattern).replace(r"\*", ".*").replace(r"\?", ".") + "$"
    return re.match(regex, value, re.IGNORECASE) is not None


def compile_globs(patterns: Sequence[str]) -> list[re.Pattern[str]]:
    out = []
    for p in patterns:
        p = p.strip()
        if not p:
            continue
        out.append(re.compile("^" + re.escape(p).replace(r"\*", ".*").replace(r"\?", ".") + "$", re.I))
    return out


def any_glob(patterns: Sequence[re.Pattern[str]], value: str) -> bool:
    return any(p.match(value) for p in patterns)


def split_multi(value: str | None) -> list[str]:
    if not value:
        return []
    return [p.strip() for p in re.split(r"[,\s]+", value) if p.strip()]


def parse_cookie_header(raw: str) -> list[dict[str, str]]:
    """Parse a ``Set-Cookie`` value into name/value plus attributes."""
    parts = [p.strip() for p in raw.split(";")]
    if not parts or "=" not in parts[0]:
        return []
    name, _, val = parts[0].partition("=")
    cookie = {"name": name.strip(), "value": val.strip(), "attrs": {}}
    for attr in parts[1:]:
        if "=" in attr:
            k, _, v = attr.partition("=")
            cookie["attrs"][k.strip().lower()] = v.strip()
        else:
            cookie["attrs"][attr.strip().lower()] = True
    return [cookie]


def decode_body(body: bytes, encoding: str) -> bytes:
    """Undo Content-Encoding.  Unknown codecs are returned untouched."""
    enc = (encoding or "").lower().strip()
    try:
        if enc in ("gzip", "x-gzip"):
            return gzip.decompress(body)
        if enc == "deflate":
            try:
                return zlib.decompress(body)
            except zlib.error:
                return zlib.decompress(body, -zlib.MAX_WBITS)
        if enc == "br":
            return body  # brotli is not in the stdlib; leave raw
    except Exception:
        return body
    return body


def read_chunked(reader: asyncio.StreamReader) -> bytes:
    """Read a ``Transfer-Encoding: chunked`` body."""
    out = bytearray()
    while True:
        line = await reader.readuntil(b"\r\n")
        size_token = line.strip().split(b";")[0]
        try:
            size = int(size_token, 16)
        except ValueError:
            break
        if size == 0:
            # trailers until blank line
            with contextlib.suppress(Exception):
                while True:
                    t = await reader.readuntil(b"\r\n")
                    if t in (b"\r\n", b"\n"):
                        break
            break
        out += await reader.readexactly(size)
        await reader.readexactly(2)  # trailing CRLF
    return bytes(out)


class TokenBucket:
    """Global request-rate limiter."""

    def __init__(self, rate: float, burst: int = 1) -> None:
        self.rate = max(0.01, rate)
        self.capacity = max(1.0, float(burst))
        self.tokens = self.capacity
        self.updated = time.monotonic()
        self._lock = asyncio.Lock()

    async def acquire(self) -> None:
        async with self._lock:
            while True:
                now = time.monotonic()
                self.tokens = min(self.capacity, self.tokens + (now - self.updated) * self.rate)
                self.updated = now
                if self.tokens >= 1.0:
                    self.tokens -= 1.0
                    return
                wait = (1.0 - self.tokens) / self.rate
                await asyncio.sleep(wait)


class HostGate:
    """Per-host concurrency + minimum delay gate."""

    def __init__(self, concurrency: int = 6, delay: float = 0.0) -> None:
        self.sem = asyncio.Semaphore(max(1, concurrency))
        self.delay = max(0.0, delay)
        self._last = 0.0
        self._lock = asyncio.Lock()

    async def __aenter__(self) -> "HostGate":
        await self.sem.acquire()
        if self.delay:
            async with self._lock:
                now = time.monotonic()
                wait = self._last + self.delay - now
                if wait > 0:
                    await asyncio.sleep(wait)
                self._last = time.monotonic()
        return self

    async def __aexit__(self, *exc) -> None:
        self.sem.release()


async def retry_async(
    coro_factory: Callable[[], Any],
    attempts: int = 3,
    base_delay: float = 0.4,
    exceptions: tuple[type[BaseException], ...] = (Exception,),
    on_retry: Callable[[int, BaseException], None] | None = None,
) -> Any:
    """Exponential backoff with full jitter."""
    last: BaseException | None = None
    for i in range(max(1, attempts)):
        try:
            return await coro_factory()
        except exceptions as exc:  # type: ignore[misc]
            last = exc
            if i == attempts - 1:
                break
            if on_retry:
                on_retry(i + 1, exc)
            await asyncio.sleep(base_delay * (2**i) * (0.5 + random.random()))
    assert last is not None
    raise last


# ---------------------------------------------------------------------------
# Minimal DER reader + X.509 certificate parser
# ---------------------------------------------------------------------------

_OID_NAMES = {
    "2.5.4.3": "CN",
    "2.5.4.4": "SN",
    "2.5.4.5": "serialNumber",
    "2.5.4.6": "C",
    "2.5.4.7": "L",
    "2.5.4.8": "ST",
    "2.5.4.9": "street",
    "2.5.4.10": "O",
    "2.5.4.11": "OU",
    "2.5.4.12": "title",
    "2.5.4.42": "GN",
    "1.2.840.113549.1.9.1": "emailAddress",
    "0.9.2342.19200300.100.1.1": "UID",
    "0.9.2342.19200300.100.1.25": "DC",
}

_OID_ALGOS = {
    "1.2.840.113549.1.1.1": "rsaEncryption",
    "1.2.840.113549.1.1.2": "md2WithRSA",
    "1.2.840.113549.1.1.4": "md5WithRSA",
    "1.2.840.113549.1.1.5": "sha1WithRSA",
    "1.2.840.113549.1.1.10": "rsassaPss",
    "1.2.840.113549.1.1.11": "sha256WithRSA",
    "1.2.840.113549.1.1.12": "sha384WithRSA",
    "1.2.840.113549.1.1.13": "sha512WithRSA",
    "1.2.840.113549.1.1.14": "sha224WithRSA",
    "1.2.840.10040.4.1": "dsa",
    "1.2.840.10040.4.3": "dsaWithSha1",
    "2.16.840.1.101.3.4.3.2": "dsaWithSha256",
    "1.2.840.10045.2.1": "ecPublicKey",
    "1.2.840.10045.3.1.7": "prime256v1",
    "1.3.132.0.34": "secp384r1",
    "1.3.132.0.35": "secp521r1",
    "1.3.132.0.10": "secp256k1",
    "1.2.840.10045.4.1": "ecdsa-with-SHA1",
    "1.2.840.10045.4.3.1": "ecdsa-with-SHA224",
    "1.2.840.10045.4.3.2": "ecdsa-with-SHA256",
    "1.2.840.10045.4.3.3": "ecdsa-with-SHA384",
    "1.2.840.10045.4.3.4": "ecdsa-with-SHA512",
    "1.3.101.112": "ed25519",
    "1.3.101.113": "ed448",
    "1.2.840.113549.1.1.11 ": "sha256WithRSA",
}

_OID_EXT = {
    "2.5.29.9": "subjectDirectoryAttributes",
    "2.5.29.14": "subjectKeyIdentifier",
    "2.5.29.15": "keyUsage",
    "2.5.29.16": "privateKeyUsagePeriod",
    "2.5.29.17": "subjectAltName",
    "2.5.29.18": "issuerAltName",
    "2.5.29.19": "basicConstraints",
    "2.5.29.20": "cRLNumber",
    "2.5.29.21": "cRLReason",
    "2.5.29.24": "invalidityDate",
    "2.5.29.27": "deltaCRLIndicator",
    "2.5.29.28": "issuingDistributionPoint",
    "2.5.29.29": "certificateIssuer",
    "2.5.29.30": "nameConstraints",
    "2.5.29.31": "cRLDistributionPoints",
    "2.5.29.32": "certificatePolicies",
    "2.5.29.35": "authorityKeyIdentifier",
    "2.5.29.36": "policyConstraints",
    "2.5.29.37": "extKeyUsage",
    "2.5.29.46": "freshestCRL",
    "2.5.29.54": "inhibitAnyPolicy",
    "1.3.6.1.5.5.7.1.1": "authorityInfoAccess",
    "1.3.6.1.5.5.7.1.24": "tlsFeature",
    "2.5.29.17 ": "subjectAltName",
    "1.3.6.1.4.1.11129.2.4.2": "signedCertificateTimestampList",
    "1.3.6.1.4.1.11129.2.4.3": "ctPoison",
}

_EKU_NAMES = {
    "1.3.6.1.5.5.7.3.1": "serverAuth",
    "1.3.6.1.5.5.7.3.2": "clientAuth",
    "1.3.6.1.5.5.7.3.3": "codeSigning",
    "1.3.6.1.5.5.7.3.4": "emailProtection",
    "1.3.6.1.5.5.7.3.8": "timeStamping",
    "2.5.29.37.0": "anyExtendedKeyUsage",
}


def der_items(data: bytes, offset: int = 0, end: int | None = None) -> Iterator[tuple[int, bytes, int, int]]:
    """Yield ``(tag, value, header_start, value_end)`` for a DER sequence."""
    end = len(data) if end is None else end
    while offset < end:
        start = offset
        tag = data[offset]
        offset += 1
        if offset >= end:
            return
        first = data[offset]
        offset += 1
        if first & 0x80:
            nbytes = first & 0x7F
            if nbytes == 0:
                return  # indefinite length: unsupported
            length = int.from_bytes(data[offset : offset + nbytes], "big")
            offset += nbytes
        else:
            length = first
        value = data[offset : offset + length]
        yield tag, value, start, offset + length
        offset += length


def der_oid(raw: bytes) -> str:
    if not raw:
        return ""
    first = raw[0]
    parts = [str(first // 40), str(first % 40)]
    value = 0
    for byte in raw[1:]:
        value = (value << 7) | (byte & 0x7F)
        if not byte & 0x80:
            parts.append(str(value))
            value = 0
    return ".".join(parts)


def der_string(raw: bytes) -> str:
    try:
        return raw.decode("utf-8")
    except UnicodeDecodeError:
        return raw.decode("latin-1", "replace")


def der_time(raw: bytes) -> datetime.datetime | None:
    text = raw.decode("ascii", "replace").strip()
    try:
        if text.endswith("Z"):
            text = text[:-1]
        if len(text) == 12:  # UTCTime YYMMDDHHMMSS
            year = int(text[0:2])
            year += 2000 if year < 50 else 1900
            return datetime.datetime(year, int(text[2:4]), int(text[4:6]), int(text[6:8]), int(text[8:10]), int(text[10:12]), tzinfo=datetime.timezone.utc)
        if len(text) == 14:  # GeneralizedTime
            return datetime.datetime(int(text[0:4]), int(text[4:6]), int(text[6:8]), int(text[8:10]), int(text[10:12]), int(text[12:14]), tzinfo=datetime.timezone.utc)
    except (ValueError, IndexError):
        return None
    return None


def parse_name(raw: bytes) -> list[tuple[str, str]]:
    """Parse an X.501 Name into a list of (attribute, value) pairs."""
    out: list[tuple[str, str]] = []
    for _tag, rdn, _s, _e in der_items(raw):
        for _t2, atv, _s2, _e2 in der_items(rdn):
            items = list(der_items(atv))
            if len(items) < 2:
                continue
            oid = der_oid(items[0][1])
            out.append((_OID_NAMES.get(oid, oid), der_string(items[1][1])))
    return out


def parse_general_names(raw: bytes) -> list[str]:
    names: list[str] = []
    for tag, value, _s, _e in der_items(raw):
        if tag == 0x82:  # dNSName
            names.append(der_string(value))
        elif tag == 0x81:  # rFC822Name
            names.append("email:" + der_string(value))
        elif tag == 0x87:  # iPAddress
            if len(value) == 4:
                names.append(str(ipaddress.IPv4Address(value)))
            elif len(value) == 16:
                names.append(str(ipaddress.IPv6Address(value)))
        elif tag == 0x86:  # uniformResourceIdentifier
            names.append("uri:" + der_string(value))
        elif tag == 0xA4:  # directoryName
            names.extend(f"{k}={v}" for k, v in parse_name(value))
        else:
            names.append(f"tag{tag}:{value.hex()[:32]}")
    return names


@dataclass
class Certificate:
    subject: dict[str, str] = field(default_factory=dict)
    issuer: dict[str, str] = field(default_factory=dict)
    serial: str = ""
    version: int = 1
    not_before: datetime.datetime | None = None
    not_after: datetime.datetime | None = None
    sig_alg: str = ""
    sig_alg_oid: str = ""
    key_alg: str = ""
    key_size: int = 0
    curve: str = ""
    san: list[str] = field(default_factory=list)
    is_ca: bool = False
    path_len: int | None = None
    key_usage: list[str] = field(default_factory=list)
    eku: list[str] = field(default_factory=list)
    ski: str = ""
    aki: str = ""
    crl_dp: list[str] = field(default_factory=list)
    ocsp: list[str] = field(default_factory=list)
    ca_issuers: list[str] = field(default_factory=list)
    policies: list[str] = field(default_factory=list)
    has_sct: bool = False
    is_precert: bool = False
    self_signed: bool = False
    signature: bytes = b""
    tbs: bytes = b""
    errors: list[str] = field(default_factory=list)

    @property
    def common_name(self) -> str:
        return self.subject.get("CN", "")

    @property
    def days_left(self) -> int | None:
        if not self.not_after:
            return None
        delta = self.not_after - datetime.datetime.now(datetime.timezone.utc)
        return delta.days

    def to_dict(self) -> dict[str, Any]:
        d = dataclasses.asdict(self)
        for key in ("not_before", "not_after"):
            d[key] = d[key].isoformat() if d[key] else None
        d["days_left"] = self.days_left
        d["signature"] = self.signature.hex()[:64]
        d["tbs"] = self.tbs.hex()[:64]
        return d


def _bitstring_names(bits: bytes, names: Sequence[str]) -> list[str]:
    if not bits:
        return []
    unused = bits[0]
    data = bits[1:]
    out = []
    total = len(data) * 8 - unused
    for i in range(min(total, len(names))):
        if data[i // 8] >> (7 - (i % 8)) & 1:
            out.append(names[i])
    return out


_KEY_USAGE_BITS = (
    "digitalSignature",
    "nonRepudiation",
    "keyEncipherment",
    "dataEncipherment",
    "keyAgreement",
    "keyCertSign",
    "cRLSign",
    "encipherOnly",
    "decipherOnly",
)


def parse_certificate(der: bytes) -> Certificate:
    """Parse a DER X.509 certificate.  Never raises."""
    cert = Certificate()
    try:
        top = list(der_items(der))
        if not top or top[0][0] != 0x30:
            cert.errors.append("not a SEQUENCE")
            return cert
        body = top[0][1]
        parts = list(der_items(body))
        if len(parts) < 2:
            cert.errors.append("truncated certificate")
            return cert
        tbs_tag, tbs, _s, _e = parts[0]
        sig_alg_raw = parts[1][1]
        cert.tbs = tbs
        cert.signature = parts[2][1] if len(parts) > 2 else b""

        # signatureAlgorithm
        try:
            sa = list(der_items(sig_alg_raw))
            cert.sig_alg_oid = der_oid(sa[0][1])
            cert.sig_alg = _OID_ALGOS.get(cert.sig_alg_oid, cert.sig_alg_oid)
        except Exception:
            pass

        fields = list(der_items(tbs))
        idx = 0
        if fields and fields[0][0] == 0xA0:  # [0] EXPLICIT version
            try:
                ver = list(der_items(fields[0][1]))
                cert.version = int.from_bytes(ver[0][1], "big") + 1
            except Exception:
                cert.version = 1
            idx = 1
        if idx < len(fields):
            cert.serial = fields[idx][1].hex()
            idx += 1
        if idx < len(fields):  # inner signature algorithm
            try:
                inner = list(der_items(fields[idx][1]))
                oid = der_oid(inner[0][1])
                if not cert.sig_alg_oid:
                    cert.sig_alg_oid = oid
                    cert.sig_alg = _OID_ALGOS.get(oid, oid)
            except Exception:
                pass
            idx += 1
        if idx < len(fields):
            cert.issuer = dict(parse_name(fields[idx][1]))
            idx += 1
        if idx < len(fields):
            try:
                v = list(der_items(fields[idx][1]))
                cert.not_before = der_time(v[0][1])
                cert.not_after = der_time(v[1][1])
            except Exception:
                cert.errors.append("unparseable validity")
            idx += 1
        if idx < len(fields):
            cert.subject = dict(parse_name(fields[idx][1]))
            idx += 1
        if idx < len(fields):  # subjectPublicKeyInfo
            try:
                spki = list(der_items(fields[idx][1]))
                alg_items = list(der_items(spki[0][1]))
                key_oid = der_oid(alg_items[0][1])
                cert.key_alg = _OID_ALGOS.get(key_oid, key_oid)
                params = alg_items[1][1] if len(alg_items) > 1 else b""
                if key_oid == "1.2.840.113549.1.1.1":  # RSA
                    bitstr = spki[1][1]
                    if bitstr:
                        seq = list(der_items(bitstr[1:]))
                        if seq:
                            modulus = int.from_bytes(seq[0][1].lstrip(b"\x00") or b"\x00", "big")
                            cert.key_size = modulus.bit_length()
                elif key_oid == "1.2.840.10045.2.1":  # EC
                    curve_oid = der_oid(params)
                    cert.curve = _OID_ALGOS.get(curve_oid, curve_oid)
                    sizes = {"prime256v1": 256, "secp384r1": 384, "secp521r1": 521, "secp256k1": 256}
                    cert.key_size = sizes.get(cert.curve, 0)
                    if not cert.key_size:
                        point = spki[1][1][1:] if spki[1][1] else b""
                        cert.key_size = ((len(point) - 1) // 2) * 8
                elif key_oid == "1.3.101.112":
                    cert.key_size = 256
                elif key_oid == "1.3.101.113":
                    cert.key_size = 448
                elif key_oid.startswith("1.2.840.10040.4"):  # DSA
                    cert.key_size = 1024
            except Exception:
                cert.errors.append("unparseable public key")
            idx += 1
        # optional [1] [2] then [3] extensions
        while idx < len(fields):
            tag, value = fields[idx][0], fields[idx][1]
            if tag == 0xA3:
                try:
                    ext_seq = list(der_items(list(der_items(value))[0][1]))
                    for _t, ext, _s2, _e2 in ext_seq:
                        items = list(der_items(ext))
                        if len(items) < 2:
                            continue
                        oid = der_oid(items[0][1])
                        critical = False
                        payload = items[-1][1]
                        if len(items) == 3:
                            critical = items[1][1] != b"\x00"
                        name = _OID_EXT.get(oid, oid)
                        if oid == "2.5.29.17":
                            cert.san = parse_general_names(payload)
                        elif oid == "2.5.29.19":
                            try:
                                bc = list(der_items(payload))
                                if bc and bc[0][0] == 0x01:
                                    cert.is_ca = bc[0][1] != b"\x00"
                                if len(bc) > 1:
                                    cert.path_len = int.from_bytes(bc[1][1], "big")
                            except Exception:
                                pass
                        elif oid == "2.5.29.15":
                            cert.key_usage = _bitstring_names(payload, _KEY_USAGE_BITS)
                        elif oid == "2.5.29.37":
                            for _t3, v3, _s3, _e3 in der_items(payload):
                                eku_oid = der_oid(v3)
                                cert.eku.append(_EKU_NAMES.get(eku_oid, eku_oid))
                        elif oid == "2.5.29.14":
                            cert.ski = payload.hex()
                        elif oid == "2.5.29.35":
                            try:
                                for _t4, v4, _s4, _e4 in der_items(payload):
                                    if v4:
                                        cert.aki = v4.hex()
                            except Exception:
                                pass
                        elif oid == "2.5.29.31":
                            for _t5, v5, _s5, _e5 in der_items(payload):
                                for _t6, v6, _s6, _e6 in der_items(v5):
                                    if v6[:7] in (b"http://", b"https://"):
                                        cert.crl_dp.append(der_string(v6))
                        elif oid == "1.3.6.1.5.5.7.1.1":
                            for _t7, v7, _s7, _e7 in der_items(payload):
                                for _t8, v8, _s8, _e8 in der_items(v7):
                                    if v8[:7] in (b"http://", b"https://"):
                                        url = der_string(v8)
                                        if _t7 == 0x86:
                                            cert.ocsp.append(url)
                                        else:
                                            cert.ca_issuers.append(url)
                        elif oid == "2.5.29.32":
                            for _t9, v9, _s9, _e9 in der_items(payload):
                                for _t10, v10, _s10, _e10 in der_items(v9):
                                    cert.policies.append(der_oid(v10))
                        elif oid == "1.3.6.1.4.1.11129.2.4.2":
                            cert.has_sct = True
                        elif oid == "1.3.6.1.4.1.11129.2.4.3":
                            cert.is_precert = True
                        if critical and oid in ("2.5.29.19", "2.5.29.15"):
                            cert.errors.append(f"critical extension {name}")
                except Exception:
                    cert.errors.append("unparseable extensions")
            idx += 1

        cert.self_signed = bool(cert.subject) and cert.subject == cert.issuer
    except Exception as exc:  # pragma: no cover - defensive
        cert.errors.append(f"parse failure: {exc}")
    return cert


# ---------------------------------------------------------------------------
# Hand-rolled asynchronous DNS client
# ---------------------------------------------------------------------------

QTYPE: dict[int, str] = {
    1: "A",
    2: "NS",
    5: "CNAME",
    6: "SOA",
    12: "PTR",
    15: "MX",
    16: "TXT",
    17: "RP",
    18: "AFSDB",
    24: "SIG",
    25: "KEY",
    28: "AAAA",
    29: "LOC",
    33: "SRV",
    35: "NAPTR",
    39: "DNAME",
    43: "DS",
    44: "SSHFP",
    46: "RRSIG",
    47: "NSEC",
    48: "DNSKEY",
    49: "DHCID",
    50: "NSEC3",
    51: "NSEC3PARAM",
    52: "TLSA",
    59: "CDS",
    60: "CDNSKEY",
    61: "OPENPGPKEY",
    62: "CSYNC",
    63: "ZONEMD",
    64: "SVCB",
    65: "HTTPS",
    99: "SPF",
    108: "EUI48",
    249: "TKEY",
    250: "TSIG",
    256: "URI",
    257: "CAA",
    32768: "TA",
    32769: "DLV",
}
RTYPE: dict[str, int] = {v: k for k, v in QTYPE.items()}
RTYPE.update({"AXFR": 252, "IXFR": 251, "ANY": 255, "OPT": 41})


def dns_encode_name(name: str) -> bytes:
    out = bytearray()
    name = name.rstrip(".")
    if name:
        for label in name.split("."):
            if not label:
                continue
            try:
                raw = label.encode("ascii")
            except UnicodeEncodeError:
                try:
                    raw = label.encode("idna")
                except Exception:
                    raw = label.encode("utf-8")
            if len(raw) > 63:
                raw = raw[:63]
            out.append(len(raw))
            out += raw
    out.append(0)
    return bytes(out)


def dns_decode_name(data: bytes, offset: int) -> tuple[str, int]:
    labels: list[str] = []
    jumped = False
    end = offset
    guard = 0
    while offset < len(data):
        guard += 1
        if guard > 128:
            break
        length = data[offset]
        if length == 0:
            offset += 1
            if not jumped:
                end = offset
            break
        if length & 0xC0 == 0xC0:
            pointer = ((length & 0x3F) << 8) | data[offset + 1]
            if not jumped:
                end = offset + 2
            offset = pointer
            jumped = True
            continue
        offset += 1
        labels.append(data[offset : offset + length].decode("utf-8", "replace"))
        offset += length
        if not jumped:
            end = offset
    return ".".join(labels), end


def dns_build_query(name: str, qtype: int, msg_id: int | None = None, recursion: bool = True) -> bytes:
    msg_id = random.randint(0, 0xFFFF) if msg_id is None else msg_id
    flags = 0x0100 if recursion else 0x0000
    header = struct.pack("!HHHHHH", msg_id, flags, 1, 0, 0, 0)
    question = dns_encode_name(name) + struct.pack("!HH", qtype, 1)
    return header + question


@dataclass
class DnsRecord:
    name: str
    rtype: int
    ttl: int
    rdata: Any

    @property
    def type_name(self) -> str:
        return QTYPE.get(self.rtype, str(self.rtype))


@dataclass
class DnsMessage:
    id: int
    flags: int
    rcode: int
    questions: list[tuple[str, int]]
    answers: list[DnsRecord]
    authorities: list[DnsRecord]
    additionals: list[DnsRecord]
    truncated: bool = False

    @property
    def ok(self) -> bool:
        return self.rcode in (0, 3)

    def by_type(self, rtype: int | str) -> list[DnsRecord]:
        want = RTYPE.get(rtype, rtype) if isinstance(rtype, str) else rtype
        return [r for r in self.answers if r.rtype == want]

    def values(self, rtype: int | str) -> list[Any]:
        return [r.rdata for r in self.by_type(rtype)]


def dns_parse_message(data: bytes) -> DnsMessage:
    if len(data) < 12:
        raise ValueError("short DNS message")
    msg_id, flags, qd, an, ns, ar = struct.unpack("!HHHHHH", data[:12])
    msg = DnsMessage(msg_id, flags, flags & 0x0F, [], [], [], [], bool(flags & 0x0200))
    offset = 12
    for _ in range(qd):
        name, offset = dns_decode_name(data, offset)
        if offset + 4 > len(data):
            break
        qtype, _qclass = struct.unpack("!HH", data[offset : offset + 4])
        offset += 4
        msg.questions.append((name, qtype))
    for count, bucket in ((an, msg.answers), (ns, msg.authorities), (ar, msg.additionals)):
        for _ in range(count):
            if offset >= len(data):
                break
            name, offset = dns_decode_name(data, offset)
            if offset + 10 > len(data):
                break
            rtype, rclass, ttl, rdlen = struct.unpack("!HHIH", data[offset : offset + 10])
            offset += 10
            rdata_raw = data[offset : offset + rdlen]
            offset += rdlen
            try:
                rdata = _dns_parse_rdata(data, offset - rdlen, rtype, rdlen)
            except Exception:
                rdata = rdata_raw
            bucket.append(DnsRecord(name, rtype, ttl, rdata))
    return msg


def _dns_parse_rdata(full: bytes, offset: int, rtype: int, rdlen: int) -> Any:
    raw = full[offset : offset + rdlen]
    if rtype == 1 and rdlen == 4:
        return str(ipaddress.IPv4Address(raw))
    if rtype == 28 and rdlen == 16:
        return str(ipaddress.IPv6Address(raw))
    if rtype in (2, 5, 12, 39):
        name, _ = dns_decode_name(full, offset)
        return name
    if rtype == 15:  # MX
        pref = struct.unpack("!H", raw[:2])[0]
        name, _ = dns_decode_name(full, offset + 2)
        return (pref, name)
    if rtype == 33:  # SRV
        prio, weight, port = struct.unpack("!HHH", raw[:6])
        name, _ = dns_decode_name(full, offset + 6)
        return (prio, weight, port, name)
    if rtype == 16:  # TXT
        parts: list[str] = []
        pos = 0
        while pos < len(raw):
            ln = raw[pos]
            parts.append(raw[pos + 1 : pos + 1 + ln].decode("utf-8", "replace"))
            pos += 1 + ln
        return "".join(parts)
    if rtype == 6:  # SOA
        mname, pos = dns_decode_name(full, offset)
        rname, pos = dns_decode_name(full, pos)
        serial, refresh, retry, expire, minimum = struct.unpack("!IIIII", full[pos : pos + 20])
        return {"mname": mname, "rname": rname, "serial": serial, "refresh": refresh, "retry": retry, "expire": expire, "minimum": minimum}
    if rtype == 257:  # CAA
        flags = raw[0]
        tag_len = raw[1]
        tag = raw[2 : 2 + tag_len].decode("ascii", "replace")
        value = raw[2 + tag_len :].decode("utf-8", "replace")
        return (flags, tag, value)
    if rtype == 48:  # DNSKEY
        flags, proto, algo = struct.unpack("!HBB", raw[:4])
        return {"flags": flags, "protocol": proto, "algorithm": algo, "key": base64.b64encode(raw[4:]).decode()[:48]}
    if rtype == 43:  # DS
        keytag, algo, digtype = struct.unpack("!HBB", raw[:4])
        return {"keytag": keytag, "algorithm": algo, "digest_type": digtype, "digest": raw[4:].hex()}
    if rtype == 52:  # TLSA
        usage, selector, mtype = struct.unpack("!BBB", raw[:3])
        return {"usage": usage, "selector": selector, "matching_type": mtype, "data": raw[3:].hex()}
    if rtype == 35:  # NAPTR
        order, pref = struct.unpack("!HH", raw[:4])
        pos = 4
        fields = []
        for _ in range(3):
            ln = raw[pos]
            fields.append(raw[pos + 1 : pos + 1 + ln].decode("ascii", "replace"))
            pos += 1 + ln
        repl, _ = dns_decode_name(full, offset + pos)
        return {"order": order, "pref": pref, "flags": fields[0], "service": fields[1], "regexp": fields[2], "replacement": repl}
    if rtype == 65:  # HTTPS/SVCB
        prio = struct.unpack("!H", raw[:2])[0]
        name, _ = dns_decode_name(full, offset + 2)
        return {"priority": prio, "target": name, "params": raw[2 + len(name) :].hex()}
    return raw


class DnsClient:
    """Async DNS client over UDP with TCP fallback, plus a resolution cache."""

    def __init__(self, log: Log, timeout: float = 4.0, servers: Sequence[str] | None = None, cache_ttl: float = 300.0) -> None:
        self.log = log
        self.timeout = timeout
        self.servers = list(servers) if servers else _system_resolvers()
        self.cache_ttl = cache_ttl
        self._cache: dict[tuple[str, int], tuple[float, DnsMessage | None]] = {}
        self._inflight: dict[tuple[str, int], asyncio.Task] = {}
        self._sem = asyncio.Semaphore(24)
        self.stats = {"queries": 0, "cache_hits": 0, "errors": 0}

    async def query(self, name: str, qtype: int | str, use_cache: bool = True) -> DnsMessage | None:
        if isinstance(qtype, str):
            qtype = RTYPE.get(qtype.upper(), 1)
        key = (name.rstrip(".").lower(), qtype)
        if use_cache:
            hit = self._cache.get(key)
            if hit and hit[0] > time.monotonic():
                self.stats["cache_hits"] += 1
                return hit[1]
            if key in self._inflight:
                return await asyncio.shield(self._inflight[key])
        task = asyncio.ensure_future(self._query_uncached(name, qtype))
        self._inflight[key] = task
        try:
            result = await task
        finally:
            self._inflight.pop(key, None)
        if use_cache:
            self._cache[key] = (time.monotonic() + self.cache_ttl, result)
        return result

    async def _query_uncached(self, name: str, qtype: int) -> DnsMessage | None:
        async with self._sem:
            self.stats["queries"] += 1
            packet = dns_build_query(name, qtype)
            for attempt in range(2):
                try:
                    data = await self._exchange_udp(packet)
                    if data:
                        msg = dns_parse_message(data)
                        if not msg.truncated or qtype in (RTYPE["AXFR"],):
                            return msg
                    return await self._exchange_tcp(packet)
                except (socket.gaierror, OSError, asyncio.TimeoutError, ValueError):
                    if attempt == 1:
                        self.stats["errors"] += 1
                        return None
        return None

    async def _exchange_udp(self, packet: bytes) -> bytes | None:
        loop = asyncio.get_running_loop()
        last_error: Exception | None = None
        for server in self.servers:
            try:
                fut = loop.create_datagram_endpoint(lambda: _DnsProtocol(), remote_addr=(server, 53))
                transport, protocol = await asyncio.wait_for(fut, timeout=self.timeout)
                try:
                    protocol.send(packet)
                    data = await asyncio.wait_for(protocol.response(), timeout=self.timeout)
                    return data
                finally:
                    transport.close()
            except (OSError, asyncio.TimeoutError) as exc:
                last_error = exc
                continue
        if last_error:
            raise last_error
        return None

    async def _exchange_tcp(self, packet: bytes) -> bytes | None:
        for server in self.servers:
            try:
                reader, writer = await asyncio.wait_for(
                    asyncio.open_connection(server, 53), timeout=self.timeout
                )
                try:
                    writer.write(struct.pack("!H", len(packet)) + packet)
                    await writer.drain()
                    header = await asyncio.wait_for(reader.readexactly(2), timeout=self.timeout)
                    length = struct.unpack("!H", header)[0]
                    data = await asyncio.wait_for(reader.readexactly(length), timeout=self.timeout)
                    return data
                finally:
                    writer.close()
                    with contextlib.suppress(Exception):
                        await writer.wait_closed()
            except (OSError, asyncio.TimeoutError, asyncio.IncompleteReadError):
                continue
        return None

    async def axfr(self, zone: str) -> list[DnsRecord]:
        """Attempt a zone transfer.  Returns [] when refused."""
        packet = dns_build_query(zone, RTYPE["AXFR"], recursion=False)
        records: list[DnsRecord] = []
        for server in self.servers:
            try:
                reader, writer = await asyncio.wait_for(
                    asyncio.open_connection(server, 53), timeout=self.timeout * 2
                )
                try:
                    writer.write(struct.pack("!H", len(packet)) + packet)
                    await writer.drain()
                    buffer = b""
                    deadline = time.monotonic() + self.timeout * 3
                    while time.monotonic() < deadline:
                        header = await asyncio.wait_for(reader.readexactly(2), timeout=self.timeout)
                        length = struct.unpack("!H", header)[0]
                        buffer += await asyncio.wait_for(reader.readexactly(length), timeout=self.timeout)
                        try:
                            msg = dns_parse_message(buffer)
                        except ValueError:
                            continue
                        records.extend(msg.answers)
                        if any(r.rtype == 6 for r in msg.answers):
                            return records
                        buffer = b""
                finally:
                    writer.close()
                    with contextlib.suppress(Exception):
                        await writer.wait_closed()
            except (OSError, asyncio.TimeoutError, asyncio.IncompleteReadError):
                continue
        return records

    # -- convenience ------------------------------------------------------
    async def resolve(self, name: str, rtype: str = "A") -> list[Any]:
        msg = await self.query(name, rtype)
        if not msg or msg.rcode not in (0, 3):
            return []
        return [r.rdata for r in msg.answers if r.rtype == RTYPE.get(rtype.upper(), -1)]

    async def resolve_all(self, name: str, rtype: str = "A") -> list[Any]:
        """Follow CNAME chains and collect every matching record."""
        seen: set[str] = set()
        out: list[Any] = []
        queue = [name]
        while queue:
            current = queue.pop(0)
            if current in seen or len(seen) > 12:
                continue
            seen.add(current)
            msg = await self.query(current, rtype)
            if not msg:
                continue
            for rec in msg.answers:
                if rec.rtype == RTYPE.get("CNAME"):
                    queue.append(rec.rdata)
                elif rec.rtype == RTYPE.get(rtype.upper(), -1):
                    out.append(rec.rdata)
            if out:
                break
        return out

    async def reverse(self, ip: str) -> list[str]:
        try:
            addr = ipaddress.ip_address(ip)
        except ValueError:
            return []
        if addr.version == 4:
            name = ".".join(reversed(str(addr).split("."))) + ".in-addr.arpa"
        else:
            name = addr.reverse_pointer
        return await self.resolve(name, "PTR")

    async def txt(self, name: str) -> list[str]:
        return [str(v) for v in await self.resolve(name, "TXT")]


class _DnsProtocol(asyncio.DatagramProtocol):
    def __init__(self) -> None:
        self._fut: asyncio.Future[bytes] | None = None

    def connection_made(self, transport) -> None:
        self.transport = transport

    def send(self, packet: bytes) -> None:
        self.transport.sendto(packet)

    def response(self) -> asyncio.Future[bytes]:
        self._fut = asyncio.get_running_loop().create_future()
        return self._fut

    def datagram_received(self, data: bytes, addr) -> None:
        if self._fut and not self._fut.done():
            self._fut.set_result(data)

    def error_received(self, exc) -> None:
        if self._fut and not self._fut.done():
            self._fut.set_exception(exc)

    def connection_lost(self, exc) -> None:
        if self._fut and not self._fut.done():
            self._fut.set_exception(exc or ConnectionError("closed"))


def _system_resolvers() -> list[str]:
    servers: list[str] = []
    for path in ("/etc/resolv.conf",):
        try:
            for line in Path(path).read_text().splitlines():
                line = line.strip()
                if line.startswith("nameserver"):
                    parts = line.split()
                    if len(parts) > 1 and parts[1] not in servers:
                        servers.append(parts[1])
        except OSError:
            pass
    if not servers:
        servers = ["1.1.1.1", "8.8.8.8"]
    return servers[:4]


# ---------------------------------------------------------------------------
# Asynchronous HTTP/1.1 client (stdlib only)
# ---------------------------------------------------------------------------

DEFAULT_UA = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/124.0.0.0 Safari/537.36"
)
SCANNER_UA = f"novascan/{__version__} (+https://github.com/novascan)"


@dataclass
class HttpResponse:
    url: str
    status: int
    reason: str
    headers: dict[str, str]
    body: bytes
    elapsed: float
    ip: str = ""
    tls: dict[str, Any] | None = None
    redirects: list[str] = field(default_factory=list)
    request_headers: dict[str, str] = field(default_factory=dict)
    error: str = ""

    @property
    def ok(self) -> bool:
        return 200 <= self.status < 400 and not self.error

    @property
    def text(self) -> str:
        return self.body.decode(self.charset, "replace")

    @property
    def charset(self) -> str:
        ctype = self.headers.get("content-type", "")
        m = re.search(r"charset=([\w\-]+)", ctype, re.I)
        return m.group(1) if m else "utf-8"

    @property
    def content_type(self) -> str:
        return self.headers.get("content-type", "").split(";")[0].strip().lower()

    @property
    def length(self) -> int:
        return len(self.body)

    def header(self, name: str, default: str = "") -> str:
        return self.headers.get(name.lower(), default)

    def has_header(self, name: str) -> bool:
        return name.lower() in self.headers

    def set_cookies(self) -> list[dict[str, str]]:
        out: list[dict[str, str]] = []
        for raw in self.headers.get("set-cookie", "").split("\n"):
            raw = raw.strip()
            if raw:
                out.extend(parse_cookie_header(raw))
        return out

    def to_dict(self) -> dict[str, Any]:
        return {
            "url": self.url,
            "status": self.status,
            "reason": self.reason,
            "headers": self.headers,
            "length": self.length,
            "elapsed_ms": round(self.elapsed * 1000, 1),
            "ip": self.ip,
            "content_type": self.content_type,
            "redirects": self.redirects,
            "error": self.error,
        }


class _HttpConnection:
    """One keep-alive TCP (optionally TLS) connection."""

    __slots__ = ("reader", "writer", "host", "port", "tls", "created", "uses", "broken")

    def __init__(self, reader, writer, host: str, port: int, tls: bool) -> None:
        self.reader = reader
        self.writer = writer
        self.host = host
        self.port = port
        self.tls = tls
        self.created = time.monotonic()
        self.uses = 0
        self.broken = False

    async def close(self) -> None:
        if self.writer is None:
            return
        try:
            self.writer.close()
            await asyncio.wait_for(self.writer.wait_closed(), timeout=2)
        except Exception:
            pass
        self.writer = None


class HttpClient:
    """
    Connection-pooling HTTP/1.1 client.

    Features: keep-alive reuse, chunked decoding, gzip/deflate, redirect
    following, proxy CONNECT tunnels, per-host gating, global rate limiting,
    retries with jitter, and TLS handshake metadata capture.
    """

    def __init__(
        self,
        log: Log,
        timeout: float = 15.0,
        max_redirects: int = 5,
        user_agent: str = DEFAULT_UA,
        verify_tls: bool = True,
        proxy: str | None = None,
        rate_limit: float = 0.0,
        host_concurrency: int = 6,
        host_delay: float = 0.0,
        max_connections: int = 64,
        follow_redirects: bool = True,
        headers: dict[str, str] | None = None,
    ) -> None:
        self.log = log
        self.timeout = timeout
        self.max_redirects = max_redirects
        self.user_agent = user_agent
        self.verify_tls = verify_tls
        self.proxy = proxy
        self.follow_redirects = follow_redirects
        self.extra_headers = headers or {}
        self.bucket = TokenBucket(rate_limit) if rate_limit else None
        self.host_concurrency = host_concurrency
        self.host_delay = host_delay
        self._gates: dict[str, HostGate] = {}
        self._pools: dict[tuple[str, int, bool], deque[_HttpConnection]] = defaultdict(deque)
        self._pool_locks: dict[tuple[str, int, bool], asyncio.Lock] = defaultdict(asyncio.Lock)
        self._sem = asyncio.Semaphore(max_connections)
        self._ssl_cache: dict[str, ssl.SSLContext] = {}
        self.stats = {"requests": 0, "retries": 0, "errors": 0, "bytes": 0, "reused": 0}
        self._closed = False

    # -- lifecycle --------------------------------------------------------
    async def close(self) -> None:
        self._closed = True
        for pool in self._pools.values():
            while pool:
                conn = pool.popleft()
                await conn.close()
        self._pools.clear()

    async def __aenter__(self) -> "HttpClient":
        return self

    async def __aexit__(self, *exc) -> None:
        await self.close()

    def gate(self, host: str) -> HostGate:
        gate = self._gates.get(host)
        if gate is None:
            gate = HostGate(self.host_concurrency, self.host_delay)
            self._gates[host] = gate
        return gate

    # -- TLS --------------------------------------------------------------
    def ssl_context(self, host: str) -> ssl.SSLContext:
        ctx = self._ssl_cache.get(host)
        if ctx:
            return ctx
        if self.verify_tls:
            ctx = ssl.create_default_context()
        else:
            ctx = ssl._create_unverified_context()
        ctx.check_hostname = self.verify_tls
        ctx.verify_mode = ssl.CERT_REQUIRED if self.verify_tls else ssl.CERT_NONE
        ctx.minimum_version = ssl.TLSVersion.TLSv1_2 if hasattr(ssl, "TLSVersion") else ctx.minimum_version
        try:
            ctx.set_alpn_protocols(["h2", "http/1.1"])
        except (NotImplementedError, AttributeError):
            pass
        self._ssl_cache[host] = ctx
        return ctx

    # -- connection pool --------------------------------------------------
    async def _acquire_conn(self, host: str, port: int, use_tls: bool, server_name: str) -> _HttpConnection:
        key = (host, port, use_tls)
        async with self._pool_locks[key]:
            now = time.monotonic()
            while self._pools[key]:
                conn = self._pools[key].popleft()
                if conn.writer is None or conn.broken or now - conn.created > 90:
                    await conn.close()
                    continue
                if conn.reader.at_eof():
                    await conn.close()
                    continue
                self.stats["reused"] += 1
                return conn
        return await self._dial(host, port, use_tls, server_name)

    async def _dial(self, host: str, port: int, use_tls: bool, server_name: str) -> _HttpConnection:
        target_host, target_port = host, port
        if self.proxy:
            target_host, target_port = self._proxy_target()
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(target_host, target_port), timeout=self.timeout
        )
        if self.proxy:
            await self._connect_tunnel(reader, writer, host, port)
        if use_tls:
            ctx = self.ssl_context(server_name)
            try:
                await asyncio.wait_for(
                    writer.start_tls(ctx, server_hostname=server_name), timeout=self.timeout
                )
            except (ssl.SSLError, ssl.CertificateError, OSError) as exc:
                with contextlib.suppress(Exception):
                    writer.close()
                raise
        conn = _HttpConnection(reader, writer, host, port, use_tls)
        return conn

    def _proxy_target(self) -> tuple[str, int]:
        assert self.proxy
        parsed = urllib.parse.urlparse(self.proxy if "://" in self.proxy else "http://" + self.proxy)
        return parsed.hostname or "127.0.0.1", parsed.port or 8080

    async def _connect_tunnel(self, reader, writer, host: str, port: int) -> None:
        request = f"CONNECT {host}:{port} HTTP/1.1\r\nHost: {host}:{port}\r\n\r\n"
        writer.write(request.encode("ascii"))
        await writer.drain()
        status_line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout=self.timeout)
        if b" 200" not in status_line:
            raise OSError(f"proxy tunnel refused: {status_line!r}")
        while True:
            line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout=self.timeout)
            if line in (b"\r\n", b"\n"):
                break

    async def _release_conn(self, conn: _HttpConnection, reusable: bool) -> None:
        if not reusable or conn.broken or self._closed:
            await conn.close()
            return
        key = (conn.host, conn.port, conn.tls)
        self._pools[key].append(conn)

    # -- request ----------------------------------------------------------
    async def request(
        self,
        method: str,
        url: str,
        headers: dict[str, str] | None = None,
        body: bytes | str | None = None,
        allow_redirects: bool | None = None,
        timeout: float | None = None,
        max_body: int = 8 * 1024 * 1024,
    ) -> HttpResponse:
        if allow_redirects is None:
            allow_redirects = self.follow_redirects
        redirects: list[str] = []
        current = url
        method_now = method
        body_now = body
        headers_now = dict(headers or {})
        for _hop in range(self.max_redirects + 1):
            response = await self._single(method_now, current, headers_now, body_now, timeout, max_body)
            if response.error:
                response.redirects = redirects
                return response
            if allow_redirects and response.status in (301, 302, 303, 307, 308) and response.header("location"):
                location = urllib.parse.urljoin(current, response.header("location"))
                redirects.append(f"{response.status} -> {location}")
                if response.status == 303 and method_now not in ("GET", "HEAD"):
                    method_now, body_now = "GET", None
                current = location
                headers_now = dict(headers or {})
                continue
            response.redirects = redirects
            return response
        return HttpResponse(url, 0, "too many redirects", {}, b"", 0.0, error="too many redirects")

    async def _single(
        self,
        method: str,
        url: str,
        headers: dict[str, str],
        body: bytes | str | None,
        timeout: float | None,
        max_body: int,
    ) -> HttpResponse:
        parsed = urllib.parse.urlsplit(url)
        scheme = parsed.scheme or "http"
        if scheme not in ("http", "https"):
            return HttpResponse(url, 0, "", {}, b"", 0.0, error=f"unsupported scheme {scheme}")
        host = parsed.hostname or ""
        port = parsed.port or (443 if scheme == "https" else 80)
        use_tls = scheme == "https"
        if not host:
            return HttpResponse(url, 0, "", {}, b"", 0.0, error="no host")
        if is_ip_literal(host):
            ip = host.strip("[]")
        else:
            ip = await self._resolve(host)
            if not ip:
                return HttpResponse(url, 0, "", {}, b"", 0.0, error=f"cannot resolve {host}")

        if self.bucket:
            await self.bucket.acquire()
        async with self._sem:
            async with self.gate(host):
                started = time.monotonic()
                last_error = ""
                for attempt in range(3):
                    conn: _HttpConnection | None = None
                    try:
                        conn = await self._acquire_conn(host, port, use_tls, host)
                        raw_headers = self._build_headers(method, parsed, headers, body)
                        conn.writer.write(raw_headers)
                        await conn.writer.drain()
                        status, reason, resp_headers, resp_body, tls_info = await self._read_response(
                            conn, timeout or self.timeout, max_body
                        )
                        elapsed = time.monotonic() - started
                        self.stats["requests"] += 1
                        self.stats["bytes"] += len(resp_body)
                        reusable = self._is_reusable(resp_headers, status)
                        await self._release_conn(conn, reusable)
                        conn = None
                        return HttpResponse(
                            url=url,
                            status=status,
                            reason=reason,
                            headers=resp_headers,
                            body=resp_body,
                            elapsed=elapsed,
                            ip=ip,
                            tls=tls_info,
                            request_headers=self._headers_to_dict(raw_headers),
                        )
                    except (ConnectionError, asyncio.IncompleteReadError, OSError, asyncio.TimeoutError) as exc:
                        last_error = f"{type(exc).__name__}: {exc}"
                        if conn is not None:
                            conn.broken = True
                            await conn.close()
                        if attempt < 2:
                            self.stats["retries"] += 1
                            await asyncio.sleep(0.25 * (attempt + 1) * (0.5 + random.random()))
                    except Exception as exc:  # pragma: no cover - defensive
                        last_error = f"{type(exc).__name__}: {exc}"
                        if conn is not None:
                            conn.broken = True
                            await conn.close()
                        break
                self.stats["errors"] += 1
                self.log.debug(f"{method} {url} failed: {last_error}")
                return HttpResponse(url, 0, "", {}, b"", time.monotonic() - started, ip=ip, error=last_error)

    async def _resolve(self, host: str) -> str:
        if not hasattr(self, "_dns"):
            self._dns: DnsClient | None = None
        if self._dns is None:
            try:
                loop = asyncio.get_running_loop()
                infos = await loop.getaddrinfo(host, None, family=0, type=socket.SOCK_STREAM)
                return infos[0][4][0] if infos else ""
            except socket.gaierror:
                return ""
        records = await self._dns.resolve(host, "A")
        return str(records[0]) if records else ""

    def _build_headers(self, method: str, parsed: urllib.parse.SplitResult, extra: dict[str, str], body: bytes | str | None) -> bytes:
        path = parsed.path or "/"
        if parsed.query:
            path += "?" + parsed.query
        lines = [f"{method.upper()} {path} HTTP/1.1"]
        headers = {
            "Host": parsed.netloc,
            "User-Agent": self.user_agent,
            "Accept": "*/*",
            "Accept-Encoding": "gzip, deflate",
            "Connection": "keep-alive",
        }
        headers.update(self.extra_headers)
        headers.update({k: v for k, v in extra.items() if v is not None})
        payload = b"" if body is None else (body.encode() if isinstance(body, str) else body)
        if method.upper() in ("POST", "PUT", "PATCH") and payload:
            headers.setdefault("Content-Type", "application/x-www-form-urlencoded")
            headers["Content-Length"] = str(len(payload))
        for key, value in headers.items():
            lines.append(f"{key}: {value}")
        raw = ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1", "replace")
        return raw + payload

    @staticmethod
    def _headers_to_dict(raw: bytes) -> dict[str, str]:
        out: dict[str, str] = {}
        try:
            head = raw.split(b"\r\n\r\n", 1)[0].decode("latin-1", "replace")
            for line in head.split("\r\n")[1:]:
                if ":" in line:
                    k, _, v = line.partition(":")
                    out[k.strip()] = v.strip()
        except Exception:
            pass
        return out

    @staticmethod
    def _is_reusable(headers: dict[str, str], status: int) -> bool:
        if headers.get("connection", "").lower() == "close":
            return False
        if headers.get("transfer-encoding", "").lower() == "chunked":
            return True
        if "content-length" in headers:
            return True
        return status in (204, 304) or 100 <= status < 200

    async def _read_response(
        self, conn: _HttpConnection, timeout: float, max_body: int
    ) -> tuple[int, str, dict[str, str], bytes, dict[str, Any] | None]:
        reader = conn.reader
        header_blob = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=timeout)
        lines = header_blob.split(b"\r\n")
        status_line = lines[0].decode("latin-1", "replace")
        parts = status_line.split(" ", 2)
        status = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
        reason = parts[2] if len(parts) > 2 else ""
        headers: dict[str, str] = {}
        for line in lines[1:]:
            if not line:
                continue
            text = line.decode("latin-1", "replace")
            if ":" not in text:
                continue
            key, _, value = text.partition(":")
            key = key.strip()
            lkey = key.lower()
            if lkey == "set-cookie":
                headers[lkey] = (headers.get(lkey, "") + "\n" + value.strip()).strip("\n")
            else:
                headers[lkey] = value.strip()
        te = headers.get("transfer-encoding", "").lower()
        if te == "chunked":
            body = await asyncio.wait_for(read_chunked(reader), timeout=timeout)
        elif "content-length" in headers:
            try:
                length = int(headers["content-length"])
            except ValueError:
                length = 0
            body = await asyncio.wait_for(reader.readexactly(min(length, max_body)), timeout=timeout)
            if length > max_body:
                conn.broken = True
        elif status in (204, 304) or 100 <= status < 200 or method_is_head(conn):
            body = b""
        else:
            body = await asyncio.wait_for(reader.read(max_body), timeout=timeout)
        if headers.get("content-encoding"):
            body = decode_body(body, headers["content-encoding"])
        tls_info = None
        ssl_object = conn.writer.get_extra_info("sslobject")
        if ssl_object is not None:
            tls_info = describe_tls(ssl_object)
        return status, reason, headers, body, tls_info


def method_is_head(conn: _HttpConnection) -> bool:
    return False


def describe_tls(ssl_object) -> dict[str, Any]:
    """Extract negotiated TLS parameters from an ssl.SSLObject."""
    info: dict[str, Any] = {}
    try:
        info["version"] = ssl_object.version()
    except Exception:
        info["version"] = None
    try:
        cipher = ssl_object.cipher()
        info["cipher"] = cipher[0] if cipher else None
        info["cipher_bits"] = cipher[2] if cipher else None
        info["cipher_protocol"] = cipher[1] if cipher else None
    except Exception:
        pass
    try:
        info["alpn"] = ssl_object.selected_alpn_protocol()
    except Exception:
        info["alpn"] = None
    try:
        der = ssl_object.getpeercert(binary_form=True)
        if der:
            cert = parse_certificate(der)
            info["cert"] = cert
            info["cert_fingerprint_sha256"] = sha256_hex(der)
    except Exception:
        pass
    try:
        info["compression"] = ssl_object.compression()
    except Exception:
        info["compression"] = None
    return info


# ---------------------------------------------------------------------------
# Data model: targets, findings, evidence
# ---------------------------------------------------------------------------


@dataclass
class Target:
    url: str
    scheme: str
    host: str
    port: int
    path: str = "/"
    ip: str = ""
    base_url: str = ""

    @classmethod
    def from_url(cls, url: str) -> "Target":
        if "://" not in url:
            url = "http://" + url
        parsed = urllib.parse.urlsplit(url)
        scheme = parsed.scheme or "http"
        host = parsed.hostname or ""
        port = parsed.port or (443 if scheme == "https" else 80)
        base = f"{scheme}://{parsed.netloc}"
        return cls(url=url, scheme=scheme, host=host, port=port, path=parsed.path or "/", base_url=base)

    def url_for(self, path: str, query: str = "") -> str:
        if path.startswith(("http://", "https://")):
            return path
        if not path.startswith("/"):
            path = "/" + path
        out = f"{self.scheme}://{self.host}:{self.port}{path}" if self._explicit_port() else f"{self.scheme}://{self.host}{path}"
        if query:
            out += "?" + query
        return out

    def _explicit_port(self) -> bool:
        return self.port not in (80, 443)

    def same_origin(self, other: str) -> bool:
        try:
            parsed = urllib.parse.urlsplit(other if "://" in other else "http://" + other)
            return parsed.hostname == self.host and (parsed.port or (443 if parsed.scheme == "https" else 80)) == self.port
        except ValueError:
            return False


@dataclass
class Finding:
    rule_id: str
    title: str
    severity: str
    confidence: str
    url: str
    method: str = "GET"
    description: str = ""
    impact: str = ""
    evidence: str = ""
    remediation: str = ""
    references: list[str] = field(default_factory=list)
    tags: list[str] = field(default_factory=list)
    request: str = ""
    response: str = ""
    cwe: str = ""
    owasp: str = ""
    cvss: float | None = None
    host: str = ""
    timestamp: str = field(default_factory=lambda: datetime.datetime.now(datetime.timezone.utc).isoformat())

    @property
    def score(self) -> float:
        return self.cvss if self.cvss is not None else SEV_SCORE.get(self.severity, 0.0)

    def to_dict(self) -> dict[str, Any]:
        return {
            "rule_id": self.rule_id,
            "title": self.title,
            "severity": self.severity,
            "confidence": self.confidence,
            "url": self.url,
            "method": self.method,
            "host": self.host,
            "description": self.description,
            "impact": self.impact,
            "evidence": self.evidence,
            "remediation": self.remediation,
            "references": self.references,
            "tags": self.tags,
            "cwe": self.cwe,
            "owasp": self.owasp,
            "cvss": self.cvss,
            "timestamp": self.timestamp,
            "request": self.request,
            "response": self.response,
        }

    def sort_key(self) -> tuple:
        return (SEV_RANK.get(self.severity, 99), -self.score, self.rule_id, self.url)


@dataclass
class TechFingerprint:
    name: str
    category: str
    version: str = ""
    confidence: int = 50
    evidence: str = ""
    website: str = ""

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


@dataclass
class CrawledPage:
    url: str
    status: int
    content_type: str
    title: str = ""
    links: set[str] = field(default_factory=set)
    forms: list[dict[str, Any]] = field(default_factory=list)
    scripts: list[str] = field(default_factory=list)
    params: set[str] = field(default_factory=set)
    comments: list[str] = field(default_factory=list)
    emails: set[str] = field(default_factory=set)
    size: int = 0
    depth: int = 0
    headers: dict[str, str] = field(default_factory=dict)
    body: bytes = b""

    def to_dict(self) -> dict[str, Any]:
        return {
            "url": self.url,
            "status": self.status,
            "content_type": self.content_type,
            "title": self.title,
            "links": sorted(self.links),
            "forms": self.forms,
            "scripts": self.scripts,
            "params": sorted(self.params),
            "comments": self.comments[:20],
            "emails": sorted(self.emails),
            "size": self.size,
            "depth": self.depth,
        }


@dataclass
class ScanConfig:
    target: str
    max_pages: int = 300
    max_depth: int = 4
    concurrency: int = 12
    rate_limit: float = 0.0
    timeout: float = 15.0
    user_agent: str = DEFAULT_UA
    authorized: bool = False
    active: bool = False
    follow_redirects: bool = True
    verify_tls: bool = True
    proxy: str | None = None
    exclude: list[str] = field(default_factory=list)
    include: list[str] = field(default_factory=list)
    headers: dict[str, str] = field(default_factory=dict)
    cookies: str = ""
    auth: tuple[str, str] | None = None
    dns_servers: list[str] = field(default_factory=list)
    wordlist: list[str] = field(default_factory=list)
    ports: list[int] = field(default_factory=list)
    scan_subdomains: bool = False
    min_severity: str = "info"
    no_color: bool = False
    output: list[str] = field(default_factory=list)
    verbose: bool = False
    quiet: bool = False
    profile: str = "default"
    max_body: int = 8 * 1024 * 1024

    def clone(self, **changes: Any) -> "ScanConfig":
        return dataclasses.replace(self, **changes)


# ---------------------------------------------------------------------------
# Crawler
# ---------------------------------------------------------------------------

TITLE_RE = re.compile(r"<title[^>]*>(.*?)</title>", re.I | re.S)
LINK_RE = re.compile(r"""<a[^>]+href\s*=\s*["']([^"']+)["']""", re.I)
SCRIPT_RE = re.compile(r"""<script[^>]+src\s*=\s*["']([^"']+)["']""", re.I)
FORM_RE = re.compile(r"<form([^>]*)>(.*?)</form>", re.I | re.S)
INPUT_RE = re.compile(r"<(input|textarea|select|button)([^>]*)>", re.I)
COMMENT_RE = re.compile(r"<!--(.*?)-->", re.S)
EMAIL_RE = re.compile(r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}")
META_RE = re.compile(r"""<meta([^>]+)>""", re.I)
URL_ATTR_RE = re.compile(r"""(?:href|src|action|data-url|data-href)\s*=\s*["']([^"']+)["']""", re.I)
JS_URL_RE = re.compile(r"""["'`](?:https?:)?//[^"'`\s]{6,}["'`]""")
ROBOTS_SITEMAP_RE = re.compile(r"(?i)\b(?:sitemap|disallow|allow|host|crawl-delay)\s*:\s*(\S+)")

STATIC_EXT = {
    ".css", ".js", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico",
    ".woff", ".woff2", ".ttf", ".eot", ".otf", ".map", ".pdf", ".zip", ".gz",
    ".tar", ".mp3", ".mp4", ".webm", ".ogg", ".wav", ".avi", ".mov",
}


class Crawler:
    """
    Bounded breadth-first crawler.

    Extracts links, forms, parameters, scripts, comments and e-mail
    addresses.  Respects robots.txt, an exclusion list and a depth/page
    budget.  All I/O is concurrent but capped.
    """

    def __init__(self, client: HttpClient, target: Target, config: ScanConfig, log: Log) -> None:
        self.client = client
        self.target = target
        self.config = config
        self.log = log
        self.pages: dict[str, CrawledPage] = {}
        self.seen: set[str] = set()
        self.robots_disallowed: list[str] = []
        self.robots_sitemaps: list[str] = []
        self.robots_delay: float = 0.0
        self.external_hosts: dict[str, int] = defaultdict(int)
        self.emails: set[str] = set()
        self.secrets: list[dict[str, str]] = []
        self._sem = asyncio.Semaphore(config.concurrency)
        self._exclude = compile_globs(config.exclude)
        self._include = compile_globs(config.include)
        self._robots_loaded = False

    # -- URL policy -------------------------------------------------------
    def normalise(self, url: str, base: str) -> str | None:
        try:
            absolute = urllib.parse.urljoin(base, url)
        except ValueError:
            return None
        absolute = absolute.split("#")[0]
        parsed = urllib.parse.urlsplit(absolute)
        if parsed.scheme not in ("http", "https"):
            return None
        if not parsed.hostname:
            return None
        host = parsed.hostname.lower()
        if host != self.target.host:
            if not self.config.scan_subdomains:
                return None
            if not host.endswith("." + self.target.host):
                return None
        path = parsed.path or "/"
        if Path(path).suffix.lower() in STATIC_EXT:
            return None
        clean = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, path, parsed.query, ""))
        return clean

    def allowed(self, url: str) -> bool:
        parsed = urllib.parse.urlsplit(url)
        path = parsed.path or "/"
        if self._exclude and any_glob(self._exclude, path):
            return False
        if self._include and not any_glob(self._include, path):
            return False
        for pattern in self.robots_disallowed:
            if pattern and wildcard_match(pattern, path):
                return False
        return True

    # -- robots.txt -------------------------------------------------------
    async def load_robots(self) -> None:
        if self._robots_loaded:
            return
        self._robots_loaded = True
        url = self.target.url_for("/robots.txt")
        response = await self.client.request("GET", url)
        if not response.ok or response.content_type not in ("text/plain", ""):
            return
        agent_applies = False
        for line in response.text.splitlines():
            line = line.split("#", 1)[0].strip()
            if not line or ":" not in line:
                continue
            key, _, value = line.partition(":")
            key = key.strip().lower()
            value = value.strip()
            if key == "user-agent":
                agent_applies = value == "*" or self.config.user_agent.lower().startswith(value.lower())
            elif agent_applies and key == "disallow":
                if value:
                    self.robots_disallowed.append(value)
            elif agent_applies and key == "allow":
                continue
            elif agent_applies and key == "sitemap":
                self.robots_sitemaps.append(value)
            elif agent_applies and key == "crawl-delay":
                try:
                    self.robots_delay = float(value)
                except ValueError:
                    pass
        if self.robots_disallowed:
            self.log.debug(f"robots.txt: {len(self.robots_disallowed)} disallow rules")

    # -- main loop --------------------------------------------------------
    async def run(self, seeds: Sequence[str] | None = None) -> dict[str, CrawledPage]:
        await self.load_robots()
        queue: deque[tuple[str, int]] = deque()
        if seeds:
            for seed in seeds:
                queue.append((seed, 0))
        else:
            queue.append((self.target.url_for("/"), 0))
            for extra in ("/sitemap.xml", "/sitemap_index.xml"):
                queue.append((self.target.url_for(extra), 0))
        progress = Progress(self.log, total=self.config.max_pages, label="crawl")
        workers = [asyncio.create_task(self._worker(queue, progress)) for _ in range(min(self.config.concurrency, 8))]
        await queue_join(queue)
        for _ in workers:
            await queue.put(("__done__", 0))
        await asyncio.gather(*workers, return_exceptions=True)
        progress.close()
        return self.pages

    async def _worker(self, queue: deque, progress: Progress) -> None:
        while True:
            item = await queue.get()
            try:
                if item[0] == "__done__":
                    return
                url, depth = item
                await self._visit(url, depth, queue, progress)
            finally:
                queue.task_done()

    async def _visit(self, url: str, depth: int, queue: deque, progress: Progress) -> None:
        if url in self.seen or len(self.pages) >= self.config.max_pages:
            return
        if depth > self.config.max_depth:
            return
        if not self.allowed(url):
            self.log.debug(f"skip (robots/exclude): {url}")
            return
        self.seen.add(url)
        async with self._sem:
            response = await self.client.request("GET", url)
        if response.error:
            self.log.debug(f"crawl error {url}: {response.error}")
            return
        page = CrawledPage(
            url=url,
            status=response.status,
            content_type=response.content_type,
            size=response.length,
            depth=depth,
            headers=response.headers,
        )
        if "html" in response.content_type or response.content_type == "":
            text = response.text
            page.body = response.body
            page.title = self._extract_title(text)
            page.links = self._extract_links(text, url)
            page.scripts = self._extract_scripts(text, url)
            page.forms = self._extract_forms(text, url)
            page.params = self._extract_params(text, url)
            page.comments = self._extract_comments(text)
            page.emails = self._extract_emails(text)
            self.emails |= page.emails
            self.secrets.extend(scan_secrets(text, url))
        self.pages[url] = page
        progress.advance()
        for link in page.links:
            if link not in self.seen and len(self.pages) < self.config.max_pages:
                queue.append((link, depth + 1))

    # -- extractors -------------------------------------------------------
    @staticmethod
    def _extract_title(text: str) -> str:
        m = TITLE_RE.search(text)
        if not m:
            return ""
        return html_mod.unescape(re.sub(r"\s+", " ", m.group(1))).strip()[:200]

    def _extract_links(self, text: str, base: str) -> set[str]:
        out: set[str] = set()
        for match in URL_ATTR_RE.finditer(text):
            candidate = match.group(1)
            if candidate.startswith(("javascript:", "mailto:", "tel:", "data:", "about:")):
                continue
            normalised = self.normalise(candidate, base)
            if normalised:
                out.add(normalised)
        for match in LINK_RE.finditer(text):
            normalised = self.normalise(match.group(1), base)
            if normalised:
                out.add(normalised)
        for match in JS_URL_RE.finditer(text):
            raw = match.group(0).strip("\"'`")
            if raw.startswith("//"):
                raw = self.target.scheme + ":" + raw
            normalised = self.normalise(raw, base)
            if normalised:
                out.add(normalised)
        return out

    def _extract_scripts(self, text: str, base: str) -> list[str]:
        out = []
        for match in SCRIPT_RE.finditer(text):
            src = match.group(1)
            absolute = urllib.parse.urljoin(base, src)
            out.append(absolute)
            parsed = urllib.parse.urlsplit(absolute)
            if parsed.hostname and parsed.hostname != self.target.host:
                self.external_hosts[parsed.hostname] += 1
        return out

    def _extract_forms(self, text: str, base: str) -> list[dict[str, Any]]:
        forms: list[dict[str, Any]] = []
        for match in FORM_RE.finditer(text):
            attrs_raw, inner = match.group(1), match.group(2)
            attrs = dict(re.findall(r'([\w\-]+)\s*=\s*"([^"]*)"', attrs_raw))
            action = attrs.get("action", "")
            absolute = urllib.parse.urljoin(base, action) if action else base
            fields = []
            for input_match in INPUT_RE.finditer(inner):
                tag = input_match.group(1).lower()
                field_attrs = dict(re.findall(r'([\w\-]+)\s*=\s*"([^"]*)"', input_match.group(2)))
                fields.append(
                    {
                        "tag": tag,
                        "name": field_attrs.get("name", ""),
                        "type": field_attrs.get("type", "text" if tag != "select" else "select"),
                        "value": field_attrs.get("value", ""),
                        "id": field_attrs.get("id", ""),
                    }
                )
            forms.append(
                {
                    "action": absolute,
                    "method": attrs.get("method", "get").upper(),
                    "fields": fields,
                    "has_csrf_token": any(
                        re.search(r"(csrf|token|nonce|authenticity)", f["name"], re.I) for f in fields
                    ),
                }
            )
        return forms

    def _extract_params(self, text: str, base: str) -> set[str]:
        params: set[str] = set()
        parsed = urllib.parse.urlsplit(base)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            params.add(key)
        for match in re.finditer(r"""\b(?:url|href|src|action|data|link)\s*[:=]\s*["'][^"']*\?([^"'&#]+)""", text, re.I):
            for key in urllib.parse.parse_qs(match.group(1), keep_blank_values=True):
                params.add(key)
        for match in re.finditer(r"""\bname\s*=\s*["']([\w\-\.\[\]]+)["']""", text):
            params.add(match.group(1))
        return params

    @staticmethod
    def _extract_comments(text: str) -> list[str]:
        out = []
        for match in COMMENT_RE.finditer(text):
            body = match.group(1).strip()
            if body and len(body) < 400:
                out.append(body)
        return out[:50]

    @staticmethod
    def _extract_emails(text: str) -> set[str]:
        return {m.group(0) for m in EMAIL_RE.finditer(text) if not m.group(0).endswith((".png", ".jpg", ".gif", ".css", ".js"))}


async def queue_join(queue: deque) -> None:
    """Wait until the queue drains, then allow workers to exit."""
    await queue.join()


# ---------------------------------------------------------------------------
# Secret / sensitive data detection
# ---------------------------------------------------------------------------

SECRET_PATTERNS: list[tuple[str, re.Pattern[str], str]] = [
    ("aws-access-key", re.compile(r"\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b"), "critical"),
    ("aws-secret-key", re.compile(r"(?i)aws.{0,20}?(?:secret|key).{0,5}?[:=]\s*['\"]?([A-Za-z0-9/+=]{40})"), "critical"),
    ("google-api-key", re.compile(r"\bAIza[0-9A-Za-z\-_]{35}\b"), "high"),
    ("google-oauth", re.compile(r"\b[0-9]+-[0-9a-z_]{32}\.apps\.googleusercontent\.com\b"), "medium"),
    ("slack-token", re.compile(r"\bxox[baprs]-[0-9A-Za-z\-]{10,}\b"), "high"),
    ("slack-webhook", re.compile(r"https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]{20,}"), "high"),
    ("github-token", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,}\b"), "critical"),
    ("github-pat", re.compile(r"\bgithub_pat_[A-Za-z0-9_]{22,}\b"), "critical"),
    ("gitlab-token", re.compile(r"\bglpat-[A-Za-z0-9\-_]{20,}\b"), "critical"),
    ("stripe-key", re.compile(r"\b[sr]k_live_[0-9A-Za-z]{20,}\b"), "critical"),
    ("stripe-restricted", re.compile(r"\brk_live_[0-9A-Za-z]{20,}\b"), "high"),
    ("twilio-key", re.compile(r"\bSK[0-9a-fA-F]{32}\b"), "high"),
    ("sendgrid-key", re.compile(r"\bSG\.[A-Za-z0-9_\-]{22,}\.[A-Za-z0-9_\-]{43,}\b"), "high"),
    ("mailgun-key", re.compile(r"\bkey-[0-9a-zA-Z]{32}\b"), "medium"),
    ("npm-token", re.compile(r"\bnpm_[A-Za-z0-9]{36}\b"), "high"),
    ("pypi-token", re.compile(r"\bpypi-[A-Za-z0-9_\-]{16,}\b"), "high"),
    ("jwt", re.compile(r"\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b"), "medium"),
    ("private-key", re.compile(r"-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----"), "critical"),
    ("basic-auth-url", re.compile(r"\b[a-z][a-z0-9+.\-]*://[^/\s:@]+:[^/\s:@]+@[^\s/]+"), "high"),
    ("heroku-api", re.compile(r"(?i)heroku.{0,20}?[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}"), "medium"),
    ("firebase-url", re.compile(r"https://[a-z0-9\-]+\.firebaseio\.com"), "low"),
    ("internal-ip", re.compile(r"\b(?:10\.\d{1,3}|192\.168|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b"), "low"),
    ("s3-bucket", re.compile(r"\bhttps?://[a-z0-9\-\.]+\.s3(?:[.\-][a-z0-9\-]+)?\.amazonaws\.com\b"), "low"),
    ("generic-secret", re.compile(r"(?i)\b(?:secret|token|api[_-]?key|apikey|passwd|password|pwd|access[_-]?key|auth[_-]?token)\b\s*[:=]\s*['\"]([^'\"\s]{8,})['\"]"), "medium"),
    ("generic-assignment", re.compile(r"(?i)\b(?:secret|token|api[_-]?key|apikey|password|passwd|pwd)\b\s*[:=]\s*([^\s'\"]{8,})"), "low"),
]


def scan_secrets(text: str, url: str) -> list[dict[str, str]]:
    """Find credential-looking strings in a response body."""
    found: list[dict[str, str]] = []
    for name, pattern, severity in SECRET_PATTERNS:
        for match in pattern.finditer(text):
            value = match.group(1) if match.groups() else match.group(0)
            if len(value) < 8:
                continue
            found.append(
                {
                    "kind": name,
                    "severity": severity,
                    "url": url,
                    "match": value[:24] + ("…" if len(value) > 24 else ""),
                    "context": text[max(0, match.start() - 60) : match.end() + 60].replace("\n", " ")[:160],
                }
            )
    # de-duplicate on (kind, match)
    seen: set[tuple[str, str]] = set()
    unique = []
    for item in found:
        key = (item["kind"], item["match"])
        if key in seen:
            continue
        seen.add(key)
        unique.append(item)
    return unique


# ---------------------------------------------------------------------------
# Technology fingerprinting
# ---------------------------------------------------------------------------

FINGERPRINTS: list[dict[str, Any]] = [
    # name, category, header/body/cookie matchers, version extractor
    {"name": "nginx", "category": "web-server", "header": ("server", r"nginx(?:/([\d.]+))?"), "version": 1},
    {"name": "Apache", "category": "web-server", "header": ("server", r"Apache(?:/([\d.]+))?"), "version": 1},
    {"name": "Apache Tomcat", "category": "app-server", "header": ("server", r"Tomcat(?:/([\d.]+))?"), "version": 1},
    {"name": "Microsoft IIS", "category": "web-server", "header": ("server", r"IIS(?:/([\d.]+))?"), "version": 1},
    {"name": "LiteSpeed", "category": "web-server", "header": ("server", r"LiteSpeed"), "version": 0},
    {"name": "Caddy", "category": "web-server", "header": ("server", r"Caddy"), "version": 0},
    {"name": "openresty", "category": "web-server", "header": ("server", r"openresty(?:/([\d.]+))?"), "version": 1},
    {"name": "Cloudflare", "category": "cdn", "header": ("server", r"cloudflare"), "version": 0},
    {"name": "Amazon CloudFront", "category": "cdn", "header": ("via|x-cache", r"CloudFront"), "version": 0},
    {"name": "Akamai", "category": "cdn", "header": ("server|x-akamai", r"Akamai|Ghost"), "version": 0},
    {"name": "Fastly", "category": "cdn", "header": ("x-served-by|via", r"Fastly|cache-"), "version": 0},
    {"name": "Varnish", "category": "cache", "header": ("via|x-varnish", r"varnish"), "version": 0},
    {"name": "Squid", "category": "proxy", "header": ("via|server", r"squid"), "version": 0},
    {"name": "Envoy", "category": "proxy", "header": ("server", r"envoy"), "version": 0},
    {"name": "Traefik", "category": "proxy", "header": ("server", r"traefik"), "version": 0},
    {"name": "HAProxy", "category": "load-balancer", "header": ("server", r"haproxy"), "version": 0},

    {"name": "PHP", "category": "language", "header": ("x-powered-by|set-cookie", r"PHP(?:/([\d.]+))?"), "version": 1},
    {"name": "ASP.NET", "category": "language", "header": ("x-powered-by|x-aspnet-version", r"ASP\.NET(?:[/ ]([\d.]+))?"), "version": 1},
    {"name": "Express", "category": "framework", "header": ("x-powered-by", r"Express"), "version": 0},
    {"name": "Django", "category": "framework", "header": ("x-frame-options|set-cookie", r"django"), "version": 0},
    {"name": "Ruby on Rails", "category": "framework", "header": ("x-powered-by|set-cookie", r"Phusion Passenger|_rails"), "version": 0},
    {"name": "Laravel", "category": "framework", "cookie": r"laravel_session", "version": 0},
    {"name": "Symfony", "category": "framework", "cookie": r"symfony", "version": 0},
    {"name": "CodeIgniter", "category": "framework", "cookie": r"ci_session", "version": 0},
    {"name": "CakePHP", "category": "framework", "cookie": r"cakephp", "version": 0},
    {"name": "JSF", "category": "framework", "header": ("set-cookie", r"JSESSIONID"), "version": 0},
    {"name": "Next.js", "category": "framework", "header": ("x-nextjs|server", r"Next\.js"), "version": 0},
    {"name": "Nuxt", "category": "framework", "header": ("x-powered-by", r"Nuxt"), "version": 0},
    {"name": "Gatsby", "category": "framework", "header": ("x-gatsby", r".*"), "version": 0},
    {"name": "WordPress", "category": "cms", "body": r"wp-content|wp-includes|/wp-json|<meta name=\"generator\" content=\"WordPress", "version": r"WordPress ([\d.]+)"},
    {"name": "Drupal", "category": "cms", "header": ("x-drupal|set-cookie", r"Drupal"), "version": 0},
    {"name": "Joomla", "category": "cms", "body": r"/media/jui/|<meta name=\"generator\" content=\"Joomla", "version": r"Joomla!? ([\d.]+)"},
    {"name": "Magento", "category": "cms", "body": r"Mage\.Cookies|/skin/frontend/|magento", "version": r"Magento(?:/([\d.]+))?"},
    {"name": "Shopify", "category": "cms", "body": r"cdn\.shopify\.com|Shopify\.theme", "version": 0},
    {"name": "Ghost", "category": "cms", "body": r"ghost-(?:url|api)|content/themes/", "version": 0},
    {"name": "Hugo", "category": "generator", "body": r"<meta name=\"generator\" content=\"Hugo", "version": r"Hugo ([\d.]+)"},
    {"name": "Jekyll", "category": "generator", "body": r"<meta name=\"generator\" content=\"Jekyll", "version": r"Jekyll v?([\d.]+)"},
    {"name": "Vue.js", "category": "js-library", "body": r"vue(?:\.runtime)?(?:\.min)?\.js|data-v-[0-9a-f]{8}|__vue__", "version": r"vue[@/]v?([\d.]+)"},
    {"name": "React", "category": "js-library", "body": r"react(?:\.production\.min)?\.js|data-reactroot|__REACT_DEVTOOLS", "version": r"react[@/]v?([\d.]+)"},
    {"name": "Angular", "category": "js-library", "body": r"ng-version|<script[^>]+angular(?:\.min)?\.js", "version": r"ng-version=\"([\d.]+)\"|angular(?:\.min)?\.js[^\"]*?([\d.]+)"},
    {"name": "jQuery", "category": "js-library", "body": r"jquery(?:\.min)?\.js", "version": r"jquery[@/]v?([\d.]+)"},
    {"name": "Bootstrap", "category": "css-framework", "body": r"bootstrap(?:\.min)?\.(?:css|js)", "version": r"bootstrap[@/]v?([\d.]+)"},
    {"name": "Tailwind CSS", "category": "css-framework", "body": r"tailwind(?:\.min)?\.css|--tw-", "version": 0},
    {"name": "Font Awesome", "category": "css-framework", "body": r"font-?awesome", "version": 0},
    {"name": "Google Analytics", "category": "analytics", "body": r"google-analytics\.com|gtag\(|UA-\d{4,}", "version": 0},
    {"name": "Google Tag Manager", "category": "analytics", "body": r"googletagmanager\.com", "version": 0},
    {"name": "Matomo", "category": "analytics", "body": r"matomo\.js|piwik\.js", "version": 0},
    {"name": "Segment", "category": "analytics", "body": r"cdn\.segment\.com/analytics\.js", "version": 0},
    {"name": "Hotjar", "category": "analytics", "body": r"static\.hotjar\.com", "version": 0},
    {"name": "Sentry", "category": "monitoring", "body": r"sentry(?:-cdn)?\.com|Sentry\.init", "version": r"Sentry.{0,20}?([\d]+\.[\d]+\.[\d]+)"},
    {"name": "Datadog RUM", "category": "monitoring", "body": r"datadoghq\.com|dd-rum", "version": 0},
    {"name": "Stripe", "category": "payment", "body": r"js\.stripe\.com", "version": 0},
    {"name": "PayPal", "category": "payment", "body": r"paypal\.com/sdk/js", "version": 0},
    {"name": "reCAPTCHA", "category": "security", "body": r"recaptcha|grecaptcha", "version": 0},
    {"name": "hCaptcha", "category": "security", "body": r"hcaptcha\.com", "version": 0},
    {"name": "Vite", "category": "build-tool", "body": r"/@vite/|vite/client", "version": 0},
    {"name": "Webpack", "category": "build-tool", "body": r"webpack(?:Jsonp|\.runtime)", "version": 0},
]


def fingerprint(response: HttpResponse, pages: Iterable[CrawledPage] = ()) -> list[TechFingerprint]:
    """Detect technologies from headers, cookies and body signatures."""
    found: list[TechFingerprint] = []
    seen: set[str] = set()
    bodies = [response.text]
    for page in pages:
        if page.body:
            bodies.append(page.body.decode("utf-8", "replace"))
    combined_body = "\n".join(bodies)[:400_000]
    cookie_blob = response.header("set-cookie", "")

    for spec in FINGERPRINTS:
        name = spec["name"]
        if name in seen:
            continue
        evidence = ""
        version = ""
        if "header" in spec:
            header_name, pattern = spec["header"]
            for hkey, hval in response.headers.items():
                if re.search(header_name, hkey, re.I):
                    m = re.search(pattern, hval, re.I)
                    if m:
                        evidence = f"{hkey}: {hval[:80]}"
                        if spec.get("version") and m.groups():
                            version = m.group(1) or ""
                        break
        if not evidence and "cookie" in spec:
            m = re.search(spec["cookie"], cookie_blob, re.I)
            if m:
                evidence = f"cookie: {m.group(0)}"
        if not evidence and "body" in spec:
            m = re.search(spec["body"], combined_body, re.I)
            if m:
                evidence = f"body: {m.group(0)[:80]}"
                if spec.get("version") and isinstance(spec.get("version"), str):
                    vm = re.search(spec["version"], combined_body, re.I)
                    if vm:
                        version = vm.group(1) or ""
        if evidence:
            seen.add(name)
            found.append(
                TechFingerprint(
                    name=name,
                    category=spec["category"],
                    version=version,
                    confidence=90 if version else 70,
                    evidence=evidence,
                )
            )
    return found


# ---------------------------------------------------------------------------
# Port scanner
# ---------------------------------------------------------------------------

COMMON_PORTS = [
    21, 22, 23, 25, 53, 67, 68, 69, 80, 110, 111, 123, 135, 137, 138, 139,
    143, 161, 162, 179, 389, 443, 445, 465, 514, 515, 587, 631, 636, 873,
    993, 995, 1080, 1194, 1433, 1521, 1723, 1883, 2049, 2082, 2083, 2181,
    2375, 2376, 2483, 3000, 3128, 3306, 3389, 3690, 4000, 4040, 4369, 4444,
    4567, 4848, 5000, 5001, 5432, 5555, 5601, 5672, 5800, 5900, 5984, 6000,
    6379, 6443, 6660, 6667, 7001, 7077, 7080, 7443, 7777, 8000, 8008, 8009,
    8080, 8081, 8086, 8088, 8443, 8500, 8888, 9000, 9001, 9042, 9090, 9092,
    9200, 9300, 9418, 9999, 10000, 11211, 15672, 27017, 27018, 28017, 50000,
    54321, 55553,
]

PORT_SERVICES = {
    21: "ftp", 22: "ssh", 23: "telnet", 25: "smtp", 53: "dns", 67: "dhcp",
    68: "dhcp", 69: "tftp", 80: "http", 110: "pop3", 111: "rpcbind", 123: "ntp",
    135: "msrpc", 137: "netbios-ns", 138: "netbios-dgm", 139: "netbios-ssn",
    143: "imap", 161: "snmp", 162: "snmptrap", 179: "bgp", 389: "ldap",
    443: "https", 445: "smb", 465: "smtps", 514: "syslog", 515: "printer",
    587: "submission", 631: "ipp", 636: "ldaps", 873: "rsync", 993: "imaps",
    995: "pop3s", 1080: "socks", 1194: "openvpn", 1433: "mssql", 1521: "oracle",
    1723: "pptp", 1883: "mqtt", 2049: "nfs", 2082: "cpanel", 2083: "cpanel-ssl",
    2181: "zookeeper", 2375: "docker", 2376: "docker-tls", 2483: "oracle-ssl",
    3000: "node-dev", 3128: "squid", 3306: "mysql", 3389: "rdp", 3690: "svn",
    4000: "http-alt", 4369: "epmd", 4444: "metasploit", 4567: "http-alt",
    4848: "glassfish", 5000: "http-alt", 5001: "http-alt", 5432: "postgresql",
    5555: "adb", 5601: "kibana", 5672: "amqp", 5800: "vnc-http", 5900: "vnc",
    5984: "couchdb", 6000: "x11", 6379: "redis", 6443: "kubernetes", 6667: "irc",
    7001: "weblogic", 7077: "spark", 7080: "http-alt", 7443: "http-alt",
    8000: "http-alt", 8008: "http-alt", 8009: "ajp13", 8080: "http-proxy",
    8081: "http-alt", 8086: "influxdb", 8088: "http-alt", 8443: "https-alt",
    8500: "consul", 8888: "http-alt", 9000: "http-alt", 9001: "http-alt",
    9042: "cassandra", 9090: "http-alt", 9092: "kafka", 9200: "elasticsearch",
    9300: "elasticsearch", 9418: "git", 9999: "http-alt", 10000: "webmin",
    11211: "memcached", 15672: "rabbitmq-mgmt", 27017: "mongodb",
    27018: "mongodb", 28017: "mongodb-http", 50000: "db2", 54321: "http-alt",
    55553: "http-alt",
}

RISKY_PORTS = {
    21: ("high", "FTP transmits credentials in cleartext"),
    23: ("critical", "Telnet transmits credentials in cleartext"),
    25: ("low", "SMTP exposed"),
    69: ("medium", "TFTP has no authentication"),
    110: ("low", "POP3 without TLS"),
    111: ("medium", "rpcbind exposes RPC services"),
    135: ("medium", "MSRPC exposed"),
    137: ("medium", "NetBIOS name service exposed"),
    138: ("medium", "NetBIOS datagram service exposed"),
    139: ("high", "NetBIOS session service exposed"),
    143: ("low", "IMAP without TLS"),
    445: ("high", "SMB exposed - common ransomware vector"),
    512: ("critical", "rexec exposed"),
    513: ("critical", "rlogin exposed"),
    514: ("critical", "rsh exposed"),
    1433: ("high", "MSSQL database exposed"),
    1521: ("high", "Oracle database exposed"),
    1883: ("medium", "MQTT broker exposed"),
    2049: ("high", "NFS exposed"),
    2181: ("high", "ZooKeeper exposed"),
    2375: ("critical", "Docker API exposed without TLS"),
    2376: ("high", "Docker TLS API exposed"),
    3000: ("medium", "Development server exposed"),
    3128: ("medium", "Open proxy port exposed"),
    3306: ("high", "MySQL database exposed"),
    3389: ("high", "RDP exposed - brute-force target"),
    3690: ("medium", "Subversion exposed"),
    4444: ("high", "Metasploit default listener port"),
    4848: ("high", "GlassFish admin console exposed"),
    5432: ("high", "PostgreSQL database exposed"),
    5555: ("high", "Android debug bridge exposed"),
    5601: ("medium", "Kibana exposed"),
    5672: ("medium", "RabbitMQ exposed"),
    5900: ("high", "VNC exposed"),
    5984: ("high", "CouchDB exposed"),
    6379: ("critical", "Redis exposed - often unauthenticated"),
    7001: ("high", "WebLogic exposed"),
    8009: ("high", "AJP connector exposed - Ghostcat risk"),
    8080: ("low", "Alternate HTTP port"),
    8086: ("medium", "InfluxDB exposed"),
    8443: ("low", "Alternate HTTPS port"),
    8500: ("medium", "Consul exposed"),
    9000: ("medium", "Alternate service port"),
    9042: ("high", "Cassandra exposed"),
    9090: ("medium", "Prometheus exposed"),
    9200: ("critical", "Elasticsearch exposed - often unauthenticated"),
    9300: ("high", "Elasticsearch transport exposed"),
    11211: ("high", "Memcached exposed - often unauthenticated"),
    15672: ("medium", "RabbitMQ management exposed"),
    27017: ("critical", "MongoDB exposed - often unauthenticated"),
    27018: ("high", "MongoDB shard exposed"),
    50000: ("high", "DB2 exposed"),
}


@dataclass
class PortResult:
    port: int
    state: str  # open | closed | filtered
    service: str = ""
    banner: str = ""
    tls: bool = False
    latency_ms: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


class PortScanner:
    """Async TCP connect scanner with banner grabbing."""

    def __init__(self, log: Log, timeout: float = 2.0, concurrency: int = 200, grab_banners: bool = True) -> None:
        self.log = log
        self.timeout = timeout
        self.grab_banners = grab_banners
        self._sem = asyncio.Semaphore(concurrency)

    async def scan_port(self, host: str, port: int) -> PortResult:
        async with self._sem:
            started = time.monotonic()
            result = PortResult(port=port, state="filtered", service=PORT_SERVICES.get(port, ""))
            try:
                reader, writer = await asyncio.wait_for(
                    asyncio.open_connection(host, port), timeout=self.timeout
                )
                result.state = "open"
                result.latency_ms = round((time.monotonic() - started) * 1000, 1)
                if self.grab_banners:
                    result.banner = await self._banner(reader, writer, port)
                writer.close()
                with contextlib.suppress(Exception):
                    await asyncio.wait_for(writer.wait_closed(), timeout=1)
            except (asyncio.TimeoutError, OSError):
                result.state = "filtered"
            except Exception as exc:  # pragma: no cover
                result.state = "error"
                result.banner = str(exc)[:80]
            return result

    async def _banner(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter, port: int) -> str:
        """Read a banner, sending a probe first for ports that need one."""
        probes: dict[int, bytes] = {
            80: b"GET / HTTP/1.0\r\nHost: localhost\r\n\r\n",
            8080: b"GET / HTTP/1.0\r\nHost: localhost\r\n\r\n",
            8000: b"GET / HTTP/1.0\r\nHost: localhost\r\n\r\n",
            443: b"",
            8443: b"",
            6379: b"PING\r\n",
            11211: b"version\r\n",
            9200: b"GET / HTTP/1.0\r\n\r\n",
            27017: b"",
            21: b"",
            22: b"",
            25: b"",
            110: b"",
            143: b"",
            3306: b"",
            5432: b"",
        }
        try:
            probe = probes.get(port)
            if probe:
                writer.write(probe)
                await asyncio.wait_for(writer.drain(), timeout=1)
            data = await asyncio.wait_for(reader.read(512), timeout=1.5)
            if not data:
                return ""
            return data.decode("utf-8", "replace").strip()[:200]
        except (asyncio.TimeoutError, OSError):
            return ""

    async def scan(self, host: str, ports: Sequence[int]) -> list[PortResult]:
        progress = Progress(self.log, total=len(ports), label="ports")
        tasks = [asyncio.create_task(self.scan_port(host, port)) for port in ports]
        results: list[PortResult] = []
        for coro in asyncio.as_completed(tasks):
            result = await coro
            progress.advance()
            if result.state == "open":
                results.append(result)
        progress.close()
        results.sort(key=lambda r: r.port)
        return results


# ---------------------------------------------------------------------------
# Subdomain discovery
# ---------------------------------------------------------------------------

DEFAULT_SUBDOMAINS = [
    "www", "mail", "remote", "blog", "webmail", "server", "ns1", "ns2", "smtp",
    "secure", "vpn", "m", "shop", "ftp", "mail2", "test", "portal", "ns", "ww1",
    "host", "support", "dev", "web", "bbs", "mx", "email", "cloud", "api",
    "admin", "app", "staging", "stage", "beta", "git", "gitlab", "jenkins",
    "ci", "cd", "build", "jira", "confluence", "wiki", "docs", "status",
    "monitor", "grafana", "kibana", "prometheus", "metrics", "logs", "kibana",
    "db", "database", "mysql", "postgres", "redis", "mongo", "elastic",
    "search", "queue", "mq", "kafka", "zookeeper", "consul", "vault", "auth",
    "sso", "login", "id", "identity", "account", "accounts", "pay", "payment",
    "checkout", "store", "cart", "cdn", "static", "assets", "img", "images",
    "media", "video", "download", "downloads", "files", "backup", "backups",
    "old", "new", "v2", "legacy", "internal", "intranet", "extranet", "corp",
    "office", "hr", "crm", "erp", "cms", "panel", "dashboard", "console",
    "kubernetes", "k8s", "rancher", "openshift", "docker", "registry", "harbor",
    "argocd", "flux", "terraform", "ansible", "puppet", "chef", "nagios",
    "zabbix", "cacti", "munin", "phpmyadmin", "pma", "adminer", "roundcube",
    "sogo", "zimbra", "exchange", "autodiscover", "lyncdiscover", "sip",
    "voip", "pbx", "asterisk", "phone", "fax", "meet", "zoom", "chat",
    "slack", "teams", "discord", "forum", "community", "help", "kb",
    "knowledge", "faq", "ticket", "tickets", "desk", "service", "services",
    "gateway", "gw", "router", "firewall", "proxy", "lb", "loadbalancer",
    "edge", "node", "worker", "workers", "batch", "cron", "scheduler",
    "analytics", "tracking", "tracker", "pixel", "beacon", "collect",
    "ingest", "data", "warehouse", "etl", "bi", "reporting", "reports",
    "finance", "billing", "invoice", "legal", "compliance", "audit",
    "security", "sec", "soc", "cert", "pki", "ca", "crl", "ocsp",
]


@dataclass
class SubdomainResult:
    name: str
    ips: list[str] = field(default_factory=list)
    cname: list[str] = field(default_factory=list)
    source: str = "bruteforce"
    http_status: int | None = None
    title: str = ""
    tls: bool = False

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


class SubdomainDiscovery:
    """Passive (CT logs) + active (brute force) subdomain enumeration."""

    CT_LOGS = [
        "https://crt.sh/?q=%25.{domain}&output=json",
        "https://otx.alienvault.com/api/v1/indicators/domain/{domain}/passive_dns",
    ]

    def __init__(self, dns: DnsClient, client: HttpClient, log: Log, wordlist: Sequence[str] | None = None, concurrency: int = 40) -> None:
        self.dns = dns
        self.client = client
        self.log = log
        self.wordlist = list(wordlist) if wordlist else DEFAULT_SUBDOMAINS
        self._sem = asyncio.Semaphore(concurrency)

    async def passive(self, domain: str) -> set[str]:
        """Query certificate transparency logs for known names."""
        found: set[str] = set()
        url = self.CT_LOGS[0].format(domain=domain)
        try:
            response = await self.client.request("GET", url, headers={"Accept": "application/json"})
            if response.ok:
                data = json.loads(response.text)
                for entry in data if isinstance(data, list) else []:
                    for name in str(entry.get("name_value", "")).split("\n"):
                        name = name.strip().lower().lstrip("*.")
                        if name.endswith(domain) and name != domain:
                            found.add(name)
        except Exception as exc:
            self.log.debug(f"crt.sh lookup failed: {exc}")
        return found

    async def brute(self, domain: str, wildcard_check: bool = True) -> list[SubdomainResult]:
        """Resolve a wordlist of subdomains, skipping wildcard DNS."""
        wildcard_ips: list[str] = []
        if wildcard_check:
            probe = f"{sha256_hex(str(time.time()))[:12]}.{domain}"
            wildcard_ips = [str(v) for v in await self.dns.resolve(probe, "A")]
            if wildcard_ips:
                self.log.warn(f"wildcard DNS detected for *.{domain} -> {wildcard_ips[0]}")

        progress = Progress(self.log, total=len(self.wordlist), label="subdomains")
        results: list[SubdomainResult] = []

        async def check(label: str) -> None:
            async with self._sem:
                fqdn = f"{label}.{domain}"
                ips = [str(v) for v in await self.dns.resolve(fqdn, "A")]
                aaaa = [str(v) for v in await self.dns.resolve(fqdn, "AAAA")]
                cnames = [str(v) for v in await self.dns.resolve(fqdn, "CNAME")]
                if not ips and not aaaa:
                    return
                if wildcard_ips and set(ips) == set(wildcard_ips):
                    return
                results.append(SubdomainResult(name=fqdn, ips=ips or aaaa, cname=cnames, source="bruteforce"))

        tasks = [asyncio.create_task(check(label)) for label in self.wordlist]
        for coro in asyncio.as_completed(tasks):
            await coro
            progress.advance()
        progress.close()
        results.sort(key=lambda r: r.name)
        return results

    async def probe_http(self, results: Sequence[SubdomainResult]) -> None:
        """Fetch titles and status codes for discovered hosts."""

        async def probe(result: SubdomainResult) -> None:
            for scheme in ("https", "http"):
                url = f"{scheme}://{result.name}"
                try:
                    response = await self.client.request("GET", url, timeout=6, allow_redirects=False)
                except Exception:
                    continue
                if response.error:
                    continue
                result.http_status = response.status
                if scheme == "https" and response.tls:
                    result.tls = True
                if "html" in response.content_type:
                    m = TITLE_RE.search(response.text)
                    if m:
                        result.title = html_mod.unescape(m.group(1)).strip()[:120]
                return

        await asyncio.gather(*(probe(r) for r in results), return_exceptions=True)

    async def run(self, domain: str, passive: bool = True, active: bool = True) -> list[SubdomainResult]:
        found: dict[str, SubdomainResult] = {}
        if passive:
            for name in await self.passive(domain):
                found[name] = SubdomainResult(name=name, source="ct-log")
        if active:
            for result in await self.brute(domain):
                found[result.name] = result
        results = list(found.values())
        if results:
            await self.probe_http(results)
        return results


# ---------------------------------------------------------------------------
# TLS / certificate analysis
# ---------------------------------------------------------------------------

WEAK_CIPHERS = ("RC4", "DES", "3DES", "MD5", "NULL", "EXPORT", "anon", "IDEA", "SEED", "RC2")
DEPRECATED_TLS = ("SSLv2", "SSLv3", "TLSv1", "TLSv1.1")


@dataclass
class TlsProbe:
    version: str
    supported: bool
    cipher: str = ""
    error: str = ""

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


@dataclass
class TlsReport:
    host: str
    port: int
    versions: list[TlsProbe] = field(default_factory=list)
    certificate: Certificate | None = None
    chain: list[Certificate] = field(default_factory=list)
    negotiated: dict[str, Any] = field(default_factory=dict)
    alpn: list[str] = field(default_factory=list)
    issues: list[str] = field(default_factory=list)
    grade: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "host": self.host,
            "port": self.port,
            "versions": [v.to_dict() for v in self.versions],
            "certificate": self.certificate.to_dict() if self.certificate else None,
            "negotiated": self.negotiated,
            "alpn": self.alpn,
            "issues": self.issues,
            "grade": self.grade,
        }


class TlsAnalyser:
    """Probe protocol versions, ciphers and analyse the certificate chain."""

    def __init__(self, log: Log, timeout: float = 8.0) -> None:
        self.log = log
        self.timeout = timeout

    def _context(self, version: str | None, verify: bool = True) -> ssl.SSLContext:
        if verify:
            ctx = ssl.create_default_context()
        else:
            ctx = ssl._create_unverified_context()
        ctx.check_hostname = verify
        ctx.verify_mode = ssl.CERT_REQUIRED if verify else ssl.CERT_NONE
        if version and hasattr(ssl, "TLSVersion"):
            mapping = {
                "SSLv3": "SSLv3",
                "TLSv1": "TLSv1",
                "TLSv1.1": "TLSv1_1",
                "TLSv1.2": "TLSv1_2",
                "TLSv1.3": "TLSv1_3",
            }
            attr = mapping.get(version)
            if attr and hasattr(ssl.TLSVersion, attr):
                ctx.minimum_version = getattr(ssl.TLSVersion, attr)
                ctx.maximum_version = getattr(ssl.TLSVersion, attr)
        try:
            ctx.set_alpn_protocols(["h2", "http/1.1"])
        except (NotImplementedError, AttributeError):
            pass
        return ctx

    async def probe_version(self, host: str, port: int, version: str, server_name: str) -> TlsProbe:
        try:
            ctx = self._context(version, verify=False)
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(host, port, ssl=ctx, server_hostname=server_name),
                timeout=self.timeout,
            )
            ssl_object = writer.get_extra_info("sslobject")
            cipher = ssl_object.cipher()[0] if ssl_object and ssl_object.cipher() else ""
            writer.close()
            with contextlib.suppress(Exception):
                await asyncio.wait_for(writer.wait_closed(), timeout=2)
            return TlsProbe(version=version, supported=True, cipher=cipher)
        except (ssl.SSLError, ssl.CertificateError) as exc:
            return TlsProbe(version=version, supported=False, error=str(exc)[:120])
        except (OSError, asyncio.TimeoutError) as exc:
            return TlsProbe(version=version, supported=False, error=f"{type(exc).__name__}")

    async def analyse(self, host: str, port: int = 443, server_name: str | None = None) -> TlsReport:
        server_name = server_name or host
        report = TlsReport(host=host, port=port)
        versions = ["SSLv3", "TLSv1", "TLSv1.1", "TLSv1.2", "TLSv1.3"]
        probes = await asyncio.gather(*(self.probe_version(host, port, v, server_name) for v in versions))
        report.versions = list(probes)
        for probe in probes:
            if probe.supported:
                if probe.version in DEPRECATED_TLS:
                    report.issues.append(f"{probe.version} is deprecated and must be disabled")
                if probe.cipher and any(weak in probe.cipher.upper() for weak in WEAK_CIPHERS):
                    report.issues.append(f"weak cipher negotiated on {probe.version}: {probe.cipher}")

        # full handshake for chain + negotiated parameters
        try:
            ctx = ssl.create_default_context()
            try:
                ctx.set_alpn_protocols(["h2", "http/1.1"])
            except (NotImplementedError, AttributeError):
                pass
            reader, writer = await asyncio.wait_for(
                asyncio.open_connection(host, port, ssl=ctx, server_hostname=server_name),
                timeout=self.timeout,
            )
            ssl_object = writer.get_extra_info("sslobject")
            if ssl_object:
                report.negotiated = {
                    "version": ssl_object.version(),
                    "cipher": ssl_object.cipher()[0] if ssl_object.cipher() else None,
                    "bits": ssl_object.cipher()[2] if ssl_object.cipher() else None,
                }
                report.alpn = [p for p in ([ssl_object.selected_alpn_protocol()] if ssl_object.selected_alpn_protocol() else [])]
                der = ssl_object.getpeercert(binary_form=True)
                if der:
                    report.certificate = parse_certificate(der)
                    chain = ssl_object.getpeercert()
                    if chain:
                        report.certificate.san = report.certificate.san or []
            writer.close()
            with contextlib.suppress(Exception):
                await asyncio.wait_for(writer.wait_closed(), timeout=2)
        except ssl.SSLCertVerificationError as exc:
            report.issues.append(f"certificate verification failed: {exc.verify_message} ({exc.verify_code})")
            try:
                ctx = self._context(None, verify=False)
                reader, writer = await asyncio.wait_for(
                    asyncio.open_connection(host, port, ssl=ctx, server_hostname=server_name),
                    timeout=self.timeout,
                )
                der = writer.get_extra_info("sslobject").getpeercert(binary_form=True)
                if der:
                    report.certificate = parse_certificate(der)
                writer.close()
                with contextlib.suppress(Exception):
                    await asyncio.wait_for(writer.wait_closed(), timeout=2)
            except Exception:
                pass
        except (OSError, asyncio.TimeoutError) as exc:
            report.issues.append(f"TLS handshake failed: {exc}")

        self._analyse_certificate(report)
        report.grade = self._grade(report)
        return report

    def _analyse_certificate(self, report: TlsReport) -> None:
        cert = report.certificate
        if not cert:
            return
        if cert.days_left is not None and cert.days_left < 0:
            report.issues.append(f"certificate expired {abs(cert.days_left)} days ago")
        elif cert.days_left is not None and cert.days_left < 14:
            report.issues.append(f"certificate expires in {cert.days_left} days")
        if cert.not_before and cert.not_before > datetime.datetime.now(datetime.timezone.utc):
            report.issues.append("certificate is not yet valid")
        if cert.self_signed:
            report.issues.append("certificate is self-signed")
        names = set(cert.san) | ({cert.common_name} if cert.common_name else set())
        host = report.host.lower()
        matched = False
        for name in names:
            if wildcard_match(name, host) or name.lower() == host:
                matched = True
                break
        if names and not matched:
            report.issues.append(f"hostname {host} not present in certificate names ({', '.join(sorted(names))[:120]})")
        if not cert.san:
            report.issues.append("certificate has no subjectAltName extension")
        if cert.key_alg == "rsaEncryption" and cert.key_size and cert.key_size < 2048:
            report.issues.append(f"RSA key too small: {cert.key_size} bits")
        if "sha1" in cert.sig_alg.lower() or "md5" in cert.sig_alg.lower():
            report.issues.append(f"weak signature algorithm: {cert.sig_alg}")
        if cert.is_ca and not cert.self_signed:
            report.issues.append("server certificate is marked as a CA")
        if not cert.ocsp and not cert.crl_dp:
            report.issues.append("no revocation information (OCSP/CRL) in certificate")

    @staticmethod
    def _grade(report: TlsReport) -> str:
        score = 100
        for probe in report.versions:
            if probe.supported:
                if probe.version == "SSLv3":
                    score -= 40
                elif probe.version == "TLSv1":
                    score -= 30
                elif probe.version == "TLSv1.1":
                    score -= 25
                elif probe.version == "TLSv1.3":
                    score += 5
        cert = report.certificate
        if cert:
            if cert.days_left is not None and cert.days_left < 0:
                score -= 50
            elif cert.days_left is not None and cert.days_left < 14:
                score -= 10
            if cert.self_signed:
                score -= 20
            if cert.key_alg == "rsaEncryption" and cert.key_size < 2048:
                score -= 30
            if "sha1" in cert.sig_alg.lower() or "md5" in cert.sig_alg.lower():
                score -= 25
            if not cert.san:
                score -= 5
        score -= 10 * len([i for i in report.issues if "weak cipher" in i])
        score = max(0, min(100, score))
        for threshold, grade in ((95, "A+"), (90, "A"), (85, "A-"), (80, "B"), (70, "C"), (55, "D"), (40, "E"), (35, "F")):
            if score >= threshold:
                return grade
        return "T"


# ---------------------------------------------------------------------------
# Security header analysis
# ---------------------------------------------------------------------------

SECURITY_HEADERS = [
    ("strict-transport-security", "medium", "Enforces HTTPS", "max-age=31536000; includeSubDomains", "CWE-319", "A05:2021"),
    ("content-security-policy", "medium", "Mitigates XSS and injection", "default-src 'self'", "CWE-79", "A03:2021"),
    ("x-frame-options", "low", "Prevents clickjacking", "DENY", "CWE-1021", "A05:2021"),
    ("x-content-type-options", "low", "Prevents MIME sniffing", "nosniff", "CWE-430", "A05:2021"),
    ("referrer-policy", "low", "Limits referrer leakage", "strict-origin-when-cross-origin", "CWE-200", "A01:2021"),
    ("permissions-policy", "low", "Restricts browser features", "geolocation=(), camera=()", "CWE-693", "A05:2021"),
    ("cross-origin-opener-policy", "low", "Isolates browsing context", "same-origin", "CWE-346", "A05:2021"),
    ("cross-origin-resource-policy", "low", "Prevents cross-origin reads", "same-origin", "CWE-346", "A05:2021"),
    ("cross-origin-embedder-policy", "info", "Requires CORP for subresources", "require-corp", "CWE-346", "A05:2021"),
    ("x-permitted-cross-domain-policies", "info", "Controls Flash/PDF policies", "none", "CWE-264", "A05:2021"),
    ("clear-site-data", "info", "Clears data on logout", '"cache", "cookies"', "CWE-613", "A07:2021"),
]

LEAKY_HEADERS = [
    ("x-powered-by", "info", "Discloses backend technology"),
    ("x-aspnet-version", "low", "Discloses exact framework version"),
    ("x-aspnetmvc-version", "low", "Discloses exact framework version"),
    ("x-runtime", "info", "Discloses response time"),
    ("x-drupal-cache", "info", "Discloses CMS cache state"),
    ("x-generator", "info", "Discloses generator software"),
    ("x-backend-server", "low", "Discloses internal hostname"),
    ("x-server-id", "low", "Discloses internal server identity"),
    ("x-amz-cf-id", "info", "Discloses CDN request id"),
    ("x-cache", "info", "Discloses cache state"),
    ("x-timer", "info", "Discloses precise timing"),
    ("x-debug-token", "medium", "Symfony debug token enables profiler access"),
    ("x-debug-token-link", "medium", "Symfony profiler link"),
    ("x-symfony-cache", "info", "Discloses framework cache state"),
    ("x-litespeed-cache", "info", "Discloses cache layer"),
    ("x-proxy-cache", "info", "Discloses proxy cache"),
    ("cf-ray", "info", "Discloses Cloudflare request id"),
    ("x-nginx-cache", "info", "Discloses cache state"),
    ("server-timing", "info", "Discloses backend timings"),
]


def analyse_security_headers(response: HttpResponse) -> list[Finding]:
    """Check for missing and misconfigured security headers."""
    findings: list[Finding] = []
    host = urllib.parse.urlsplit(response.url).netloc
    for name, severity, purpose, recommended, cwe, owasp in SECURITY_HEADERS:
        value = response.header(name)
        if not value:
            findings.append(
                Finding(
                    rule_id=f"header.missing.{name}",
                    title=f"Missing security header: {name}",
                    severity=severity,
                    confidence="high",
                    url=response.url,
                    description=f"The response does not set the {name} header. {purpose}.",
                    impact=f"Without {name}, {purpose.lower()} is not enforced by the browser.",
                    evidence=f"header '{name}' absent from response",
                    remediation=f"Add: {name}: {recommended}",
                    references=[f"https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/{name}"],
                    tags=["headers", "hardening"],
                    cwe=cwe,
                    owasp=owasp,
                    host=host,
                )
            )
            continue
        if name == "strict-transport-security":
            m = re.search(r"max-age=(\d+)", value)
            if not m or int(m.group(1)) < 15768000:
                findings.append(
                    Finding(
                        rule_id="header.weak.hsts",
                        title="HSTS max-age is too short",
                        severity="low",
                        confidence="high",
                        url=response.url,
                        description="HSTS max-age should be at least 15768000 (6 months); one year is recommended.",
                        evidence=f"strict-transport-security: {value}",
                        remediation="Use: max-age=31536000; includeSubDomains",
                        references=["https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Strict-Transport-Security"],
                        tags=["headers", "tls"],
                        cwe="CWE-319",
                        owasp="A05:2021",
                        host=host,
                    )
                )
        if name == "content-security-policy":
            if "unsafe-inline" in value or "unsafe-eval" in value:
                findings.append(
                    Finding(
                        rule_id="header.weak.csp",
                        title="CSP allows unsafe-inline or unsafe-eval",
                        severity="low",
                        confidence="high",
                        url=response.url,
                        description="A CSP containing 'unsafe-inline' or 'unsafe-eval' provides little protection against XSS.",
                        evidence=f"content-security-policy: {value[:200]}",
                        remediation="Remove unsafe-inline/unsafe-eval; use nonces or hashes.",
                        references=["https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP"],
                        tags=["headers", "xss"],
                        cwe="CWE-79",
                        owasp="A03:2021",
                        host=host,
                    )
                )
            if "default-src *" in value.replace(" ", ""):
                findings.append(
                    Finding(
                        rule_id="header.weak.csp-wildcard",
                        title="CSP default-src allows all origins",
                        severity="low",
                        confidence="high",
                        url=response.url,
                        evidence=f"content-security-policy: {value[:200]}",
                        remediation="Restrict default-src to 'self' plus explicit hosts.",
                        tags=["headers"],
                        host=host,
                    )
                )
    for name, severity, purpose in LEAKY_HEADERS:
        value = response.header(name)
        if value:
            findings.append(
                Finding(
                    rule_id=f"header.info-disclosure.{name}",
                    title=f"Information disclosure via {name} header",
                    severity=severity,
                    confidence="high",
                    url=response.url,
                    description=f"The {name} header exposes implementation detail. {purpose}.",
                    evidence=f"{name}: {value[:120]}",
                    remediation=f"Remove or sanitise the {name} header at the proxy or application layer.",
                    tags=["headers", "disclosure"],
                    cwe="CWE-200",
                    owasp="A05:2021",
                    host=host,
                )
            )
    return findings


def analyse_cookies(response: HttpResponse) -> list[Finding]:
    """Check Set-Cookie attributes."""
    findings: list[Finding] = []
    host = urllib.parse.urlsplit(response.url).netloc
    secure_context = response.url.startswith("https://")
    for cookie in response.set_cookies():
        name = cookie["name"]
        attrs = cookie["attrs"]
        problems: list[str] = []
        if not attrs.get("httponly"):
            problems.append("HttpOnly")
        if secure_context and not attrs.get("secure"):
            problems.append("Secure")
        if not attrs.get("samesite"):
            problems.append("SameSite")
        elif str(attrs.get("samesite")).lower() == "none" and not attrs.get("secure"):
            problems.append("SameSite=None without Secure")
        for problem in problems:
            severity = "medium" if problem == "HttpOnly" else "low"
            findings.append(
                Finding(
                    rule_id=f"cookie.missing.{problem.lower().replace('=', '-')}",
                    title=f"Cookie '{name}' missing {problem} attribute",
                    severity=severity,
                    confidence="high",
                    url=response.url,
                    description=f"The cookie '{name}' is set without the {problem} attribute.",
                    impact={
                        "HttpOnly": "JavaScript can read the cookie, so an XSS bug becomes session theft.",
                        "Secure": "The cookie can be sent over plaintext HTTP and intercepted.",
                        "SameSite": "The cookie is sent on cross-site requests, enabling CSRF.",
                    }.get(problem, ""),
                    evidence=f"set-cookie: {name}=…; {cookie.get('attrs', {})}",
                    remediation=f"Set {problem} on the cookie.",
                    references=["https://developer.mozilla.org/en-US/docs/Web/HTTP/Cookies"],
                    tags=["cookies", "session"],
                    cwe="CWE-1004" if problem == "HttpOnly" else "CWE-614",
                    owasp="A05:2021",
                    host=host,
                )
            )
        if not cookie["value"] and name:
            findings.append(
                Finding(
                    rule_id="cookie.empty-value",
                    title=f"Cookie '{name}' has an empty value",
                    severity="info",
                    confidence="medium",
                    url=response.url,
                    evidence=f"set-cookie: {name}=",
                    tags=["cookies"],
                    host=host,
                )
            )
    return findings


# ---------------------------------------------------------------------------
# Check registry
# ---------------------------------------------------------------------------


@dataclass
class CheckContext:
    target: Target
    client: HttpClient
    dns: DnsClient
    config: ScanConfig
    log: Log
    pages: dict[str, CrawledPage] = field(default_factory=dict)
    techs: list[TechFingerprint] = field(default_factory=list)
    baseline: HttpResponse | None = None
    findings: list[Finding] = field(default_factory=list)
    data: dict[str, Any] = field(default_factory=dict)


@dataclass
class Check:
    check_id: str
    name: str
    description: str
    category: str
    active: bool = False
    severity: str = "medium"
    runner: Callable[[CheckContext], Any] | None = None

    async def run(self, ctx: CheckContext) -> list[Finding]:
        if not self.runner:
            return []
        try:
            result = self.runner(ctx)
            if inspect.isawaitable(result):
                result = await result
            return result or []
        except Exception as exc:  # a broken check must not kill the scan
            ctx.log.debug(f"check {self.check_id} raised {type(exc).__name__}: {exc}")
            return []


CHECKS: list[Check] = []


def register(check: Check) -> Check:
    CHECKS.append(check)
    return check


def check(
    check_id: str,
    name: str,
    description: str,
    category: str,
    active: bool = False,
    severity: str = "medium",
) -> Callable[[Callable], Callable]:
    def decorator(func: Callable) -> Callable:
        register(Check(check_id, name, description, category, active, severity, func))
        return func

    return decorator


# ---------------------------------------------------------------------------
# Recon / exposure checks
# ---------------------------------------------------------------------------


async def _fetch_text(ctx: CheckContext, path: str) -> HttpResponse | None:
    response = await ctx.client.request("GET", ctx.target.url_for(path))
    return response if response.ok else None


@check("exposure.git", "Exposed Git repository", "Detects an accessible .git directory or config file", "exposure", True, "high")
async def check_git_exposure(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    for path in ("/.git/config", "/.git/HEAD", "/.git/index"):
        response = await ctx.client.request("GET", ctx.target.url_for(path))
        if response.ok and (response.length > 0 or path.endswith("HEAD")):
            body = response.text[:200]
            if path.endswith("config") and ("[core]" in body or "repositoryformatversion" in body):
                findings.append(
                    Finding(
                        rule_id="exposure.git.config",
                        title="Git repository metadata exposed",
                        severity="high",
                        confidence="high",
                        url=response.url,
                        description="The .git/config file is downloadable, leaking repository URLs, remotes and sometimes credentials.",
                        impact="An attacker can reconstruct the entire source history with git-dumper, including deleted secrets.",
                        evidence=body[:160],
                        remediation="Block dotfiles at the web server and deploy build artefacts only.",
                        references=["https://owasp.org/www-project-web-security-testing-guide/latest/4-Web_Application_Security_Testing/01-Information_Gathering/05-Review_Webpage_Content_for_Information_Leakage"],
                        tags=["exposure", "vcs"],
                        cwe="CWE-527",
                        owasp="A05:2021",
                        host=ctx.target.host,
                    )
                )
                break
            if path.endswith("HEAD") and body.startswith("ref:"):
                findings.append(
                    Finding(
                        rule_id="exposure.git.head",
                        title="Git HEAD file exposed",
                        severity="high",
                        confidence="high",
                        url=response.url,
                        description="The .git/HEAD file is readable, confirming a deployed .git directory.",
                        impact="Full source disclosure via git-dumper.",
                        evidence=body[:120],
                        remediation="Remove .git from the web root or deny access to dotfiles.",
                        tags=["exposure", "vcs"],
                        cwe="CWE-527",
                        owasp="A05:2021",
                        host=ctx.target.host,
                    )
                )
                break
    return findings


@check("exposure.env", "Exposed environment file", "Looks for .env, .env.local and similar files", "exposure", True, "critical")
async def check_env_exposure(ctx: CheckContext) -> list[Finding]:
    findings = []
    paths = ["/.env", "/.env.local", "/.env.production", "/.env.backup", "/.env.dev", "/.env.example", "/env", "/.env.save"]
    for path in paths:
        response = await ctx.client.request("GET", ctx.target.url_for(path))
        if not response.ok or response.length < 8:
            continue
        body = response.text
        if re.search(r"(?m)^[A-Z_]{3,}\s*=", body) and "html" not in response.content_type:
            secrets = scan_secrets(body, response.url)
            findings.append(
                Finding(
                    rule_id="exposure.env",
                    title="Environment file exposed",
                    severity="critical",
                    confidence="high",
                    url=response.url,
                    description="A .env file is served by the web server. These files hold database credentials, API keys and application secrets.",
                    impact="Direct compromise of databases, third-party services and the application itself.",
                    evidence=body[:200].replace("\n", " | "),
                    remediation="Never deploy .env inside the web root; deny access and rotate every credential it contains.",
                    references=["https://cwe.mitre.org/data/definitions/538.html"],
                    tags=["exposure", "secrets"],
                    cwe="CWE-538",
                    owasp="A05:2021",
                    host=ctx.target.host,
                    request=f"GET {path}",
                )
            )
            ctx.data.setdefault("secrets", []).extend(secrets)
            break
    return findings


SENSITIVE_PATHS = [
    ("/.htaccess", "high", "Apache configuration exposed", "CWE-538"),
    ("/.htpasswd", "critical", "Apache password file exposed", "CWE-538"),
    ("/server-status", "medium", "Apache server-status page exposed", "CWE-200"),
    ("/server-info", "medium", "Apache server-info page exposed", "CWE-200"),
    ("/wp-config.php.bak", "critical", "WordPress config backup exposed", "CWE-538"),
    ("/config.php.bak", "critical", "Application config backup exposed", "CWE-538"),
    ("/web.config", "medium", "IIS configuration exposed", "CWE-538"),
    ("/crossdomain.xml", "low", "Flash crossdomain policy exposed", "CWE-942"),
    ("/clientaccesspolicy.xml", "low", "Silverlight policy exposed", "CWE-942"),
    ("/elmah.axd", "high", "ELMAH error log exposed", "CWE-215"),
    ("/trace.axd", "high", "ASP.NET trace viewer exposed", "CWE-215"),
    ("/phpinfo.php", "medium", "phpinfo() page exposed", "CWE-200"),
    ("/info.php", "medium", "phpinfo() page exposed", "CWE-200"),
    ("/phpmyadmin/", "medium", "phpMyAdmin exposed", "CWE-200"),
    ("/pma/", "medium", "phpMyAdmin exposed", "CWE-200"),
    ("/adminer.php", "medium", "Adminer database tool exposed", "CWE-200"),
    ("/backup.sql", "critical", "SQL backup exposed", "CWE-538"),
    ("/dump.sql", "critical", "SQL dump exposed", "CWE-538"),
    ("/database.sql", "critical", "SQL dump exposed", "CWE-538"),
    ("/backup.zip", "high", "Backup archive exposed", "CWE-530"),
    ("/backup.tar.gz", "high", "Backup archive exposed", "CWE-530"),
    ("/www.zip", "high", "Source archive exposed", "CWE-530"),
    ("/app.zip", "high", "Source archive exposed", "CWE-530"),
    ("/.DS_Store", "medium", "macOS directory metadata exposed", "CWE-538"),
    ("/.svn/entries", "high", "Subversion metadata exposed", "CWE-527"),
    ("/.svn/wc.db", "high", "Subversion database exposed", "CWE-527"),
    ("/CVS/Root", "medium", "CVS metadata exposed", "CWE-527"),
    ("/.hg/store", "high", "Mercurial metadata exposed", "CWE-527"),
    ("/WEB-INF/web.xml", "high", "Java web.xml exposed", "CWE-538"),
    ("/META-INF/MANIFEST.MF", "low", "Java manifest exposed", "CWE-200"),
    ("/actuator", "high", "Spring Boot actuator exposed", "CWE-200"),
    ("/actuator/env", "critical", "Spring Boot actuator env exposed (secrets)", "CWE-200"),
    ("/actuator/heapdump", "critical", "Spring Boot heap dump exposed", "CWE-200"),
    ("/jolokia", "high", "Jolokia JMX bridge exposed", "CWE-200"),
    ("/console", "medium", "Management console exposed", "CWE-200"),
    ("/manager/html", "high", "Tomcat manager exposed", "CWE-200"),
    ("/jmx-console/", "high", "JBoss JMX console exposed", "CWE-200"),
    ("/invoker/JMXInvokerServlet", "critical", "JBoss invoker exposed", "CWE-200"),
    ("/api/", "info", "API root exposed", "CWE-200"),
    ("/graphql", "info", "GraphQL endpoint exposed", "CWE-200"),
    ("/graphiql", "low", "GraphiQL IDE exposed", "CWE-200"),
    ("/swagger.json", "low", "OpenAPI specification exposed", "CWE-200"),
    ("/openapi.json", "low", "OpenAPI specification exposed", "CWE-200"),
    ("/api-docs", "low", "API documentation exposed", "CWE-200"),
    ("/.well-known/security.txt", "info", "security.txt present", "CWE-200"),
    ("/robots.txt", "info", "robots.txt present", "CWE-200"),
    ("/sitemap.xml", "info", "sitemap present", "CWE-200"),
    ("/crossdomain.xml ", "low", "Flash policy exposed", "CWE-942"),
    ("/id_rsa", "critical", "SSH private key exposed", "CWE-538"),
    ("/id_rsa.pub", "low", "SSH public key exposed", "CWE-200"),
    ("/.ssh/authorized_keys", "high", "SSH authorized_keys exposed", "CWE-538"),
    ("/config.json", "medium", "Configuration file exposed", "CWE-538"),
    ("/config.yml", "medium", "Configuration file exposed", "CWE-538"),
    ("/settings.py", "high", "Django settings exposed", "CWE-538"),
    ("/debug", "medium", "Debug endpoint exposed", "CWE-215"),
    ("/_debug", "medium", "Debug endpoint exposed", "CWE-215"),
    ("/_profiler", "medium", "Profiler exposed", "CWE-215"),
    ("/__debug__/", "medium", "Django debug toolbar exposed", "CWE-215"),
    ("/django-admin/", "info", "Django admin exposed", "CWE-200"),
    ("/wp-admin/", "info", "WordPress admin exposed", "CWE-200"),
    ("/administrator/", "info", "Joomla admin exposed", "CWE-200"),
    ("/admin/", "info", "Admin interface exposed", "CWE-200"),
    ("/cgi-bin/", "medium", "CGI directory exposed", "CWE-538"),
    ("/icons/README", "low", "Apache icons README exposed", "CWE-200"),
    ("/README.md", "info", "README exposed", "CWE-200"),
    ("/CHANGELOG.md", "info", "Changelog exposed", "CWE-200"),
    ("/composer.json", "low", "Composer manifest exposed", "CWE-200"),
    ("/package.json", "low", "npm manifest exposed", "CWE-200"),
    ("/yarn.lock", "info", "Dependency lockfile exposed", "CWE-200"),
    ("/Gemfile", "info", "Ruby dependency file exposed", "CWE-200"),
    ("/requirements.txt", "info", "Python dependency file exposed", "CWE-200"),
    ("/Dockerfile", "medium", "Dockerfile exposed", "CWE-200"),
    ("/docker-compose.yml", "medium", "Compose file exposed", "CWE-200"),
    ("/.dockerenv", "info", "Docker environment marker exposed", "CWE-200"),
    ("/vendor/", "medium", "Vendor directory listing exposed", "CWE-548"),
    ("/node_modules/", "high", "node_modules exposed", "CWE-548"),
]


@check("exposure.sensitive-files", "Sensitive file exposure", "Probes for commonly exposed files and directories", "exposure", True, "high")
async def check_sensitive_files(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def probe(path: str, severity: str, title: str, cwe: str) -> None:
        async with sem:
            response = await ctx.client.request("GET", ctx.target.url_for(path))
            if response.error:
                return
            if response.status in (401, 403):
                return
            if response.status >= 400:
                return
            body = response.text[:400]
            # reject soft-404 pages
            if ctx.baseline and response.length > 0 and abs(response.length - ctx.baseline.length) < 20:
                if response.status == ctx.baseline.status:
                    return
            if response.length == 0 and path not in ("/robots.txt",):
                return
            if path == "/.env" and not re.search(r"(?m)^[A-Z_]{3,}\s*=", body):
                return
            if path.endswith(".sql") and not re.search(r"(?i)(insert into|create table|--)", body):
                return
            if path.endswith((".zip", ".gz")) and response.content_type not in ("application/zip", "application/gzip", "application/octet-stream"):
                return
            if path.endswith("web.xml") and "<web-app" not in body:
                return
            if path.endswith("README") and len(body) < 50:
                return
            if path in ("/graphql", "/api/") and "html" in response.content_type and ctx.baseline and response.status == ctx.baseline.status:
                return
            findings.append(
                Finding(
                    rule_id=f"exposure.path.{path.strip('/').replace('/', '.') or 'root'}",
                    title=title,
                    severity=severity,
                    confidence="medium",
                    url=response.url,
                    description=f"The path {path} is reachable and returns HTTP {response.status}.",
                    impact="Exposed files can leak credentials, source code or infrastructure detail.",
                    evidence=f"HTTP {response.status}, {human_bytes(response.length)}, content-type {response.content_type or 'unknown'}",
                    remediation=f"Restrict access to {path} or remove it from the web root.",
                    references=["https://owasp.org/www-project-web-security-testing-guide/"],
                    tags=["exposure", "path"],
                    cwe=cwe,
                    owasp="A05:2021",
                    host=ctx.target.host,
                    request=f"GET {path}",
                )
            )

    tasks = [probe(p, s, t, c) for p, s, t, c in SENSITIVE_PATHS]
    await asyncio.gather(*tasks, return_exceptions=True)
    return findings


@check("exposure.dirlisting", "Directory listing", "Checks whether directories return an index", "exposure", True, "medium")
async def check_directory_listing(ctx: CheckContext) -> list[Finding]:
    findings = []
    candidates = {"/"}
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        if parsed.path and parsed.path != "/" and not Path(parsed.path).suffix:
            candidates.add(parsed.path + "/")
    for path in sorted(candidates)[:40]:
        response = await ctx.client.request("GET", ctx.target.url_for(path))
        if not response.ok or "html" not in response.content_type:
            continue
        body = response.text
        if re.search(r"(?i)<title>\s*index of\s*", body) or re.search(r"(?i)directory listing for", body):
            findings.append(
                Finding(
                    rule_id="exposure.dirlisting",
                    title=f"Directory listing enabled at {path}",
                    severity="medium",
                    confidence="high",
                    url=response.url,
                    description="The server returns a browsable directory index.",
                    impact="Attackers can enumerate files that are not linked anywhere.",
                    evidence=re.search(r"(?i)<title>(.*?)</title>", body).group(1)[:120] if re.search(r"(?i)<title>(.*?)</title>", body) else "index page",
                    remediation="Disable autoindex (nginx: autoindex off;) or add a default index document.",
                    tags=["exposure", "listing"],
                    cwe="CWE-548",
                    owasp="A05:2021",
                    host=ctx.target.host,
                )
            )
    return findings


@check("info.methods", "Dangerous HTTP methods", "Tests for TRACE, TRACK, PUT, DELETE and CONNECT", "config", True, "medium")
async def check_http_methods(ctx: CheckContext) -> list[Finding]:
    findings = []
    url = ctx.target.url_for("/")
    response = await ctx.client.request("OPTIONS", url)
    allowed = response.header("allow") or response.header("access-control-allow-methods")
    dangerous = [m for m in ("PUT", "DELETE", "TRACE", "TRACK", "CONNECT", "PATCH") if allowed and m in allowed.upper()]
    if dangerous:
        findings.append(
            Finding(
                rule_id="config.methods.advertised",
                title=f"Dangerous HTTP methods advertised: {', '.join(dangerous)}",
                severity="medium",
                confidence="medium",
                url=url,
                description=f"The OPTIONS response advertises {', '.join(dangerous)}.",
                impact="Methods such as PUT and DELETE may allow unauthorised modification of resources.",
                evidence=f"allow: {allowed[:200]}",
                remediation="Disable unused methods at the web server.",
                tags=["config", "methods"],
                cwe="CWE-650",
                owasp="A05:2021",
                host=ctx.target.host,
                request="OPTIONS /",
            )
        )
    trace = await ctx.client.request("TRACE", url)
    if trace.status < 400 and trace.status != 405 and b"TRACE" in trace.body[:2000]:
        findings.append(
            Finding(
                rule_id="config.methods.trace",
                title="HTTP TRACE enabled (Cross-Site Tracing)",
                severity="medium",
                confidence="high",
                url=url,
                description="TRACE echoes the request, which can be abused to read HttpOnly cookies via XST.",
                impact="Session cookies may be stolen even when marked HttpOnly.",
                evidence=trace.body[:160].decode("utf-8", "replace"),
                remediation="Disable TRACE.",
                references=["https://owasp.org/www-community/attacks/Cross_Site_Tracing"],
                tags=["config", "methods", "xst"],
                cwe="CWE-693",
                owasp="A05:2021",
                host=ctx.target.host,
                request="TRACE /",
            )
        )
    return findings


@check("config.cors", "CORS misconfiguration", "Tests for permissive cross-origin resource sharing", "config", False, "high")
async def check_cors(ctx: CheckContext) -> list[Finding]:
    findings = []
    url = ctx.target.url_for("/")
    probes = [
        ("https://evil.example.com", "arbitrary origin reflected"),
        ("null", "null origin accepted"),
    ]
    for origin, label in probes:
        response = await ctx.client.request("GET", url, headers={"Origin": origin})
        acao = response.header("access-control-allow-origin")
        if not acao:
            continue
        if acao == "*" or acao == origin:
            creds = response.header("access-control-allow-credentials")
            severity = "high" if (creds and creds.lower() == "true" and acao == origin) else "medium"
            findings.append(
                Finding(
                    rule_id=f"config.cors.{origin.replace('https://', '').replace('.', '-')}",
                    title=f"CORS policy too permissive ({label})",
                    severity=severity,
                    confidence="high",
                    url=url,
                    description=f"Access-Control-Allow-Origin is '{acao}' for an attacker-controlled origin.",
                    impact="Any website can read authenticated responses from this origin.",
                    evidence=f"ACAO: {acao}; ACAC: {creds or 'absent'}",
                    remediation="Validate Origin against an allow-list and never reflect arbitrary origins with credentials.",
                    references=["https://portswigger.net/web-security/cors"],
                    tags=["cors", "config"],
                    cwe="CWE-942",
                    owasp="A05:2021",
                    host=ctx.target.host,
                )
            )
    return findings


@check("info.tech", "Technology fingerprint", "Identifies server, framework and library versions", "recon", False, "info")
async def check_tech(ctx: CheckContext) -> list[Finding]:
    findings = []
    if not ctx.baseline:
        return findings
    outdated = {
        "jQuery": ("1.12", "low", "CWE-1104"),
        "Angular": ("1.7", "medium", "CWE-1104"),
        "Bootstrap": ("3.3", "low", "CWE-1104"),
    }
    for tech in ctx.techs:
        if tech.version and tech.name in outdated:
            floor, severity, cwe = outdated[tech.name]
            if _version_lt(tech.version, floor):
                findings.append(
                    Finding(
                        rule_id=f"tech.outdated.{tech.name.lower().replace(' ', '-')}",
                        title=f"Outdated {tech.name} version {tech.version}",
                        severity=severity,
                        confidence="medium",
                        url=ctx.baseline.url,
                        description=f"{tech.name} {tech.version} is older than {floor} and has known vulnerabilities.",
                        impact="Known client-side CVEs may be exploitable.",
                        evidence=f"{tech.name} {tech.version} ({tech.evidence})",
                        remediation=f"Upgrade {tech.name} to a supported release.",
                        references=["https://snyk.io/vuln/"],
                        tags=["tech", "outdated"],
                        cwe=cwe,
                        owasp="A06:2021",
                        host=ctx.target.host,
                    )
                )
    return findings


def _version_lt(version: str, floor: str) -> bool:
    def parse(v: str) -> tuple[int, ...]:
        return tuple(int(p) for p in re.findall(r"\d+", v)[:3]) or (0,)

    a, b = parse(version), parse(floor)
    length = max(len(a), len(b))
    a += (0,) * (length - len(a))
    b += (0,) * (length - len(b))
    return a < b


# ---------------------------------------------------------------------------
# Injection checks
# ---------------------------------------------------------------------------

XSS_CANARIES = [
    ("nsv7x2q9", "plain"),
    ('"><svg/onload=nsv7x2q9>', "tag-break"),
    ("'><svg/onload=nsv7x2q9>", "single-quote-break"),
    ("nsv7x2q9\"><img src=x onerror=nsv7x2q9>", "attribute-break"),
    ("</title><svg/onload=nsv7x2q9>", "title-break"),
    ("javascript:nsv7x2q9", "js-uri"),
    ("nsv7x2q9'-confirm(1)-'", "js-context"),
    ("${nsv7x2q9}", "template-literal"),
    ("{{nsv7x2q9}}", "template-mustache"),
]

SQLI_ERROR_SIGNATURES = [
    (r"SQL syntax.*?MySQL", "MySQL"),
    (r"Warning.*?\bmysqli?\b", "MySQL"),
    (r"MySQLSyntaxErrorException", "MySQL"),
    (r"valid MySQL result", "MySQL"),
    (r"PostgreSQL.*?ERROR", "PostgreSQL"),
    (r"Warning.*?\bpg_\w+\(", "PostgreSQL"),
    (r"PSQLException", "PostgreSQL"),
    (r"org\.postgresql\.util\.", "PostgreSQL"),
    (r"Driver.*? SQL[-\s_]*Server", "MSSQL"),
    (r"OLE DB.*? SQL Server", "MSSQL"),
    (r"\[SQL Server\]", "MSSQL"),
    (r"Microsoft OLE DB Provider for SQL Server", "MSSQL"),
    (r"Unclosed quotation mark after the character string", "MSSQL"),
    (r"SQLiteException", "SQLite"),
    (r"SQLITE_ERROR", "SQLite"),
    (r"sqlite3\.OperationalError", "SQLite"),
    (r"ORA-\d{5}", "Oracle"),
    (r"Oracle error", "Oracle"),
    (r"Oracle.*?Driver", "Oracle"),
    (r"CLI Driver.*?DB2", "DB2"),
    (r"DB2 SQL error", "DB2"),
    (r"SQLSTATE\[\w+\]", "PDO"),
    (r"Syntax error or access violation", "SQL"),
    (r"quoted string not properly terminated", "SQL"),
    (r"unterminated quoted string", "SQL"),
    (r"Microsoft JET Database Engine", "Access"),
    (r"JDBC.*?Exception", "JDBC"),
    (r"pg_query\(\): Query failed", "PostgreSQL"),
    (r"supplied argument is not a valid MySQL", "MySQL"),
]

SQLI_PAYLOADS = [
    "'",
    "\"",
    "' OR '1'='1",
    "\" OR \"1\"=\"1",
    "1' ORDER BY 1--",
    "1' UNION SELECT NULL--",
    "1 AND SLEEP(3)",
    "1' AND SLEEP(3)--",
    "1; WAITFOR DELAY '0:0:3'--",
    "1 AND 1=CONVERT(int,(SELECT @@version))--",
    "' AND extractvalue(1,concat(0x7e,version()))--",
    "1' AND (SELECT 1 FROM (SELECT SLEEP(3))a)--",
    "\\",
    "%27",
    "1 OR 1=1",
]

BOOLEAN_PAIRS = [
    ("1' AND '1'='1", "1' AND '1'='2"),
    ("1 AND 1=1", "1 AND 1=2"),
    ("1' AND 'a'='a", "1' AND 'a'='b"),
]

TIME_THRESHOLD = 2.5


@check("injection.xss.reflected", "Reflected cross-site scripting", "Reflects canary payloads into responses", "injection", True, "high")
async def check_reflected_xss(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    injectable: list[tuple[str, str, str]] = []  # (url, param, method)
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            injectable.append((page.url, key, "GET"))
        for form in page.forms:
            for field in form["fields"]:
                if field["name"]:
                    injectable.append((form["action"], field["name"], form["method"]))
    if not injectable and ctx.config.active:
        injectable.append((ctx.target.url_for("/"), "q", "GET"))
    seen: set[tuple[str, str]] = set()
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, param: str, method: str) -> None:
        async with sem:
            for payload, kind in XSS_CANARIES:
                if method == "GET":
                    parsed = urllib.parse.urlsplit(url)
                    query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))
                    query[param] = payload
                    target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                    response = await ctx.client.request("GET", target_url)
                else:
                    response = await ctx.client.request(method, url, body=urllib.parse.urlencode({param: payload}))
                if response.error or response.status >= 500:
                    continue
                body = response.text
                if "nsv7x2q9" not in body:
                    continue
                context = _xss_context(body, "nsv7x2q9")
                if context is None:
                    continue
                if context == "encoded":
                    continue
                findings.append(
                    Finding(
                        rule_id=f"injection.xss.{kind}",
                        title=f"Reflected XSS in parameter '{param}' ({kind})",
                        severity="high" if context in ("html", "attribute", "script", "tag") else "medium",
                        confidence="high" if context in ("html", "attribute", "script", "tag") else "medium",
                        url=response.url,
                        method=method,
                        description=f"The value of '{param}' is reflected into the response without encoding.",
                        impact="An attacker can run arbitrary JavaScript in a victim's browser, stealing sessions or performing actions as the victim.",
                        evidence=f"payload {payload!r} reflected in {context} context: …{_snippet(body, 'nsv7x2q9')}…",
                        remediation="Encode output for its context (HTML, attribute, JS, URL) and add a Content-Security-Policy.",
                        references=["https://owasp.org/www-community/attacks/xss/", "https://cheatsheetseries.owasp.org/cheatsheets/Cross_Site_Scripting_Prevention_Cheat_Sheet.html"],
                        tags=["xss", "injection"],
                        cwe="CWE-79",
                        owasp="A03:2021",
                        host=ctx.target.host,
                        request=f"{method} {response.url} with {param}={payload!r}",
                    )
                )
                return

    await asyncio.gather(*(test(u, p, m) for u, p, m in injectable if (u, p) not in seen and not seen.add((u, p))), return_exceptions=True)
    return findings


def _xss_context(body: str, canary: str) -> str | None:
    """Classify where the canary landed, or None when it is absent/encoded."""
    index = body.find(canary)
    if index < 0:
        return None
    before = body[max(0, index - 400) : index]
    if re.search(r"&lt;|&#x?0*60;|%3C", before[-80:]):
        pass
    if re.search(r"<script[^>]*>[^<]*$", before, re.I):
        return "script"
    if re.search(r"<title[^>]*>[^<]*$", before, re.I):
        return "title"
    if re.search(r"<textarea[^>]*>[^<]*$", before, re.I):
        return "textarea"
    if re.search(r"<style[^>]*>[^<]*$", before, re.I):
        return "style"
    if re.search(r"<!--[^>]*$", before):
        return "comment"
    if re.search(r"""[\w\-)\s]\s*=\s*["'][^"']*$""", before) or re.search(r"""=\s*["'][^"']*$""", before):
        return "attribute"
    if re.search(r"<[a-zA-Z][^>]*$", before):
        return "tag"
    return "html"


def _snippet(body: str, needle: str, width: int = 70) -> str:
    index = body.find(needle)
    if index < 0:
        return ""
    start = max(0, index - width // 2)
    return re.sub(r"\s+", " ", body[start : index + width]).strip()


@check("injection.sqli.error", "SQL injection (error based)", "Looks for database errors triggered by quotes", "injection", True, "critical")
async def check_sqli_error(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    targets: list[tuple[str, str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            targets.append((page.url, key, "GET"))
    if not targets and ctx.config.active:
        targets.append((ctx.target.url_for("/"), "id", "GET"))
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, param: str, method: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            base_query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))
            for payload in SQLI_PAYLOADS[:8]:
                query = dict(base_query)
                query[param] = payload
                target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                response = await ctx.client.request("GET", target_url)
                if response.error:
                    continue
                body = response.text
                for pattern, engine in SQLI_ERROR_SIGNATURES:
                    m = re.search(pattern, body, re.I)
                    if m:
                        findings.append(
                            Finding(
                                rule_id=f"injection.sqli.error.{engine.lower()}",
                                title=f"SQL injection in parameter '{param}' ({engine} error)",
                                severity="critical",
                                confidence="high",
                                url=target_url,
                                method=method,
                                description=f"A database error is raised when '{param}' contains {payload!r}, indicating unsanitised SQL.",
                                impact="An attacker can read, modify or destroy the entire database.",
                                evidence=f"{engine}: {m.group(0)[:120]}",
                                remediation="Use parameterised queries or an ORM; validate input types.",
                                references=["https://cheatsheetseries.owasp.org/cheatsheets/SQL_Injection_Prevention_Cheat_Sheet.html"],
                                tags=["sqli", "injection"],
                                cwe="CWE-89",
                                owasp="A03:2021",
                                host=ctx.target.host,
                                request=f"GET {target_url}",
                            )
                        )
                        return

    await asyncio.gather(*(test(u, p, m) for u, p, m in targets), return_exceptions=True)
    return findings


@check("injection.sqli.time", "SQL injection (time based blind)", "Measures response delay for sleep payloads", "injection", True, "critical")
async def check_sqli_time(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    targets: list[tuple[str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            targets.append((page.url, key))
    if not targets and ctx.config.active:
        targets.append((ctx.target.url_for("/"), "id"))
    sem = asyncio.Semaphore(4)

    async def test(url: str, param: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            base_query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))

            async def send(value: str) -> HttpResponse:
                query = dict(base_query)
                query[param] = value
                target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                return await ctx.client.request("GET", target_url)

            baseline = await send(base_query.get(param, "1"))
            if baseline.error:
                return
            for payload in SQLI_PAYLOADS[5:12]:
                response = await send(payload)
                if response.error:
                    continue
                delta = response.elapsed - baseline.elapsed
                if response.elapsed >= TIME_THRESHOLD and delta >= TIME_THRESHOLD - 0.6:
                    findings.append(
                        Finding(
                            rule_id="injection.sqli.time",
                            title=f"Time-based blind SQL injection in parameter '{param}'",
                            severity="critical",
                            confidence="medium",
                            url=response.url,
                            description=f"The response took {response.elapsed:.1f}s (baseline {baseline.elapsed:.1f}s) with payload {payload!r}.",
                            impact="Blind SQL injection allows full database extraction one bit at a time.",
                            evidence=f"baseline {baseline.elapsed:.2f}s vs payload {response.elapsed:.2f}s",
                            remediation="Use parameterised queries.",
                            references=["https://owasp.org/www-community/attacks/Blind_SQL_Injection"],
                            tags=["sqli", "blind", "injection"],
                            cwe="CWE-89",
                            owasp="A03:2021",
                            host=ctx.target.host,
                            request=f"GET {response.url}",
                        )
                    )
                    return

    await asyncio.gather(*(test(u, p) for u, p in targets), return_exceptions=True)
    return findings


@check("injection.sqli.boolean", "SQL injection (boolean based blind)", "Compares responses for true/false payload pairs", "injection", True, "high")
async def check_sqli_boolean(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    targets: list[tuple[str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            targets.append((page.url, key))
    if not targets and ctx.config.active:
        targets.append((ctx.target.url_for("/"), "id"))
    sem = asyncio.Semaphore(4)

    async def test(url: str, param: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            base_query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))

            async def send(value: str) -> HttpResponse:
                query = dict(base_query)
                query[param] = value
                target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                return await ctx.client.request("GET", target_url)

            for true_payload, false_payload in BOOLEAN_PAIRS:
                true_resp = await send(true_payload)
                false_resp = await send(false_payload)
                if true_resp.error or false_resp.error:
                    continue
                if true_resp.status != false_resp.status:
                    continue
                similarity = difflib.SequenceMatcher(None, true_resp.text[:20000], false_resp.text[:20000]).ratio()
                if similarity < 0.90 and abs(true_resp.length - false_resp.length) > 8:
                    findings.append(
                        Finding(
                            rule_id="injection.sqli.boolean",
                            title=f"Boolean-based blind SQL injection in parameter '{param}'",
                            severity="high",
                            confidence="medium",
                            url=url,
                            description=f"Responses differ for {true_payload!r} and {false_payload!r} (similarity {similarity:.2f}).",
                            impact="Boolean differences allow byte-by-byte database extraction.",
                            evidence=f"len {true_resp.length} vs {false_resp.length}, similarity {similarity:.2f}",
                            remediation="Use parameterised queries.",
                            references=["https://owasp.org/www-community/attacks/Blind_SQL_Injection"],
                            tags=["sqli", "blind", "injection"],
                            cwe="CWE-89",
                            owasp="A03:2021",
                            host=ctx.target.host,
                        )
                    )
                    return

    await asyncio.gather(*(test(u, p) for u, p in targets), return_exceptions=True)
    return findings


@check("injection.ssti", "Server-side template injection", "Probes for template evaluation", "injection", True, "critical")
async def check_ssti(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    targets: list[tuple[str, str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            targets.append((page.url, key, "GET"))
    if not targets and ctx.config.active:
        targets.append((ctx.target.url_for("/"), "name", "GET"))
    probes = [
        ("{{7*7}}", "49", "jinja2/twig"),
        ("${7*7}", "49", "freemarker/velocity"),
        ("<%= 7*7 %>", "49", "erb"),
        ("#{7*7}", "49", "ruby/thymeleaf"),
        ("${{7*7}}", "49", "pebble"),
        ("@{7*7}", "49", "thymeleaf"),
        ("{{7*'7'}}", "7777777", "jinja2"),
        ("{{''.__class__.__mro__[1].__subclasses__()}}", "subclasses", "jinja2-rce"),
    ]
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, param: str, method: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            base_query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))
            for payload, expected, engine in probes:
                query = dict(base_query)
                query[param] = payload
                target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                response = await ctx.client.request("GET", target_url)
                if response.error:
                    continue
                if expected in response.text and payload not in response.text:
                    findings.append(
                        Finding(
                            rule_id=f"injection.ssti.{engine.split('/')[0]}",
                            title=f"Server-side template injection in '{param}' ({engine})",
                            severity="critical",
                            confidence="high",
                            url=target_url,
                            description=f"The payload {payload!r} was evaluated to {expected!r}, so user input reaches a template engine.",
                            impact="Template injection usually escalates to remote code execution.",
                            evidence=f"{payload!r} -> {expected!r}",
                            remediation="Never pass user input as a template; use data binding instead.",
                            references=["https://portswigger.net/research/server-side-template-injection"],
                            tags=["ssti", "rce", "injection"],
                            cwe="CWE-1336",
                            owasp="A03:2021",
                            host=ctx.target.host,
                            request=f"GET {target_url}",
                        )
                    )
                    return

    await asyncio.gather(*(test(u, p, m) for u, p, m in targets), return_exceptions=True)
    return findings


@check("injection.cmdi", "OS command injection", "Looks for command output in responses", "injection", True, "critical")
async def check_command_injection(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    targets: list[tuple[str, str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            targets.append((page.url, key, "GET"))
    if not targets and ctx.config.active:
        targets.append((ctx.target.url_for("/"), "host", "GET"))
    canary = "nsc" + sha256_hex(str(time.time()))[:8]
    payloads = [
        f";echo {canary}",
        f"|echo {canary}",
        f"&&echo {canary}",
        f"`echo {canary}`",
        f"$(echo {canary})",
        f"\necho {canary}",
        f";echo${'{'}9%9/{'}'}9{canary}",  # obfuscated
    ]
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, param: str, method: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            base_query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))
            for payload in payloads:
                query = dict(base_query)
                query[param] = payload
                target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                response = await ctx.client.request("GET", target_url)
                if response.error:
                    continue
                if canary in response.text and payload not in response.text:
                    findings.append(
                        Finding(
                            rule_id="injection.cmdi",
                            title=f"OS command injection in parameter '{param}'",
                            severity="critical",
                            confidence="high",
                            url=target_url,
                            description=f"The payload {payload!r} executed and its output ({canary}) appears in the response.",
                            impact="Full remote code execution as the web server user.",
                            evidence=f"canary {canary} present in response",
                            remediation="Never build shell commands from user input; use argument arrays and strict allow-lists.",
                            references=["https://cheatsheetseries.owasp.org/cheatsheets/OS_Command_Injection_Defense_Cheat_Sheet.html"],
                            tags=["cmdi", "rce", "injection"],
                            cwe="CWE-78",
                            owasp="A03:2021",
                            host=ctx.target.host,
                            request=f"GET {target_url}",
                        )
                    )
                    return

    await asyncio.gather(*(test(u, p, m) for u, p, m in targets), return_exceptions=True)
    return findings


@check("injection.path", "Path traversal", "Attempts to read system files", "injection", True, "high")
async def check_path_traversal(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    signatures = [b"root:x:0:0", b"root:*:0:0", b"[boot loader]", b"for 16-bit app support", b"; for 16-bit app support"]
    payloads = [
        "../../../../etc/passwd",
        "..%2f..%2f..%2f..%2fetc%2fpasswd",
        "....//....//....//etc/passwd",
        "..\\..\\..\\..\\windows\\win.ini",
        "/etc/passwd%00",
        "..%252f..%252f..%252fetc%252fpasswd",
        "%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd",
    ]
    params: list[tuple[str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            params.append((page.url, key))
    if not params and ctx.config.active:
        params.append((ctx.target.url_for("/"), "file"))
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, param: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            base_query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))
            for payload in payloads:
                query = dict(base_query)
                query[param] = payload
                target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
                response = await ctx.client.request("GET", target_url)
                if response.error:
                    continue
                for signature in signatures:
                    if signature in response.body:
                        findings.append(
                            Finding(
                                rule_id="injection.path",
                                title=f"Path traversal in parameter '{param}'",
                                severity="high",
                                confidence="high",
                                url=target_url,
                                description=f"The payload {payload!r} caused a system file to be returned.",
                                impact="Arbitrary file read, including credentials and private keys.",
                                evidence=f"signature {signature!r} found in response",
                                remediation="Canonicalise paths and confine them to an allow-listed base directory.",
                                references=["https://owasp.org/www-community/attacks/Path_Traversal"],
                                tags=["traversal", "lfi", "injection"],
                                cwe="CWE-22",
                                owasp="A01:2021",
                                host=ctx.target.host,
                                request=f"GET {target_url}",
                            )
                        )
                        return

    await asyncio.gather(*(test(u, p) for u, p in params), return_exceptions=True)
    return findings


@check("injection.xxe", "XML external entity injection", "Tests XML endpoints for XXE", "injection", True, "high")
async def check_xxe(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    canary = "nsx" + sha256_hex(str(time.time()))[:8]
    entity_payload = (
        '<?xml version="1.0" encoding="ISO-8859-1"?>'
        f'<!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><root>&xxe;</root>'
    )
    endpoints: list[tuple[str, str]] = []
    for page in ctx.pages.values():
        if "xml" in page.content_type or page.url.endswith((".xml", "/api/xml")):
            endpoints.append((page.url, "POST"))
    if not endpoints:
        endpoints.append((ctx.target.url_for("/"), "POST"))
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, method: str) -> None:
        async with sem:
            response = await ctx.client.request(
                method,
                url,
                headers={"Content-Type": "application/xml"},
                body=entity_payload,
            )
            if response.error:
                return
            if b"root:x:0:0" in response.body or b"root:*:0:0" in response.body:
                findings.append(
                    Finding(
                        rule_id="injection.xxe.file",
                        title="XML external entity injection (file read)",
                        severity="high",
                        confidence="high",
                        url=url,
                        method=method,
                        description="The XML parser resolves external entities, so local files can be read.",
                        impact="Arbitrary file read, SSRF and denial of service.",
                        evidence="/etc/passwd content returned",
                        remediation="Disable DTD processing and external entity resolution in the XML parser.",
                        references=["https://cheatsheetseries.owasp.org/cheatsheets/XML_External_Entity_Prevention_Cheat_Sheet.html"],
                        tags=["xxe", "injection"],
                        cwe="CWE-611",
                        owasp="A05:2021",
                        host=ctx.target.host,
                        request=f"{method} {url} with XXE payload",
                    )
                )
            elif response.status >= 500 and re.search(r"(?i)(simplexmlelement|domdocument|saxparser|xml)", response.text[:500]):
                findings.append(
                    Finding(
                        rule_id="injection.xxe.error",
                        title="XML parser error on entity payload",
                        severity="medium",
                        confidence="low",
                        url=url,
                        method=method,
                        description="The endpoint parses XML and errors on a DTD payload; XXE may be present.",
                        evidence=response.text[:160],
                        remediation="Disable DTD processing.",
                        tags=["xxe", "injection"],
                        cwe="CWE-611",
                        owasp="A05:2021",
                        host=ctx.target.host,
                    )
                )

    await asyncio.gather(*(test(u, m) for u, m in endpoints), return_exceptions=True)
    return findings


@check("injection.hostheader", "Host header injection", "Tests for password-reset poisoning", "injection", True, "medium")
async def check_host_header(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    url = ctx.target.url_for("/")
    evil = "evil.example.com"
    response = await ctx.client.request("GET", url, headers={"Host": evil})
    if response.error:
        return findings
    if evil in response.text or evil in response.header("location", ""):
        findings.append(
            Finding(
                rule_id="injection.hostheader",
                title="Host header value reflected in response",
                severity="medium",
                confidence="medium",
                url=url,
                description="The application trusts the Host header, which enables cache poisoning and password-reset poisoning.",
                impact="Password reset links can be redirected to an attacker-controlled domain.",
                evidence=f"Host: {evil} reflected",
                remediation="Validate the Host header against an allow-list and use absolute URLs from configuration.",
                references=["https://portswigger.net/web-security/host-header"],
                tags=["host-header", "poisoning"],
                cwe="CWE-644",
                owasp="A05:2021",
                host=ctx.target.host,
                request=f"GET / with Host: {evil}",
            )
        )
    return findings


@check("injection.crlf", "CRLF / HTTP response splitting", "Tests for header injection via CRLF", "injection", True, "medium")
async def check_crlf(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    url = ctx.target.url_for("/")
    payloads = [
        "%0d%0aX-Nova-Injected:%20yes",
        "%0d%0a%0d%0a<script>alert(1)</script>",
        "%E5%98%8A%E5%98%8DX-Nova-Injected:%20yes",
        "\r\nX-Nova-Injected: yes",
    ]
    for payload in payloads:
        target_url = url + ("&" if "?" in url else "?") + "novacrlf=" + payload
        response = await ctx.client.request("GET", target_url)
        if response.error:
            continue
        if response.has_header("x-nova-injected"):
            findings.append(
                Finding(
                    rule_id="injection.crlf",
                    title="CRLF injection in query parameter",
                    severity="medium",
                    confidence="high",
                    url=target_url,
                    description="A CRLF sequence in the request injects a response header.",
                    impact="Response splitting enables cache poisoning and XSS.",
                    evidence="x-nova-injected header present in response",
                    remediation="Reject CR and LF in user input and encode URL components.",
                    references=["https://owasp.org/www-community/attacks/HTTP_Response_Splitting"],
                    tags=["crlf", "injection"],
                    cwe="CWE-113",
                    owasp="A03:2021",
                    host=ctx.target.host,
                )
            )
            break
    return findings


@check("injection.openredirect", "Open redirect", "Tests redirect parameters for arbitrary targets", "injection", True, "medium")
async def check_open_redirect(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    params: list[tuple[str, str]] = []
    for page in ctx.pages.values():
        parsed = urllib.parse.urlsplit(page.url)
        for key in urllib.parse.parse_qs(parsed.query, keep_blank_values=True):
            if re.search(r"(redirect|url|next|return|dest|target|continue|goto|rurl|forward)", key, re.I):
                params.append((page.url, key))
    if not params:
        params.append((ctx.target.url_for("/"), "next"))
    evil = "https://evil.example.com/"
    sem = asyncio.Semaphore(ctx.config.concurrency)

    async def test(url: str, param: str) -> None:
        async with sem:
            parsed = urllib.parse.urlsplit(url)
            query = dict(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True))
            query[param] = evil
            target_url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urllib.parse.urlencode(query), ""))
            response = await ctx.client.request("GET", target_url, allow_redirects=False)
            if response.error:
                return
            location = response.header("location", "")
            if response.status in (301, 302, 303, 307, 308) and location.startswith("https://evil.example.com"):
                findings.append(
                    Finding(
                        rule_id="injection.openredirect",
                        title=f"Open redirect via parameter '{param}'",
                        severity="medium",
                        confidence="high",
                        url=target_url,
                        description=f"The application redirects to an arbitrary URL supplied in '{param}'.",
                        impact="Phishing: victims trust the initial domain and are forwarded to a malicious site.",
                        evidence=f"location: {location}",
                        remediation="Validate redirect targets against an allow-list of relative paths.",
                        references=["https://cheatsheetseries.owasp.org/cheatsheets/Unvalidated_Redirects_and_Forwards_Cheat_Sheet.html"],
                        tags=["redirect", "phishing"],
                        cwe="CWE-601",
                        owasp="A01:2021",
                        host=ctx.target.host,
                        request=f"GET {target_url}",
                    )
                )

    await asyncio.gather(*(test(u, p) for u, p in params), return_exceptions=True)
    return findings


# ---------------------------------------------------------------------------
# Authentication / session checks
# ---------------------------------------------------------------------------

DEFAULT_CREDENTIALS = [
    ("admin", "admin"),
    ("admin", "password"),
    ("admin", "123456"),
    ("admin", "admin123"),
    ("administrator", "administrator"),
    ("root", "root"),
    ("root", "toor"),
    ("test", "test"),
    ("guest", "guest"),
    ("user", "user"),
    ("admin", ""),
    ("admin", "letmein"),
    ("admin", "changeme"),
    ("operator", "operator"),
    ("admin", "Admin123"),
    ("admin", "P@ssw0rd"),
    ("tomcat", "tomcat"),
    ("admin", "tomcat"),
    ("weblogic", "weblogic"),
    ("oracle", "oracle"),
    ("postgres", "postgres"),
    ("sa", "sa"),
    ("admin", "default"),
    ("admin", "welcome"),
    ("demo", "demo"),
    ("pi", "raspberry"),
    ("ubnt", "ubnt"),
    ("admin", "1234"),
    ("admin", "12345"),
    ("admin", "1234567890"),
    ("admin", "qwerty"),
    ("admin", "abc123"),
    ("admin", "password123"),
    ("admin", "iloveyou"),
    ("admin", "monkey"),
    ("admin", "dragon"),
    ("admin", "master"),
    ("admin", "sunshine"),
    ("admin", "princess"),
    ("admin", "football"),
    ("admin", "shadow"),
    ("admin", "baseball"),
    ("admin", "superman"),
    ("admin", "trustno1"),
    ("admin", "hello"),
    ("admin", "freedom"),
    ("admin", "whatever"),
    ("admin", "qazwsx"),
]


@check("auth.default-creds", "Default credentials", "Attempts common username/password pairs on discovered login forms", "auth", True, "critical")
async def check_default_credentials(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    login_forms: list[dict[str, Any]] = []
    for page in ctx.pages.values():
        for form in page.forms:
            fields = form["fields"]
            has_password = any(f["type"] == "password" or re.search(r"pass", f["name"], re.I) for f in fields)
            has_user = any(
                re.search(r"user|login|email|name|account", f["name"], re.I) and f["type"] != "password" for f in fields
            )
            if has_password and has_user:
                login_forms.append(form)
    if not login_forms:
        return findings
    sem = asyncio.Semaphore(2)

    async def test(form: dict[str, Any]) -> None:
        async with sem:
            user_fields = [f for f in form["fields"] if f["type"] != "password" and re.search(r"user|login|email|name|account", f["name"], re.I)]
            pass_fields = [f for f in form["fields"] if f["type"] == "password" or re.search(r"pass", f["name"], re.I)]
            if not user_fields or not pass_fields:
                return
            user_field, pass_field = user_fields[0]["name"], pass_fields[0]["name"]
            baseline = await ctx.client.request(
                form["method"], form["action"], body=urllib.parse.urlencode({user_field: "nsv_nobody", pass_field: "nsv_wrong" + sha256_hex(str(time.time()))[:6]})
            )
            baseline_sig = (baseline.status, baseline.length)
            for username, password in DEFAULT_CREDENTIALS:
                response = await ctx.client.request(
                    form["method"],
                    form["action"],
                    body=urllib.parse.urlencode({user_field: username, pass_field: password}),
                )
                if response.error:
                    continue
                indicators = [
                    response.status != baseline_sig[0] and response.status in (200, 302, 303),
                    abs(response.length - baseline_sig[1]) > 40,
                    bool(re.search(r"(?i)(logout|sign out|dashboard|welcome,|my account)", response.text[:4000])),
                    bool(re.search(r"(?i)(invalid|incorrect|failed|wrong|denied|error)", response.text[:4000])) is False,
                ]
                if sum(indicators) >= 2:
                    findings.append(
                        Finding(
                            rule_id="auth.default-creds",
                            title=f"Default credentials accepted: {username}/{password}",
                            severity="critical",
                            confidence="medium",
                            url=form["action"],
                            method=form["method"],
                            description=f"The login form at {form['action']} accepted {username!r} with a common password.",
                            impact="Complete account takeover with no exploitation required.",
                            evidence=f"status {response.status} (baseline {baseline_sig[0]}), length {response.length} (baseline {baseline_sig[1]})",
                            remediation="Change all default credentials and enforce a password policy.",
                            references=["https://owasp.org/www-community/vulnerabilities/Use_of_hard-coded_password"],
                            tags=["auth", "credentials"],
                            cwe="CWE-1392",
                            owasp="A07:2021",
                            host=ctx.target.host,
                            request=f"{form['method']} {form['action']} with {username}/{password}",
                        )
                    )
                    return

    await asyncio.gather(*(test(f) for f in login_forms[:5]), return_exceptions=True)
    return findings


@check("auth.bruteforce", "No brute-force protection", "Checks whether repeated failed logins are throttled", "auth", True, "medium")
async def check_bruteforce_protection(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    login_forms = []
    for page in ctx.pages.values():
        for form in page.forms:
            if any(f["type"] == "password" for f in form["fields"]):
                login_forms.append(form)
    if not login_forms:
        return findings
    form = login_forms[0]
    user_fields = [f for f in form["fields"] if f["type"] != "password"]
    pass_fields = [f for f in form["fields"] if f["type"] == "password"]
    if not user_fields or not pass_fields:
        return findings
    user_field, pass_field = user_fields[0]["name"], pass_fields[0]["name"]
    statuses: list[int] = []
    for i in range(8):
        response = await ctx.client.request(
            form["method"],
            form["action"],
            body=urllib.parse.urlencode({user_field: "nsv_probe", pass_field: f"wrong{i}nsc"}),
        )
        if response.error:
            return findings
        statuses.append(response.status)
    if all(s < 400 or s == 401 for s in statuses) and 429 not in statuses:
        findings.append(
            Finding(
                rule_id="auth.bruteforce",
                title="No rate limiting or lockout on login",
                severity="medium",
                confidence="medium",
                url=form["action"],
                method=form["method"],
                description="Eight consecutive failed logins were all accepted without throttling, CAPTCHA or lockout.",
                impact="Online password guessing is practical.",
                evidence=f"status sequence: {statuses}",
                remediation="Add rate limiting, account lockout, and a CAPTCHA after repeated failures.",
                references=["https://owasp.org/www-community/controls/Blocking_Brute_Force_Attacks"],
                tags=["auth", "bruteforce"],
                cwe="CWE-307",
                owasp="A07:2021",
                host=ctx.target.host,
            )
        )
    return findings


@check("auth.user-enum", "Username enumeration", "Compares responses for valid and invalid usernames", "auth", True, "low")
async def check_user_enumeration(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    candidates = ["/forgot-password", "/password/reset", "/reset-password", "/forgot", "/account/forgot"]
    for path in candidates:
        response = await ctx.client.request("GET", ctx.target.url_for(path))
        if not response.ok:
            continue
        forms = re.findall(r"<form([^>]*)>(.*?)</form>", response.text, re.I | re.S)
        if not forms:
            continue
        for attrs, inner in forms:
            action = re.search(r'action\s*=\s*"([^"]*)"', attrs)
            method = re.search(r'method\s*=\s*"([^"]*)"', attrs)
            inputs = re.findall(r"<input([^>]*)>", inner, re.I)
            field = None
            for input_attrs in inputs:
                name = re.search(r'name\s*=\s*"([^"]*)"', input_attrs)
                if name and re.search(r"email|user|login|account", name.group(1), re.I):
                    field = name.group(1)
                    break
            if not field:
                continue
            action_url = urllib.parse.urljoin(response.url, action.group(1)) if action else response.url
            http_method = (method.group(1) if method else "post").upper()
            valid = await ctx.client.request(http_method, action_url, body=urllib.parse.urlencode({field: "admin@example.com"}))
            invalid = await ctx.client.request(http_method, action_url, body=urllib.parse.urlencode({field: "nsv" + sha256_hex(str(time.time()))[:10] + "@example.com"}))
            if valid.error or invalid.error:
                continue
            similarity = difflib.SequenceMatcher(None, valid.text[:8000], invalid.text[:8000]).ratio()
            if similarity < 0.95:
                findings.append(
                    Finding(
                        rule_id="auth.user-enum",
                        title="Username enumeration via password reset",
                        severity="low",
                        confidence="medium",
                        url=action_url,
                        method=http_method,
                        description="The reset form answers differently for existing and non-existing accounts.",
                        impact="Attackers can build a list of valid usernames for targeted attacks.",
                        evidence=f"response similarity {similarity:.2f} between valid and invalid accounts",
                        remediation="Return an identical response regardless of whether the account exists.",
                        references=["https://owasp.org/www-project-web-security-testing-guide/latest/4-Web_Application_Security_Testing/03-Identity_Management_Testing/04-Testing_for_Account_Enumeration_and_Guessable_User_Account"],
                        tags=["auth", "enumeration"],
                        cwe="CWE-204",
                        owasp="A07:2021",
                        host=ctx.target.host,
                    )
                )
            break
    return findings


@check("auth.tls-transport", "Login form over plaintext HTTP", "Checks whether credentials can be submitted insecurely", "auth", False, "high")
async def check_login_transport(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    for page in ctx.pages.values():
        for form in page.forms:
            has_password = any(f["type"] == "password" for f in form["fields"])
            if has_password and form["action"].startswith("http://"):
                findings.append(
                    Finding(
                        rule_id="auth.tls-transport",
                        title="Login form submits over HTTP",
                        severity="high",
                        confidence="high",
                        url=page.url,
                        method=form["method"],
                        description=f"The form posts credentials to {form['action']} over plaintext HTTP.",
                        impact="Credentials are readable by anyone on the network path.",
                        evidence=f"form action: {form['action']}",
                        remediation="Serve the login page over HTTPS and post to an https:// URL.",
                        references=["https://cheatsheetseries.owasp.org/cheatsheets/Transport_Layer_Security_Cheat_Sheet.html"],
                        tags=["auth", "tls"],
                        cwe="CWE-319",
                        owasp="A02:2021",
                        host=ctx.target.host,
                    )
                )
    return findings


@check("auth.csrf", "Missing CSRF protection", "Checks state-changing forms for anti-CSRF tokens", "auth", False, "medium")
async def check_csrf(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    for page in ctx.pages.values():
        for form in page.forms:
            if form["method"] not in ("POST", "PUT", "PATCH", "DELETE"):
                continue
            if any(f["type"] == "password" for f in form["fields"]):
                continue  # login forms are covered by other checks
            if form["has_csrf_token"]:
                continue
            findings.append(
                Finding(
                    rule_id="auth.csrf",
                    title=f"Form without CSRF token ({form['method']} {form['action']})",
                    severity="medium",
                    confidence="medium",
                    url=page.url,
                    method=form["method"],
                    description="The form has no CSRF token, so it may be submittable from another site.",
                    impact="An attacker can make a logged-in user perform state-changing actions.",
                    evidence=f"fields: {', '.join(f['name'] for f in form['fields'] if f['name'])[:120]}",
                    remediation="Add a per-session CSRF token and verify it server-side.",
                    references=["https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html"],
                    tags=["csrf", "session"],
                    cwe="CWE-352",
                    owasp="A01:2021",
                    host=ctx.target.host,
                )
            )
            break
    return findings


# ---------------------------------------------------------------------------
# Network / infrastructure checks
# ---------------------------------------------------------------------------


@check("net.dns.email", "Email security records", "Checks SPF, DMARC and DKIM configuration", "network", False, "medium")
async def check_email_security(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    domain = ctx.target.host
    spf = await ctx.dns.txt(domain)
    has_spf = any(v.startswith("v=spf1") for v in spf)
    if not has_spf:
        findings.append(
            Finding(
                rule_id="net.dns.spf",
                title="No SPF record",
                severity="medium",
                confidence="high",
                url=ctx.target.url_for("/"),
                description=f"{domain} has no SPF TXT record, so any host may send mail as this domain.",
                impact="E-mail spoofing and phishing against your customers.",
                evidence="no v=spf1 TXT record found",
                remediation="Publish an SPF record, e.g. v=spf1 include:_spf.example.com ~all",
                references=["https://datatracker.ietf.org/doc/html/rfc7208"],
                tags=["dns", "email"],
                cwe="CWE-290",
                owasp="A05:2021",
                host=domain,
            )
        )
    else:
        record = next(v for v in spf if v.startswith("v=spf1"))
        if record.rstrip().endswith("+all"):
            findings.append(
                Finding(
                    rule_id="net.dns.spf-all",
                    title="SPF record uses +all",
                    severity="high",
                    confidence="high",
                    url=ctx.target.url_for("/"),
                    description="An SPF record ending in +all authorises every host on the internet.",
                    evidence=record[:200],
                    remediation="Use -all or ~all.",
                    tags=["dns", "email"],
                    cwe="CWE-290",
                    host=domain,
                )
            )
    dmarc = await ctx.dns.txt(f"_dmarc.{domain}")
    has_dmarc = any(v.startswith("v=DMARC1") for v in dmarc)
    if not has_dmarc:
        findings.append(
            Finding(
                rule_id="net.dns.dmarc",
                title="No DMARC record",
                severity="medium",
                confidence="high",
                url=ctx.target.url_for("/"),
                description=f"_dmarc.{domain} has no DMARC policy.",
                impact="Spoofed mail from this domain is not rejected or reported.",
                evidence="no v=DMARC1 TXT record",
                remediation="Publish _dmarc TXT: v=DMARC1; p=reject; rua=mailto:dmarc@example.com",
                references=["https://datatracker.ietf.org/doc/html/rfc7489"],
                tags=["dns", "email"],
                cwe="CWE-290",
                host=domain,
            )
        )
    else:
        policy = next(v for v in dmarc if v.startswith("v=DMARC1"))
        if "p=none" in policy:
            findings.append(
                Finding(
                    rule_id="net.dns.dmarc-none",
                    title="DMARC policy is p=none",
                    severity="low",
                    confidence="high",
                    url=ctx.target.url_for("/"),
                    description="A DMARC policy of none only monitors; it does not block spoofed mail.",
                    evidence=policy[:200],
                    remediation="Move to p=quarantine and then p=reject.",
                    tags=["dns", "email"],
                    host=domain,
                )
            )
    caa = await ctx.dns.resolve(domain, "CAA")
    if not caa:
        findings.append(
            Finding(
                rule_id="net.dns.caa",
                title="No CAA record",
                severity="low",
                confidence="medium",
                url=ctx.target.url_for("/"),
                description="Without a CAA record any CA may issue certificates for this domain.",
                evidence="no CAA record",
                remediation="Publish CAA: 0 issue \"letsencrypt.org\"",
                references=["https://datatracker.ietf.org/doc/html/rfc8659"],
                tags=["dns", "tls"],
                host=domain,
            )
        )
    return findings


@check("net.dns.axfr", "Zone transfer allowed", "Attempts an AXFR against the domain's nameservers", "network", True, "high")
async def check_zone_transfer(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    if not ctx.config.authorized:
        return findings
    nameservers = [str(v) for v in await ctx.dns.resolve(ctx.target.host, "NS")]
    for ns in nameservers[:3]:
        records = await ctx.dns.axfr(ctx.target.host)
        if records and len(records) > 3:
            findings.append(
                Finding(
                    rule_id="net.dns.axfr",
                    title=f"DNS zone transfer permitted by {ns}",
                    severity="high",
                    confidence="high",
                    url=ctx.target.url_for("/"),
                    description=f"The nameserver {ns} returned {len(records)} records for an AXFR query.",
                    impact="The complete DNS zone is disclosed, mapping the whole infrastructure.",
                    evidence=f"{len(records)} records transferred, e.g. {records[0].name}",
                    remediation="Restrict AXFR to authorised secondary servers only.",
                    references=["https://datatracker.ietf.org/doc/html/rfc5936"],
                    tags=["dns", "network"],
                    cwe="CWE-200",
                    host=ctx.target.host,
                )
            )
            break
    return findings


@check("net.subdomain-takeover", "Subdomain takeover", "Checks dangling CNAMEs for takeover candidates", "network", False, "high")
async def check_subdomain_takeover(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    takeover_fingerprints = [
        (r"\.s3[.\-].*amazonaws\.com", "There isn't a GitHub Pages site here", "AWS S3"),
        (r"\.github\.io", "There isn't a GitHub Pages site here", "GitHub Pages"),
        (r"\.herokuapp\.com", "No such app", "Heroku"),
        (r"\.azurewebsites\.net", "Web App - Pair your domain", "Azure"),
        (r"\.cloudfront\.net", "Bad request", "CloudFront"),
        (r"\.fastly\.net", "Fastly error: unknown domain", "Fastly"),
        (r"\.shopify\.com", "Sorry, this shop is currently unavailable", "Shopify"),
        (r"\.tumblr\.com", "There's nothing here", "Tumblr"),
        (r"\.wordpress\.com", "Do you want to register", "WordPress.com"),
        (r"\.pantheon\.io", "404 error unknown site", "Pantheon"),
        (r"\.bitbucket\.io", "Repository not found", "Bitbucket"),
        (r"\.ghost\.io", "Domain error", "Ghost"),
        (r"\.helpjuice\.com", "no settings for this domain", "Helpjuice"),
        (r"\.helpscoutdocs\.com", "No settings were found for this company", "HelpScout"),
        (r"\.cargo\.collective", "404 Not Found", "Cargo"),
        (r"\.statuspage\.io", "You are being redirected", "StatusPage"),
        (r"\.tave\.com", "<h1>Error 404", "Tave"),
        (r"\.uservoice\.com", "This UserVoice subdomain is currently available", "UserVoice"),
        (r"\.surge\.sh", "project not found", "Surge"),
        (r"\.zendesk\.com", "Help Center Closed", "Zendesk"),
    ]
    names = set(ctx.data.get("subdomains", []))
    for page in ctx.pages.values():
        names.add(urllib.parse.urlsplit(page.url).hostname or "")
    for name in sorted(n for n in names if n):
        cnames = [str(v) for v in await ctx.dns.resolve(name, "CNAME")]
        for cname in cnames:
            for pattern, error_text, service in takeover_fingerprints:
                if re.search(pattern, cname, re.I):
                    for scheme in ("https", "http"):
                        response = await ctx.client.request("GET", f"{scheme}://{name}")
                        if response.error:
                            continue
                        if error_text.lower() in response.text.lower() or response.status == 404:
                            findings.append(
                                Finding(
                                    rule_id="net.subdomain-takeover",
                                    title=f"Subdomain takeover possible: {name}",
                                    severity="high",
                                    confidence="high",
                                    url=response.url,
                                    description=f"{name} has a CNAME to {cname} ({service}), which is not claimed.",
                                    impact="An attacker can claim the target and serve content from your domain.",
                                    evidence=f"CNAME {cname} -> {error_text!r}",
                                    remediation=f"Remove the dangling CNAME or claim the {service} resource.",
                                    references=["https://github.com/EdOverflow/can-i-take-over-xyz"],
                                    tags=["dns", "takeover"],
                                    cwe="CWE-350",
                                    host=name,
                                )
                            )
                        break
    return findings


@check("net.ports", "Exposed network services", "Scans for risky open ports", "network", True, "high")
async def check_ports(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    if not ctx.config.ports:
        return findings
    scanner = PortScanner(ctx.log, timeout=ctx.config.timeout)
    results = await scanner.scan(ctx.target.host, ctx.config.ports)
    ctx.data["ports"] = [r.to_dict() for r in results]
    for result in results:
        if result.port in RISKY_PORTS:
            severity, reason = RISKY_PORTS[result.port]
            findings.append(
                Finding(
                    rule_id=f"net.port.{result.port}",
                    title=f"Risky service exposed on port {result.port} ({result.service or 'unknown'})",
                    severity=severity,
                    confidence="high",
                    url=f"{ctx.target.scheme}://{ctx.target.host}:{result.port}",
                    description=f"{reason}.",
                    impact="The service is reachable from the internet and may be unauthenticated.",
                    evidence=f"port {result.port} open, banner: {result.banner[:100] or 'none'}",
                    remediation="Close the port or restrict it with a firewall and require authentication.",
                    references=["https://owasp.org/www-community/controls/"],
                    tags=["network", "ports"],
                    cwe="CWE-1327",
                    owasp="A05:2021",
                    host=ctx.target.host,
                )
            )
    return findings


@check("net.tls", "TLS configuration", "Analyses protocol versions and the certificate chain", "network", False, "high")
async def check_tls(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    if ctx.target.scheme != "https" and 443 not in ctx.config.ports:
        findings.append(
            Finding(
                rule_id="net.tls.absent",
                title="Site does not use HTTPS",
                severity="high",
                confidence="high",
                url=ctx.target.url_for("/"),
                description="The target is served over plaintext HTTP.",
                impact="All traffic, including credentials, can be read and modified in transit.",
                evidence=f"scheme: {ctx.target.scheme}",
                remediation="Serve the site over HTTPS with a valid certificate and redirect HTTP to HTTPS.",
                references=["https://cheatsheetseries.owasp.org/cheatsheets/Transport_Layer_Security_Cheat_Sheet.html"],
                tags=["tls", "network"],
                cwe="CWE-319",
                owasp="A02:2021",
                host=ctx.target.host,
            )
        )
        return findings
    analyser = TlsAnalyser(ctx.log, timeout=ctx.config.timeout)
    report = await analyser.analyse(ctx.target.host, 443 if ctx.target.scheme == "https" else 443)
    ctx.data["tls"] = report.to_dict()
    for issue in report.issues:
        severity = "high"
        if "expired" in issue or "self-signed" in issue:
            severity = "high"
        elif "not yet valid" in issue or "hostname" in issue:
            severity = "high"
        elif "deprecated" in issue:
            severity = "medium"
        elif "weak cipher" in issue or "too small" in issue or "weak signature" in issue:
            severity = "medium"
        else:
            severity = "low"
        findings.append(
            Finding(
                rule_id="net.tls.issue",
                title=f"TLS issue: {issue}",
                severity=severity,
                confidence="high",
                url=f"https://{ctx.target.host}",
                description=issue,
                impact="Weak TLS configuration allows interception or impersonation.",
                evidence=f"grade {report.grade}, negotiated {report.negotiated.get('version')} {report.negotiated.get('cipher')}",
                remediation="Disable legacy protocols and weak ciphers; use a certificate from a trusted CA with a strong key.",
                references=["https://wiki.mozilla.org/Security/Server_Side_TLS"],
                tags=["tls", "network", "crypto"],
                cwe="CWE-327",
                owasp="A02:2021",
                host=ctx.target.host,
            )
        )
    cert = report.certificate
    if cert and cert.days_left is not None and 0 <= cert.days_left < 30:
        findings.append(
            Finding(
                rule_id="net.tls.expiring",
                title=f"Certificate expires in {cert.days_left} days",
                severity="low",
                confidence="high",
                url=f"https://{ctx.target.host}",
                description=f"The certificate for {cert.common_name or ctx.target.host} expires on {cert.not_after}.",
                evidence=f"not_after: {cert.not_after}",
                remediation="Renew the certificate and automate renewal.",
                tags=["tls", "certificate"],
                host=ctx.target.host,
            )
        )
    return findings


@check("net.http-to-https", "HTTP to HTTPS redirect", "Checks whether HTTP redirects to HTTPS", "network", False, "medium")
async def check_http_redirect(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    response = await ctx.client.request("GET", f"http://{ctx.target.host}/", allow_redirects=False)
    if response.error:
        return findings
    location = response.header("location", "")
    if response.status not in (301, 302, 307, 308) or not location.startswith("https://"):
        findings.append(
            Finding(
                rule_id="net.http-to-https",
                title="HTTP does not redirect to HTTPS",
                severity="medium",
                confidence="medium",
                url=f"http://{ctx.target.host}/",
                description="The plaintext site does not redirect to HTTPS, so visitors may stay on HTTP.",
                impact="Session cookies and credentials can be transmitted in cleartext.",
                evidence=f"status {response.status}, location {location or 'absent'}",
                remediation="Return a 301 redirect to the HTTPS URL and enable HSTS.",
                tags=["tls", "network"],
                cwe="CWE-319",
                owasp="A02:2021",
                host=ctx.target.host,
            )
        )
    return findings


@check("net.http2", "HTTP/2 support", "Checks ALPN negotiation for h2", "network", False, "info")
async def check_http2(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    if ctx.target.scheme != "https":
        return findings
    tls_data = ctx.data.get("tls", {})
    alpn = tls_data.get("alpn") or []
    if "h2" not in alpn:
        findings.append(
            Finding(
                rule_id="net.http2",
                title="HTTP/2 not negotiated",
                severity="info",
                confidence="medium",
                url=f"https://{ctx.target.host}",
                description="The server does not advertise h2 via ALPN.",
                evidence=f"alpn: {alpn or 'none'}",
                remediation="Enable HTTP/2 for better performance and header compression.",
                tags=["network", "performance"],
                host=ctx.target.host,
            )
        )
    return findings


@check("net.hsts-preload", "HSTS preload eligibility", "Checks the preload directive", "network", False, "info")
async def check_hsts_preload(ctx: CheckContext) -> list[Finding]:
    findings: list[Finding] = []
    if not ctx.baseline:
        return findings
    value = ctx.baseline.header("strict-transport-security", "")
    if "max-age=" in value and "includeSubDomains" in value and "preload" not in value:
        findings.append(
            Finding(
                rule_id="net.hsts-preload",
                title="HSTS preload directive missing",
                severity="info",
                confidence="high",
                url=ctx.baseline.url,
                description="The HSTS header lacks the preload directive, so first visits are not protected.",
                evidence=f"strict-transport-security: {value}",
                remediation="Add '; preload' and submit the domain to hstspreload.org.",
                references=["https://hstspreload.org/"],
                tags=["tls", "hardening"],
                host=ctx.target.host,
            )
        )
    return findings


# ---------------------------------------------------------------------------
# Scanner orchestration
# ---------------------------------------------------------------------------


@dataclass
class ScanResult:
    target: Target
    started_at: str
    finished_at: str = ""
    duration: float = 0.0
    findings: list[Finding] = field(default_factory=list)
    techs: list[TechFingerprint] = field(default_factory=list)
    pages: dict[str, CrawledPage] = field(default_factory=dict)
    ports: list[PortResult] = field(default_factory=list)
    subdomains: list[SubdomainResult] = field(default_factory=list)
    tls: dict[str, Any] = field(default_factory=dict)
    secrets: list[dict[str, str]] = field(default_factory=list)
    emails: set[str] = field(default_factory=set)
    stats: dict[str, Any] = field(default_factory=dict)
    errors: list[str] = field(default_factory=list)

    @property
    def severity_counts(self) -> dict[str, int]:
        counts = {s: 0 for s in SEV_ORDER}
        for finding in self.findings:
            counts[finding.severity] = counts.get(finding.severity, 0) + 1
        return counts

    @property
    def risk_score(self) -> float:
        """Weighted 0-100 score derived from findings."""
        if not self.findings:
            return 0.0
        weights = {"critical": 25, "high": 12, "medium": 5, "low": 2, "info": 0.5}
        total = sum(weights.get(f.severity, 0) for f in self.findings)
        return min(100.0, round(total, 1))

    @property
    def risk_grade(self) -> str:
        score = self.risk_score
        for threshold, grade in ((90, "F"), (70, "D"), (50, "C"), (30, "B"), (12, "A"), (0, "A+")):
            if score >= threshold:
                return grade
        return "A+"

    def to_dict(self) -> dict[str, Any]:
        return {
            "target": {
                "url": self.target.url,
                "host": self.target.host,
                "port": self.target.port,
                "scheme": self.target.scheme,
                "ip": self.target.ip,
            },
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "duration_seconds": round(self.duration, 2),
            "risk_score": self.risk_score,
            "risk_grade": self.risk_grade,
            "severity_counts": self.severity_counts,
            "findings": [f.to_dict() for f in self.findings],
            "technologies": [t.to_dict() for t in self.techs],
            "pages": {url: page.to_dict() for url, page in self.pages.items()},
            "ports": [p.to_dict() for p in self.ports],
            "subdomains": [s.to_dict() for s in self.subdomains],
            "tls": self.tls,
            "secrets": self.secrets,
            "emails": sorted(self.emails),
            "stats": self.stats,
            "errors": self.errors,
        }


class Scanner:
    """Drives the whole scan: recon, crawl, checks, reporting."""

    def __init__(self, config: ScanConfig, log: Log | None = None) -> None:
        self.config = config
        self.log = log or Log("info", color=not config.no_color)
        C.enabled = not config.no_color
        self.target = Target.from_url(config.target)
        self.result = ScanResult(target=self.target, started_at=datetime.datetime.now(datetime.timezone.utc).isoformat())
        self.client: HttpClient | None = None
        self.dns: DnsClient | None = None
        self._start = 0.0

    # -- lifecycle --------------------------------------------------------
    async def run(self) -> ScanResult:
        self._start = time.monotonic()
        started = datetime.datetime.now(datetime.timezone.utc)
        self.result.started_at = started.isoformat()
        self.log.rule(f"novascan {__version__}  ·  {self.target.url}")
        if not self.config.authorized and self.config.active:
            self.log.warn("active checks are enabled without --authorized; only passive checks will run")
            self.config.active = False

        headers = dict(self.config.headers)
        if self.config.cookies:
            headers["Cookie"] = self.config.cookies
        if self.config.auth:
            raw = f"{self.config.auth[0]}:{self.config.auth[1]}".encode()
            headers["Authorization"] = "Basic " + base64.b64encode(raw).decode()

        self.client = HttpClient(
            log=self.log,
            timeout=self.config.timeout,
            user_agent=self.config.user_agent,
            verify_tls=self.config.verify_tls,
            proxy=self.config.proxy,
            rate_limit=self.config.rate_limit,
            host_concurrency=min(self.config.concurrency, 8),
            follow_redirects=self.config.follow_redirects,
            headers=headers,
        )
        self.client._dns = None
        self.dns = DnsClient(log=self.log, timeout=min(self.config.timeout, 6.0), servers=self.config.dns_servers or None)

        try:
            await self._phase_resolve()
            await self._phase_recon()
            await self._phase_crawl()
            await self._phase_fingerprint()
            await self._phase_checks()
            await self._phase_network()
        except asyncio.CancelledError:
            self.result.errors.append("scan cancelled")
            raise
        except Exception as exc:
            self.result.errors.append(f"{type(exc).__name__}: {exc}")
            self.log.error(f"scan phase failed: {exc}")
        finally:
            await self.client.close()

        self.result.duration = time.monotonic() - self._start
        self.result.finished_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
        self.result.stats = {
            "requests": self.client.stats["requests"],
            "retries": self.client.stats["retries"],
            "errors": self.client.stats["errors"],
            "bytes": self.client.stats["bytes"],
            "dns_queries": self.dns.stats["queries"],
            "pages_crawled": len(self.result.pages),
        }
        self.result.findings.sort(key=lambda f: f.sort_key())
        return self.result

    # -- phases -----------------------------------------------------------
    async def _phase_resolve(self) -> None:
        self.log.rule("resolve")
        if is_ip_literal(self.target.host):
            self.target.ip = self.target.host
            self.log.ok(f"target is an IP literal: {self.target.ip}")
            return
        records = await self.dns.resolve_all(self.target.host, "A")
        if not records:
            self.log.error(f"cannot resolve {self.target.host}")
            self.result.errors.append(f"DNS resolution failed for {self.target.host}")
            return
        self.target.ip = str(records[0])
        self.log.ok(f"{self.target.host} -> {', '.join(str(r) for r in records[:4])}")
        aaaa = await self.dns.resolve_all(self.target.host, "AAAA")
        if aaaa:
            self.log.info(f"IPv6: {', '.join(str(r) for r in aaaa[:2])}")
        ptr = await self.dns.reverse(self.target.ip)
        if ptr:
            self.log.info(f"PTR: {ptr[0]}")
        ns_records = await self.dns.resolve(self.target.host, "NS")
        if ns_records:
            self.log.info(f"nameservers: {', '.join(str(r) for r in ns_records[:4])}")
        mx_records = await self.dns.resolve(self.target.host, "MX")
        if mx_records:
            self.log.info(f"MX: {', '.join(f'{p} {h}' for p, h in mx_records[:3])}")

    async def _phase_recon(self) -> None:
        self.log.rule("recon")
        baseline = await self.client.request("GET", self.target.url_for("/"))
        if baseline.error:
            self.log.error(f"baseline request failed: {baseline.error}")
            self.result.errors.append(f"baseline request failed: {baseline.error}")
            return
        self.log.ok(
            f"GET / -> {baseline.status} {baseline.reason}  "
            f"{human_bytes(baseline.length)}  {baseline.elapsed * 1000:.0f}ms  "
            f"server={baseline.header('server') or '?'}"
        )
        if baseline.redirects:
            for hop in baseline.redirects:
                self.log.info(f"redirect: {hop}")
        if baseline.tls:
            self.log.info(
                f"TLS {baseline.tls.get('version')} · {baseline.tls.get('cipher')} · "
                f"alpn={baseline.tls.get('alpn') or 'none'}"
            )
        self.result.pages[self.target.url_for("/")] = CrawledPage(
            url=self.target.url_for("/"),
            status=baseline.status,
            content_type=baseline.content_type,
            headers=baseline.headers,
            size=baseline.length,
        )
        ctx = self._context(baseline=baseline)
        header_findings = analyse_security_headers(baseline)
        cookie_findings = analyse_cookies(baseline)
        self._record(header_findings + cookie_findings)
        for finding in header_findings + cookie_findings:
            if sev_at_least(finding.severity, "medium"):
                self.log.vuln(f"{finding.severity.upper():8} {finding.title}")
        self.result.secrets.extend(scan_secrets(baseline.text, baseline.url))

    async def _phase_crawl(self) -> None:
        self.log.rule("crawl")
        crawler = Crawler(self.client, self.target, self.config, self.log)
        pages = await crawler.run()
        self.result.pages.update(pages)
        self.result.emails = crawler.emails
        self.result.secrets.extend(crawler.secrets)
        self.log.ok(
            f"crawled {len(pages)} pages · "
            f"{sum(len(p.forms) for p in pages.values())} forms · "
            f"{sum(len(p.params) for p in pages.values())} parameters"
        )
        if crawler.emails:
            self.log.info(f"{len(crawler.emails)} e-mail addresses found")
        if crawler.secrets:
            self.log.warn(f"{len(crawler.secrets)} potential secrets in page content")
        if self.result.secrets:
            for secret in self.result.secrets[:10]:
                self.log.vuln(f"secret  {secret['severity']:8} {secret['kind']} at {secret['url']}")

    async def _phase_fingerprint(self) -> None:
        self.log.rule("fingerprint")
        baseline_url = self.target.url_for("/")
        baseline = None
        response = await self.client.request("GET", baseline_url)
        if not response.error:
            baseline = response
        if baseline is None:
            return
        techs = fingerprint(baseline, self.result.pages.values())
        self.result.techs = techs
        if techs:
            grouped: dict[str, list[str]] = defaultdict(list)
            for tech in techs:
                label = f"{tech.name} {tech.version}".strip()
                grouped[tech.category].append(label)
            for category, labels in sorted(grouped.items()):
                self.log.info(f"{category}: {', '.join(labels)}")
        else:
            self.log.info("no technologies identified")

    async def _phase_checks(self) -> None:
        self.log.rule("checks")
        baseline = await self.client.request("GET", self.target.url_for("/"))
        ctx = self._context(baseline=baseline if not baseline.error else None)
        selected = [c for c in CHECKS if self._should_run(c)]
        skipped = [c for c in CHECKS if not self._should_run(c)]
        if skipped:
            self.log.debug(f"skipped {len(skipped)} checks: {', '.join(c.check_id for c in skipped)}")
        progress = Progress(self.log, total=len(selected), label="checks")
        sem = asyncio.Semaphore(max(1, self.config.concurrency // 2))

        async def run_check(check_obj: Check) -> None:
            async with sem:
                started = time.monotonic()
                findings = await check_obj.run(ctx)
                elapsed = time.monotonic() - started
                self._record(findings)
                if findings:
                    worst = min(SEV_RANK.get(f.severity, 99) for f in findings)
                    worst_name = SEV_ORDER[worst]
                    self.log.vuln(f"{worst_name.upper():8} {check_obj.name}  ({len(findings)} finding(s), {elapsed:.1f}s)")
                else:
                    self.log.debug(f"{check_obj.check_id}: clean ({elapsed:.1f}s)")
                progress.advance()

        await asyncio.gather(*(run_check(c) for c in selected), return_exceptions=True)
        progress.close()

    async def _phase_network(self) -> None:
        if not self.config.scan_subdomains:
            return
        self.log.rule("subdomains")
        discovery = SubdomainDiscovery(self.dns, self.client, self.log, wordlist=self.config.wordlist or None)
        results = await discovery.run(self.target.host)
        self.result.subdomains = results
        ctx = self._context()
        ctx.data["subdomains"] = [r.name for r in results]
        if results:
            self.log.ok(f"{len(results)} subdomains discovered")
            for result in results[:15]:
                status = str(result.http_status) if result.http_status else "-"
                self.log.info(f"{result.name:40} {result.ips[0] if result.ips else '?':15} {status}")
        else:
            self.log.info("no subdomains discovered")
        takeover = await check_subdomain_takeover(ctx)
        self._record(takeover)

    # -- helpers ----------------------------------------------------------
    def _context(self, baseline: HttpResponse | None = None) -> CheckContext:
        return CheckContext(
            target=self.target,
            client=self.client,
            dns=self.dns,
            config=self.config,
            log=self.log,
            pages=self.result.pages,
            techs=self.result.techs,
            baseline=baseline,
            findings=self.result.findings,
            data={},
        )

    def _should_run(self, check_obj: Check) -> bool:
        if check_obj.active and not self.config.active:
            return False
        if not check_obj.active and self.config.profile == "active-only":
            return False
        return True

    def _record(self, findings: Iterable[Finding]) -> None:
        seen = {(f.rule_id, f.url, f.evidence[:80]) for f in self.result.findings}
        for finding in findings:
            key = (finding.rule_id, finding.url, finding.evidence[:80])
            if key in seen:
                continue
            seen.add(key)
            self.result.findings.append(finding)


# ---------------------------------------------------------------------------
# Reporters
# ---------------------------------------------------------------------------


def report_console(result: ScanResult, log: Log, show_evidence: bool = True) -> str:
    """Render the terminal report and return it as a string."""
    lines: list[str] = []
    out = lines.append
    counts = result.severity_counts
    grade = result.risk_grade
    grade_color = {"A+": C.GREEN, "A": C.GREEN, "B": C.CYAN, "C": C.YELLOW, "D": C.RED, "F": C.BG_RED + C.WHITE}[grade]

    out("")
    out(C.wrap("  ╭" + "─" * 62 + "╮", C.BLUE))
    out(C.wrap("  │", C.BLUE) + C.wrap(f"  scan complete · {result.target.url}".ljust(62), C.BOLD) + C.wrap("│", C.BLUE))
    out(C.wrap("  │", C.BLUE) + " " + C.wrap(f"risk {result.risk_score:5.1f}/100", grade_color, C.BOLD) + C.wrap(f"  grade {grade}".ljust(20), grade_color, C.BOLD) + C.wrap(f"{human_duration(result.duration)}".rjust(20), C.DIM) + C.wrap(" │", C.BLUE))
    out(C.wrap("  ╰" + "─" * 62 + "╯", C.BLUE))
    out("")
    badges = []
    for severity in SEV_ORDER:
        if counts.get(severity):
            badges.append(C.wrap(f" {severity.upper()} {counts[severity]} ", *([SEV_COLOR[severity]] if severity != "critical" else [SEV_COLOR[severity]])))
    if badges:
        out("  " + "  ".join(badges))
    else:
        out(C.wrap("  no findings", C.GREEN, C.BOLD))
    out("")

    if result.findings:
        out(C.wrap("  findings", C.BOLD, C.UNDER))
        out("")
        current = None
        for finding in result.findings:
            if finding.severity != current:
                current = finding.severity
                out(C.wrap(f"  {current.upper()}", SEV_COLOR[current], C.BOLD))
            out(f"    {C.wrap(finding.rule_id, C.GRAY)}  {C.wrap(finding.title, C.BOLD)}")
            out(f"      {C.wrap(finding.url, C.BLUE)}  {C.wrap(finding.method, C.DIM)}  {C.wrap(finding.confidence, C.DIM)}")
            if finding.description:
                out(f"      {C.wrap(_wrap_text(finding.description, 88), C.DIM)}")
            if show_evidence and finding.evidence:
                out(f"      {C.wrap('evidence:', C.YELLOW)} {_wrap_text(finding.evidence, 84)}")
            if finding.remediation:
                out(f"      {C.wrap('fix:', C.GREEN)} {_wrap_text(finding.remediation, 90)}")
            if finding.cwe:
                out(f"      {C.wrap(f'{finding.cwe} · {finding.owasp}', C.GRAY)}")
            out("")

    if result.techs:
        out(C.wrap("  technologies", C.BOLD, C.UNDER))
        grouped: dict[str, list[str]] = defaultdict(list)
        for tech in result.techs:
            grouped[tech.category].append(f"{tech.name} {tech.version}".strip())
        for category, labels in sorted(grouped.items()):
            out(f"    {C.wrap(category, C.CYAN):24} {', '.join(labels)}")
        out("")

    if result.subdomains:
        out(C.wrap("  subdomains", C.BOLD, C.UNDER))
        for sub in result.subdomains[:40]:
            status = C.wrap(str(sub.http_status), C.GREEN if sub.http_status and sub.http_status < 400 else C.RED) if sub.http_status else C.wrap("-", C.GRAY)
            out(f"    {sub.name:44} {C.wrap(sub.ips[0] if sub.ips else '?', C.DIM):16} {status}  {C.wrap(sub.title[:40], C.DIM)}")
        out("")

    if result.ports:
        out(C.wrap("  open ports", C.BOLD, C.UNDER))
        for port in result.ports:
            out(f"    {C.wrap(str(port.port).rjust(5), C.CYAN)}  {C.wrap((port.service or '?').ljust(14), C.BOLD)} {C.wrap(port.banner[:60], C.DIM)}")
        out("")

    if result.secrets:
        out(C.wrap("  secrets", C.BOLD, C.UNDER))
        for secret in result.secrets[:20]:
            out(f"    {C.wrap(secret['severity'].upper():8, SEV_COLOR.get(secret['severity'], C.GRAY))} {C.wrap(secret['kind'], C.BOLD)}  {C.wrap(secret['url'], C.BLUE)}")
            out(f"      {C.wrap(secret['match'], C.YELLOW)}")
        out("")

    if result.tls:
        out(C.wrap("  tls", C.BOLD, C.UNDER))
        out(f"    {C.wrap('grade', C.CYAN):20} {result.tls.get('grade', '?')}")
        out(f"    {C.wrap('negotiated', C.CYAN):20} {result.tls.get('negotiated', {}).get('version')} · {result.tls.get('negotiated', {}).get('cipher')}")
        for issue in result.tls.get("issues", []):
            out(f"    {C.wrap('!', C.YELLOW)} {issue}")
        out("")

    stats = result.stats
    out(C.wrap("  statistics", C.BOLD, C.UNDER))
    out(f"    {C.wrap('pages', C.CYAN):20} {stats.get('pages_crawled', 0)}")
    out(f"    {C.wrap('requests', C.CYAN):20} {stats.get('requests', 0)}  ({human_bytes(stats.get('bytes', 0))}, {stats.get('retries', 0)} retries)")
    out(f"    {C.wrap('dns queries', C.CYAN):20} {stats.get('dns_queries', 0)}")
    if result.errors:
        out(f"    {C.wrap('errors', C.RED):20} {len(result.errors)}")
    out("")
    text = "\n".join(lines)
    log.stream.write(text + "\n")
    log.stream.flush()
    return text


def _wrap_text(text: str, width: int) -> str:
    import textwrap

    return "\n      ".join(textwrap.wrap(text, width=width)) or ""


def report_json(result: ScanResult, pretty: bool = True) -> str:
    return json.dumps(result.to_dict(), indent=2 if pretty else None, sort_keys=False, default=str)


def report_csv(result: ScanResult) -> str:
    import io

    buffer = io.StringIO()
    writer = csv.writer(buffer)
    writer.writerow(["severity", "rule_id", "title", "confidence", "url", "method", "cwe", "owasp", "evidence"])
    for finding in result.findings:
        writer.writerow([finding.severity, finding.rule_id, finding.title, finding.confidence, finding.url, finding.method, finding.cwe, finding.owasp, finding.evidence[:300]])
    return buffer.getvalue()


def report_markdown(result: ScanResult) -> str:
    counts = result.severity_counts
    lines = [
        f"# Security scan report: {result.target.url}",
        "",
        f"- **Target:** `{result.target.url}` ({result.target.host})",
        f"- **Started:** {result.started_at}",
        f"- **Duration:** {human_duration(result.duration)}",
        f"- **Risk score:** {result.risk_score}/100 (grade {result.risk_grade})",
        f"- **Findings:** " + ", ".join(f"{counts.get(s, 0)} {s}" for s in SEV_ORDER),
        "",
        "## Technologies",
        "",
    ]
    if result.techs:
        for tech in result.techs:
            lines.append(f"- **{tech.name}** {tech.version} (`{tech.category}`) — {tech.evidence}")
    else:
        lines.append("_none identified_")
    lines += ["", "## Findings", ""]
    if not result.findings:
        lines.append("No findings.")
    for finding in result.findings:
        lines += [
            f"### [{finding.severity.upper()}] {finding.title}",
            "",
            f"- **Rule:** `{finding.rule_id}`",
            f"- **URL:** {finding.url}",
            f"- **Method:** `{finding.method}`",
            f"- **Confidence:** {finding.confidence}",
        ]
        if finding.cwe:
            lines.append(f"- **CWE:** {finding.cwe} · **OWASP:** {finding.owasp}")
        if finding.description:
            lines += ["", finding.description]
        if finding.impact:
            lines += ["", f"**Impact.** {finding.impact}"]
        if finding.evidence:
            lines += ["", "```", finding.evidence[:600], "```"]
        if finding.request:
            lines += ["", f"**Request.** `{finding.request}`"]
        if finding.remediation:
            lines += ["", f"**Remediation.** {finding.remediation}"]
        if finding.references:
            lines += ["", "**References.**"]
            lines += [f"- {ref}" for ref in finding.references]
        lines.append("")
    if result.secrets:
        lines += ["## Secrets", "", "| Severity | Kind | URL | Match |", "|---|---|---|---|"]
        for secret in result.secrets:
            lines.append(f"| {secret['severity']} | {secret['kind']} | {secret['url']} | `{secret['match']}` |")
        lines.append("")
    lines += [
        "## Statistics",
        "",
        f"- Pages crawled: {result.stats.get('pages_crawled', 0)}",
        f"- HTTP requests: {result.stats.get('requests', 0)}",
        f"- DNS queries: {result.stats.get('dns_queries', 0)}",
        f"- Bytes transferred: {human_bytes(result.stats.get('bytes', 0))}",
        "",
        f"_Generated by novascan {__version__}_",
    ]
    return "\n".join(lines) + "\n"


HTML_TEMPLATE = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>novascan report · __TARGET__</title>
<style>
:root{--bg:#0d1117;--panel:#161b22;--border:#30363d;--fg:#c9d1d9;--muted:#8b949e;
--crit:#ff4d4f;--high:#ff7a45;--med:#faad14;--low:#40a9ff;--info:#8b949e;--ok:#3fb950;}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
a{color:var(--low)}
.wrap{max-width:1100px;margin:0 auto;padding:32px 20px 80px}
header{border-bottom:1px solid var(--border);padding-bottom:20px;margin-bottom:24px}
h1{margin:0 0 8px;font-size:24px}
h2{margin:36px 0 14px;font-size:18px;border-bottom:1px solid var(--border);padding-bottom:8px}
.meta{color:var(--muted);font-size:13px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin:20px 0}
.card{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:14px}
.card .n{font-size:26px;font-weight:700}
.card .l{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.06em}
.grade{font-size:44px;font-weight:800;line-height:1}
table{width:100%;border-collapse:collapse;font-size:13px;margin:12px 0}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--border);vertical-align:top}
th{color:var(--muted);font-weight:600;font-size:11px;text-transform:uppercase;letter-spacing:.05em}
tr:hover td{background:#1c2128}
.sev{display:inline-block;padding:2px 8px;border-radius:10px;font-size:11px;font-weight:700;text-transform:uppercase;color:#fff}
.sev.critical{background:var(--crit)}.sev.high{background:var(--high)}.sev.medium{background:var(--med);color:#000}
.sev.low{background:var(--low)}.sev.info{background:var(--info);color:#000}
.finding{background:var(--panel);border:1px solid var(--border);border-left-width:3px;border-radius:6px;padding:14px 16px;margin:12px 0}
.finding.critical{border-left-color:var(--crit)}.finding.high{border-left-color:var(--high)}
.finding.medium{border-left-color:var(--med)}.finding.low{border-left-color:var(--low)}
.finding.info{border-left-color:var(--info)}
.finding h3{margin:0 0 6px;font-size:15px}
.finding .url{color:var(--low);font-size:12px;word-break:break-all}
.finding p{margin:8px 0}
pre{background:#0b0f14;border:1px solid var(--border);border-radius:6px;padding:10px;overflow:auto;font-size:12px}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.tag{display:inline-block;background:#21262d;border:1px solid var(--border);border-radius:10px;padding:1px 8px;font-size:11px;color:var(--muted);margin-right:4px}
.fix{color:var(--ok)}
.muted{color:var(--muted)}
footer{margin-top:48px;padding-top:16px;border-top:1px solid var(--border);color:var(--muted);font-size:12px}
</style>
</head>
<body><div class="wrap">
<header>
<h1>Security scan report</h1>
<div class="meta"><code>__TARGET__</code> · __HOST__ · __DATE__ · __DURATION__</div>
</header>
<div class="cards">
<div class="card"><div class="grade">__GRADE__</div><div class="l">risk grade</div></div>
<div class="card"><div class="n">__SCORE__</div><div class="l">risk score / 100</div></div>
<div class="card"><div class="n" style="color:var(--crit)">__CRIT__</div><div class="l">critical</div></div>
<div class="card"><div class="n" style="color:var(--high)">__HIGH__</div><div class="l">high</div></div>
<div class="card"><div class="n" style="color:var(--med)">__MED__</div><div class="l">medium</div></div>
<div class="card"><div class="n" style="color:var(--low)">__LOW__</div><div class="l">low</div></div>
<div class="card"><div class="n">__PAGES__</div><div class="l">pages crawled</div></div>
<div class="card"><div class="n">__REQS__</div><div class="l">http requests</div></div>
</div>
__SECTIONS__
<footer>Generated by novascan __VERSION__ · only scan systems you are authorised to test.</footer>
</div></body></html>
"""


def _esc(text: Any) -> str:
    return html_mod.escape(str(text if text is not None else ""))


def report_html(result: ScanResult) -> str:
    counts = result.severity_counts
    sections: list[str] = []
    if result.techs:
        rows = "".join(
            f"<tr><td>{_esc(t.name)}</td><td>{_esc(t.version or '-')}</td><td>{_esc(t.category)}</td>"
            f"<td>{t.confidence}%</td><td class='muted'>{_esc(t.evidence[:80])}</td></tr>"
            for t in result.techs
        )
        sections.append(f"<h2>Technologies</h2><table><tr><th>Name</th><th>Version</th><th>Category</th><th>Confidence</th><th>Evidence</th></tr>{rows}</table>")

    sections.append("<h2>Findings</h2>")
    if not result.findings:
        sections.append("<p class='muted'>No findings.</p>")
    for finding in result.findings:
        refs = "".join(f"<li><a href='{_esc(r)}'>{_esc(r)}</a></li>" for r in finding.references)
        tags = "".join(f"<span class='tag'>{_esc(t)}</span>" for t in finding.tags)
        sections.append(
            f"<div class='finding {finding.severity}'>"
            f"<h3><span class='sev {finding.severity}'>{_esc(finding.severity)}</span> {_esc(finding.title)}</h3>"
            f"<div class='url'>{_esc(finding.method)} <a href='{_esc(finding.url)}'>{_esc(finding.url)}</a></div>"
            f"<p>{_esc(finding.description)}</p>"
            + (f"<p><strong>Impact.</strong> {_esc(finding.impact)}</p>" if finding.impact else "")
            + (f"<pre>{_esc(finding.evidence[:800])}</pre>" if finding.evidence else "")
            + (f"<p><strong>Request.</strong> <code>{_esc(finding.request)}</code></p>" if finding.request else "")
            + (f"<p class='fix'><strong>Remediation.</strong> {_esc(finding.remediation)}</p>" if finding.remediation else "")
            + (f"<p class='muted'>{_esc(finding.cwe)} · {_esc(finding.owasp)} · confidence {_esc(finding.confidence)}</p>" if finding.cwe else "")
            + (f"<ul>{refs}</ul>" if refs else "")
            + (f"<div>{tags}</div>" if tags else "")
            + "</div>"
        )

    if result.subdomains:
        rows = "".join(
            f"<tr><td>{_esc(s.name)}</td><td>{_esc(', '.join(s.ips))}</td><td>{_esc(s.http_status or '-')}</td>"
            f"<td>{_esc(s.title)}</td><td>{_esc(s.source)}</td></tr>"
            for s in result.subdomains
        )
        sections.append(f"<h2>Subdomains</h2><table><tr><th>Name</th><th>IPs</th><th>Status</th><th>Title</th><th>Source</th></tr>{rows}</table>")

    if result.ports:
        rows = "".join(
            f"<tr><td>{p.port}</td><td>{_esc(p.service)}</td><td>{_esc(p.banner[:100])}</td></tr>" for p in result.ports
        )
        sections.append(f"<h2>Open ports</h2><table><tr><th>Port</th><th>Service</th><th>Banner</th></tr>{rows}</table>")

    if result.secrets:
        rows = "".join(
            f"<tr><td><span class='sev {s['severity']}'>{_esc(s['severity'])}</span></td><td>{_esc(s['kind'])}</td>"
            f"<td>{_esc(s['url'])}</td><td><code>{_esc(s['match'])}</code></td></tr>"
            for s in result.secrets
        )
        sections.append(f"<h2>Secrets</h2><table><tr><th>Severity</th><th>Kind</th><th>URL</th><th>Match</th></tr>{rows}</table>")

    if result.tls:
        issues = "".join(f"<li>{_esc(i)}</li>" for i in result.tls.get("issues", []))
        cert = result.tls.get("certificate") or {}
        sections.append(
            "<h2>TLS</h2><table>"
            f"<tr><th>Grade</th><td>{_esc(result.tls.get('grade'))}</td></tr>"
            f"<tr><th>Negotiated</th><td>{_esc(result.tls.get('negotiated', {}).get('version'))} · {_esc(result.tls.get('negotiated', {}).get('cipher'))}</td></tr>"
            f"<tr><th>Subject</th><td>{_esc(cert.get('subject', {}).get('CN', '-'))}</td></tr>"
            f"<tr><th>Issuer</th><td>{_esc(cert.get('issuer', {}).get('CN', '-'))}</td></tr>"
            f"<tr><th>Key</th><td>{_esc(cert.get('key_alg'))} {_esc(cert.get('key_size'))} bits</td></tr>"
            f"<tr><th>Expires</th><td>{_esc(cert.get('not_after'))} ({_esc(cert.get('days_left'))} days)</td></tr>"
            f"<tr><th>SAN</th><td>{_esc(', '.join(cert.get('san', [])[:8]))}</td></tr>"
            "</table>"
            + (f"<ul>{issues}</ul>" if issues else "")
        )

    sections.append(
        "<h2>Statistics</h2><table>"
        f"<tr><th>Pages crawled</th><td>{result.stats.get('pages_crawled', 0)}</td></tr>"
        f"<tr><th>HTTP requests</th><td>{result.stats.get('requests', 0)}</td></tr>"
        f"<tr><th>Bytes transferred</th><td>{human_bytes(result.stats.get('bytes', 0))}</td></tr>"
        f"<tr><th>DNS queries</th><td>{result.stats.get('dns_queries', 0)}</td></tr>"
        f"<tr><th>Retries</th><td>{result.stats.get('retries', 0)}</td></tr>"
        "</table>"
    )

    html = HTML_TEMPLATE
    replacements = {
        "__TARGET__": _esc(result.target.url),
        "__HOST__": _esc(result.target.host),
        "__DATE__": _esc(result.started_at[:19].replace("T", " ")),
        "__DURATION__": _esc(human_duration(result.duration)),
        "__GRADE__": _esc(result.risk_grade),
        "__SCORE__": _esc(result.risk_score),
        "__CRIT__": _esc(counts.get("critical", 0)),
        "__HIGH__": _esc(counts.get("high", 0)),
        "__MED__": _esc(counts.get("medium", 0)),
        "__LOW__": _esc(counts.get("low", 0)),
        "__PAGES__": _esc(result.stats.get("pages_crawled", 0)),
        "__REQS__": _esc(result.stats.get("requests", 0)),
        "__SECTIONS__": "\n".join(sections),
        "__VERSION__": _esc(__version__),
    }
    for key, value in replacements.items():
        html = html.replace(key, value)
    return html


REPORTERS: dict[str, Callable[[ScanResult], str]] = {
    "json": lambda r: report_json(r),
    "html": report_html,
    "csv": report_csv,
    "md": report_markdown,
    "markdown": report_markdown,
    "txt": lambda r: report_markdown(r),
}


def write_reports(result: ScanResult, outputs: Sequence[str], log: Log) -> list[str]:
    written: list[str] = []
    for spec in outputs:
        if spec == "console":
            continue
        path, _, fmt = spec.partition(":")
        fmt = (fmt or Path(path).suffix.lstrip(".") or "json").lower()
        reporter = REPORTERS.get(fmt)
        if not reporter:
            log.error(f"unknown report format '{fmt}' (from {spec})")
            continue
        try:
            content = reporter(result)
            target = Path(path)
            if target.parent and not target.parent.exists():
                target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content, encoding="utf-8")
            written.append(str(target))
            log.ok(f"wrote {target} ({human_bytes(len(content))})")
        except OSError as exc:
            log.error(f"cannot write {path}: {exc}")
    return written


# ---------------------------------------------------------------------------
# Command line interface
# ---------------------------------------------------------------------------

EPILOG = """examples:
  novascan.py https://example.com
  novascan.py example.com --active --authorized -o report.html -o report.json
  novascan.py https://example.com --profile deep --ports common --subdomains
  novascan.py https://example.com -q --min-severity high -o findings.csv
  novascan.py https://example.com --proxy http://127.0.0.1:8080 --rate 5

Only scan systems you own or have written permission to test.
Active checks (--active) send attack payloads and require --authorized.
"""

PROFILES = {
    "quick": {"max_pages": 40, "max_depth": 2, "concurrency": 8, "timeout": 8.0},
    "default": {"max_pages": 300, "max_depth": 4, "concurrency": 12, "timeout": 15.0},
    "deep": {"max_pages": 1200, "max_depth": 7, "concurrency": 24, "timeout": 20.0},
    "stealth": {"max_pages": 60, "max_depth": 3, "concurrency": 2, "timeout": 25.0, "rate_limit": 0.5, "host_delay": 1.0},
    "api": {"max_pages": 500, "max_depth": 6, "concurrency": 16, "timeout": 15.0},
}

PORT_PRESETS = {
    "common": COMMON_PORTS,
    "top100": COMMON_PORTS[:100],
    "top20": [80, 443, 22, 21, 25, 3306, 5432, 3389, 8080, 8443, 6379, 27017, 9200, 11211, 445, 139, 23, 5900, 1433, 1521],
    "web": [80, 443, 8080, 8443, 8000, 8008, 8888, 3000, 5000, 9000],
    "db": [1433, 1521, 3306, 5432, 6379, 9200, 9300, 27017, 11211, 5984, 9042, 2181],
    "all": list(range(1, 1025)),
}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="novascan",
        description="Advanced asynchronous web security scanner (single file, stdlib only).",
        epilog=EPILOG,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("target", nargs="?", help="target URL or hostname")
    parser.add_argument("-t", "--target", dest="target_opt", help="target URL (alternative to positional)")

    scope = parser.add_argument_group("scope")
    scope.add_argument("-a", "--active", action="store_true", help="enable active checks that send attack payloads")
    scope.add_argument("--authorized", action="store_true", help="confirm you are authorised to test the target (required for active checks)")
    scope.add_argument("--profile", choices=sorted(PROFILES), default="default", help="scan profile (default: default)")
    scope.add_argument("--max-pages", type=int, help="maximum pages to crawl")
    scope.add_argument("--max-depth", type=int, help="maximum crawl depth")
    scope.add_argument("--exclude", action="append", default=[], metavar="GLOB", help="exclude paths matching glob (repeatable)")
    scope.add_argument("--include", action="append", default=[], metavar="GLOB", help="only include paths matching glob (repeatable)")
    scope.add_argument("--subdomains", action="store_true", help="enumerate subdomains")
    scope.add_argument("--wordlist", type=Path, help="subdomain wordlist file (one name per line)")

    net = parser.add_argument_group("network")
    net.add_argument("-c", "--concurrency", type=int, help="concurrent requests")
    net.add_argument("--rate", type=float, metavar="N", help="global request rate limit (requests/second)")
    net.add_argument("--timeout", type=float, metavar="SEC", help="request timeout in seconds")
    net.add_argument("-A", "--user-agent", help="User-Agent header to send")
    net.add_argument("-H", "--header", action="append", default=[], metavar="K:V", help="extra header (repeatable)")
    net.add_argument("-b", "--cookie", metavar="STR", help="Cookie header to send")
    net.add_argument("--auth", metavar="USER:PASS", help="HTTP basic authentication credentials")
    net.add_argument("--proxy", metavar="URL", help="proxy URL, e.g. http://127.0.0.1:8080")
    net.add_argument("--dns", metavar="IP", action="append", default=[], help="DNS server to query (repeatable)")
    net.add_argument("--ports", metavar="SPEC", help="ports to scan: 'common', 'top100', 'top20', 'web', 'db', 'all' or a list like 80,443,8000-8100")
    net.add_argument("--no-follow-redirects", action="store_true", help="do not follow redirects")
    net.add_argument("--insecure", action="store_true", help="do not verify TLS certificates")

    out = parser.add_argument_group("output")
    out.add_argument("-o", "--output", action="append", default=[], metavar="FILE[:FMT]", help="write a report: report.json, report.html, report.csv, report.md, or file:json")
    out.add_argument("--min-severity", choices=SEV_ORDER, default="info", help="minimum severity to report (default: info)")
    out.add_argument("--no-color", action="store_true", help="disable coloured output")
    out.add_argument("-v", "--verbose", action="store_true", help="debug logging")
    out.add_argument("-q", "--quiet", action="store_true", help="only print the summary")
    out.add_argument("--no-evidence", action="store_true", help="hide evidence in the console report")
    out.add_argument("--version", action="version", version=f"novascan {__version__}")

    info = parser.add_argument_group("information")
    info.add_argument("--list-checks", action="store_true", help="list every check and exit")
    info.add_argument("--list-profiles", action="store_true", help="list scan profiles and exit")
    info.add_argument("--list-ports", action="store_true", help="list the port presets and exit")
    return parser


def parse_ports(spec: str | None) -> list[int]:
    if not spec:
        return []
    if spec in PORT_PRESETS:
        return list(PORT_PRESETS[spec])
    ports: list[int] = []
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            start_s, _, end_s = part.partition("-")
            try:
                start, end = int(start_s), int(end_s)
            except ValueError:
                continue
            ports.extend(range(max(1, start), min(65535, end) + 1))
        else:
            try:
                ports.append(int(part))
            except ValueError:
                continue
    return sorted(set(p for p in ports if 1 <= p <= 65535))


def config_from_args(args: argparse.Namespace) -> ScanConfig:
    profile = dict(PROFILES[args.profile])
    config = ScanConfig(target=args.target or args.target_opt or "")
    for key, value in profile.items():
        if hasattr(config, key):
            setattr(config, key, value)
    if args.max_pages:
        config.max_pages = args.max_pages
    if args.max_depth:
        config.max_depth = args.max_depth
    if args.concurrency:
        config.concurrency = args.concurrency
    if args.timeout:
        config.timeout = args.timeout
    if args.user_agent:
        config.user_agent = args.user_agent
    if args.rate:
        config.rate_limit = args.rate
    if args.proxy:
        config.proxy = args.proxy
    if args.insecure:
        config.verify_tls = False
    if args.no_follow_redirects:
        config.follow_redirects = False
    if args.cookie:
        config.cookies = args.cookie
    if args.auth:
        user, _, password = args.auth.partition(":")
        config.auth = (user, password)
    if args.dns:
        config.dns_servers = args.dns
    config.exclude = list(args.exclude)
    config.include = list(args.include)
    config.scan_subdomains = args.subdomains
    config.active = args.active
    config.authorized = args.authorized
    config.no_color = args.no_color
    config.verbose = args.verbose
    config.quiet = args.quiet
    config.min_severity = args.min_severity
    config.output = list(args.output)
    config.ports = parse_ports(args.ports)
    for header in args.header:
        key, _, value = header.partition(":")
        if key.strip():
            config.headers[key.strip()] = value.strip()
    if args.wordlist:
        try:
            config.wordlist = [
                line.strip()
                for line in args.wordlist.read_text().splitlines()
                if line.strip() and not line.startswith("#")
            ]
        except OSError as exc:
            print(f"warning: cannot read wordlist {args.wordlist}: {exc}", file=sys.stderr)
    if args.profile == "stealth":
        config.host_delay = 1.0
    return config


def print_checks() -> None:
    print(f"{'ID':34} {'MODE':8} {'SEVERITY':9} NAME")
    print("-" * 100)
    for check_obj in sorted(CHECKS, key=lambda c: (c.category, c.check_id)):
        mode = "active" if check_obj.active else "passive"
        print(f"{check_obj.check_id:34} {mode:8} {check_obj.severity:9} {check_obj.name}")
    print(f"\n{len(CHECKS)} checks: {sum(1 for c in CHECKS if c.active)} active, {sum(1 for c in CHECKS if not c.active)} passive")


def print_profiles() -> None:
    print(f"{'PROFILE':10} {'PAGES':7} {'DEPTH':6} {'CONC':5} {'TIMEOUT':8} NOTES")
    print("-" * 78)
    notes = {
        "quick": "fast surface pass",
        "default": "balanced",
        "deep": "thorough, slow",
        "stealth": "slow and polite",
        "api": "tuned for JSON APIs",
    }
    for name, settings in PROFILES.items():
        print(f"{name:10} {settings['max_pages']:<7} {settings['max_depth']:<6} {settings['concurrency']:<5} {settings['timeout']:<8} {notes[name]}")


def print_port_presets() -> None:
    for name, ports in PORT_PRESETS.items():
        print(f"{name:10} {len(ports):5} ports: {ports[:12]}{'…' if len(ports) > 12 else ''}")


async def amain(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.list_checks:
        print_checks()
        return 0
    if args.list_profiles:
        print_profiles()
        return 0
    if args.list_ports:
        print_port_presets()
        return 0
    target = args.target or args.target_opt
    if not target:
        parser.print_help()
        return 2

    config = config_from_args(args)
    level = "debug" if config.verbose else ("quiet" if config.quiet else "info")
    log = Log(level=level, color=not config.no_color, show_time=config.verbose)

    scanner = Scanner(config, log)
    try:
        result = await scanner.run()
    except KeyboardInterrupt:
        log.error("interrupted")
        return 130
    except Exception as exc:
        log.error(f"scan failed: {type(exc).__name__}: {exc}")
        if config.verbose:
            raise
        return 1

    findings = [f for f in result.findings if sev_at_least(f.severity, config.min_severity)]
    result.findings = findings
    if "console" in config.output or not config.output:
        report_console(result, log, show_evidence=not args.no_evidence)
    if config.output:
        write_reports(result, [o for o in config.output if o != "console"], log)

    counts = result.severity_counts
    critical_high = counts.get("critical", 0) + counts.get("high", 0)
    if not config.quiet:
        log.blank()
        if critical_high:
            log.warn(f"{critical_high} critical/high finding(s) — risk grade {result.risk_grade} ({result.risk_score}/100)")
        elif result.findings:
            log.info(f"{len(result.findings)} finding(s) — risk grade {result.risk_grade} ({result.risk_score}/100)")
        else:
            log.ok("no findings — risk grade A+")
    return 1 if critical_high else 0


def main(argv: Sequence[str] | None = None) -> int:
    if argv is None and len(sys.argv) > 1 and sys.argv[1] in ("-h", "--help"):
        pass
    try:
        return asyncio.run(amain(argv))
    except KeyboardInterrupt:
        print("\ninterrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
