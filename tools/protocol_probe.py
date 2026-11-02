"""Probe an already running receiver without an Apple device.

This validates negotiation and malformed input handling; it is not an
end-to-end playback or pairing test. Run only against a receiver you control.
"""
import argparse
import plistlib
import socket


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=7000)
    args = parser.parse_args()
    with socket.create_connection((args.host, args.port), timeout=5) as conn:
        stream = conn.makefile("rb")
        sequence = 0

        def request(method, path, payload=None):
            nonlocal sequence
            sequence += 1
            body = b"" if payload is None else plistlib.dumps(payload, fmt=plistlib.FMT_BINARY)
            headers = [f"{method} {path} RTSP/1.0", f"CSeq: {sequence}", f"Content-Length: {len(body)}"]
            if payload is not None:
                headers.append("Content-Type: application/x-apple-binary-plist")
            conn.sendall(("\r\n".join(headers) + "\r\n\r\n").encode() + body)
            status = stream.readline().decode().strip()
            headers = {}
            while True:
                line = stream.readline()
                if line == b"\r\n":
                    break
                if not line:
                    raise RuntimeError("receiver closed RTSP connection")
                key, value = line.decode().split(":", 1)
                headers[key.lower()] = value.strip()
            body = stream.read(int(headers.get("content-length", "0")))
            return int(status.split()[1]), plistlib.loads(body) if body.startswith(b"bplist") else body

        status, info = request("GET", "/info")
        assert status == 200 and info["features"] & (1 << 40) and info["features"] & (1 << 41)
        assert info["volumeControlType"] == 3 and info["supportedFormats"]["audioStream"] == 0x40000
        status, setup = request("SETUP", "/probe", {"timingProtocol": "PTP"})
        assert status == 200 and setup["eventPort"] == 60003 and setup["timingPeerInfo"]["Addresses"]
        bad_stream = {"streams": [{"type": 97, "audioFormat": 0x40000, "shk": bytes(32)}]}
        status, _ = request("SETUP", "/probe", bad_stream)
        assert status == 400, f"unsupported stream accepted: {status}"
        realtime_stream = {"streams": [{"type": 96, "audioFormat": 0x40000, "ct": 2, "spf": 352, "shk": bytes(32)}]}
        status, setup = request("SETUP", "/probe", realtime_stream)
        assert status == 200 and setup["streams"][0]["dataPort"] > 0 and setup["streams"][0]["controlPort"] > 0
        status, _ = request("FLUSH", "/probe")
        assert status == 200, f"realtime FLUSH failed: {status}"
        good_stream = {"streams": [{"type": 103, "audioFormat": 0x400000, "ct": 3, "spf": 1024, "shk": bytes(32)}]}
        status, setup = request("SETUP", "/probe", good_stream)
        assert status == 200, f"buffered setup failed: {status}, {setup!r}"
        endpoint = setup["streams"][0]
        assert endpoint["type"] == 103 and endpoint["dataPort"] > 0
        with socket.create_connection((args.host, endpoint["dataPort"]), timeout=5) as audio:
            audio.sendall(b"\x00\x01")
            assert audio.recv(1) == b"", "short audio packet should close the audio connection"
        status, _ = request("TEARDOWN", "/probe", {})
        assert status == 200
        status, _ = request("GET", "/info")
        assert status == 200
    print("PASS: buffered/PTP features, setup, stream rejection, and malformed packet handling")


if __name__ == "__main__":
    main()
