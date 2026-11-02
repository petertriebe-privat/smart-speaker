"""Builds data/cert/x509_crt_bundle.bin: Mozilla's root CAs in the format of ESP-IDF's
certificate bundle (WiFiClientSecure::setCACertBundle), for servers whose root the firmware does
not pin (MCP servers added at run time).

    python scripts/ca_bundle.py            # downloads curl.se/ca/cacert.pem
    python scripts/ca_bundle.py cacert.pem # from a local file

Format (esp_crt_bundle.c): 2 bytes number of certificates, then per certificate, sorted by the
DER bytes of its subject: 2 bytes subject length, 2 bytes key length, the subject (DER Name),
the public key (DER SubjectPublicKeyInfo). Big endian. Needs no packages beyond the standard
library: the two fields are cut out of the DER certificate directly.
"""

import base64
import re
import struct
import sys
import urllib.request
from pathlib import Path

SOURCE = "https://curl.se/ca/cacert.pem"
OUTPUT = Path(__file__).resolve().parent.parent / "data" / "cert" / "x509_crt_bundle.bin"


def element(der: bytes, offset: int) -> tuple[int, int, int]:
    """Tag, start of the content and end of one DER element at `offset`."""
    tag = der[offset]
    length = der[offset + 1]
    start = offset + 2
    if length & 0x80:
        count = length & 0x7F
        length = int.from_bytes(der[start:start + count], "big")
        start += count
    return tag, start, start + length


def subject_and_key(der: bytes) -> tuple[bytes, bytes]:
    _, cert_start, _ = element(der, 0)             # Certificate
    _, position, _ = element(der, cert_start)      # tbsCertificate: walk its fields
    fields = []
    while len(fields) < 7:
        tag, _, end = element(der, position)
        if not fields and tag == 0xA0:             # [0] version, optional
            position = end
            continue
        fields.append((position, end))
        position = end
    # serialNumber, signature, issuer, validity, subject, subjectPublicKeyInfo
    subject = der[fields[4][0]:fields[4][1]]
    key = der[fields[5][0]:fields[5][1]]
    return subject, key


def main() -> None:
    if len(sys.argv) > 1:
        pem = Path(sys.argv[1]).read_text(encoding="utf-8")
    else:
        with urllib.request.urlopen(SOURCE, timeout=30) as response:
            pem = response.read().decode("utf-8")
    blocks = re.findall(r"-----BEGIN CERTIFICATE-----(.+?)-----END CERTIFICATE-----", pem, re.S)
    entries = {}
    for block in blocks:
        subject, key = subject_and_key(base64.b64decode("".join(block.split())))
        entries.setdefault(subject, key)  # the first of the same name, like gen_crt_bundle.py
    bundle = bytearray(struct.pack(">H", len(entries)))
    for subject in sorted(entries):
        key = entries[subject]
        bundle += struct.pack(">HH", len(subject), len(key)) + subject + key
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    OUTPUT.write_bytes(bundle)
    print(f"{len(entries)} certificates, {len(bundle)} bytes -> {OUTPUT}")


if __name__ == "__main__":
    main()
