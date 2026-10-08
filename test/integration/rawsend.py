"""Send one UDP datagram as a raw Ethernet frame, past the routing table.

Usage: rawsend.py IFACE SRC_IP GATEWAY_MAC DST_IP DST_PORT PAYLOAD

The frame goes to the gateway's MAC address, whatever routes the container
has: this is how a process with CAP_NET_RAW, which Docker grants by default,
gets past the unreachable routes for the tailnet ranges. Standard library
only, so the test needs no packages.
"""

import socket
import struct
import sys


def checksum(data: bytes) -> int:
    """Return the Internet checksum of an even-length header."""
    total = sum(struct.unpack(f"!{len(data) // 2}H", data))
    total = (total >> 16) + (total & 0xFFFF)
    total += total >> 16
    return ~total & 0xFFFF


def main() -> None:
    """Send the datagram the arguments describe."""
    iface, src, gw_mac, dst, port, payload = sys.argv[1:7]
    with open(f"/sys/class/net/{iface}/address", encoding="ascii") as f:
        src_mac = bytes.fromhex(f.read().strip().replace(":", ""))
    data = payload.encode()
    # UDP checksum 0: none, which IPv4 allows
    udp = struct.pack("!HHHH", 40000, int(port), 8 + len(data), 0) + data
    header = struct.pack(
        "!BBHHHBBH4s4s",
        0x45,
        0,
        20 + len(udp),
        0,
        0,
        64,
        socket.IPPROTO_UDP,
        0,
        socket.inet_aton(src),
        socket.inet_aton(dst),
    )
    header = header[:10] + struct.pack("!H", checksum(header)) + header[12:]
    gw = bytes.fromhex(gw_mac.replace(":", ""))
    frame = gw + src_mac + b"\x08\x00" + header + udp
    with socket.socket(socket.AF_PACKET, socket.SOCK_RAW) as s:
        s.bind((iface, 0))
        s.send(frame)


if __name__ == "__main__":
    main()
