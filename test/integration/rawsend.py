"""Send one UDP datagram as a raw Ethernet frame, past the routing table.

Usage: rawsend.py IFACE SRC_IP GATEWAY_MAC DST_IP DST_PORT PAYLOAD [SRC_PORT]

The frame goes to the gateway's MAC address, whatever routes the container
has: this is how a process with CAP_NET_RAW, which Docker grants by default,
gets past the unreachable routes for the tailnet ranges. SRC_IP and DST_IP
are both IPv4 or both IPv6 addresses; an IPv6 frame needs no IPv6 in the
container. SRC_PORT defaults to 40000. Standard library only, so the test
needs no packages.
"""

import socket
import struct
import sys


def checksum(data: bytes) -> int:
    """Return the Internet checksum of even-length data."""
    total = sum(struct.unpack(f"!{len(data) // 2}H", data))
    total = (total >> 16) + (total & 0xFFFF)
    total += total >> 16
    return ~total & 0xFFFF


def ipv6_packet(src: str, dst: str, port: int, data: bytes, sport: int) -> bytes:
    """Return an IPv6 packet holding a UDP datagram, with the checksum IPv6 needs."""
    s = socket.inet_pton(socket.AF_INET6, src)
    d = socket.inet_pton(socket.AF_INET6, dst)
    length = 8 + len(data)
    udp = struct.pack("!HHHH", sport, port, length, 0) + data
    pseudo = s + d + struct.pack("!IxxxB", length, socket.IPPROTO_UDP) + udp
    total = checksum(pseudo + b"\x00" * (len(pseudo) % 2)) or 0xFFFF
    udp = udp[:6] + struct.pack("!H", total) + udp[8:]
    return struct.pack("!IHBB", 6 << 28, length, socket.IPPROTO_UDP, 64) + s + d + udp


def ipv4_packet(src: str, dst: str, port: int, data: bytes, sport: int) -> bytes:
    """Return an IPv4 packet holding a UDP datagram."""
    # UDP checksum 0: none, which IPv4 allows
    udp = struct.pack("!HHHH", sport, port, 8 + len(data), 0) + data
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
    return header[:10] + struct.pack("!H", checksum(header)) + header[12:] + udp


def main() -> None:
    """Send the datagram the arguments describe."""
    iface, src, gw_mac, dst, port, payload = sys.argv[1:7]
    sport = int(sys.argv[7]) if len(sys.argv) > 7 else 40000
    with open(f"/sys/class/net/{iface}/address", encoding="ascii") as f:
        src_mac = bytes.fromhex(f.read().strip().replace(":", ""))
    gw = bytes.fromhex(gw_mac.replace(":", ""))
    data = payload.encode()
    if ":" in dst:
        ethertype, packet = b"\x86\xdd", ipv6_packet(src, dst, int(port), data, sport)
    else:
        ethertype, packet = b"\x08\x00", ipv4_packet(src, dst, int(port), data, sport)
    frame = gw + src_mac + ethertype + packet
    with socket.socket(socket.AF_PACKET, socket.SOCK_RAW) as s:
        s.bind((iface, 0))
        s.send(frame)


if __name__ == "__main__":
    main()
