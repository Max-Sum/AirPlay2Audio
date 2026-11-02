"""Generate a small self-contained 44.1 kHz stereo AAC test fixture with FDK.

Run from the project directory with the shipped FDK DLL. The output contains
big-endian 16-bit lengths followed by raw AAC access units.
"""
import ctypes as c
import math
import pathlib
import struct


class BufferDescriptor(c.Structure):
    _fields_ = [("count", c.c_int), ("buffers", c.POINTER(c.c_void_p)),
                ("identifiers", c.POINTER(c.c_int)), ("sizes", c.POINTER(c.c_int)),
                ("element_sizes", c.POINTER(c.c_int))]


class InArgs(c.Structure):
    _fields_ = [("samples", c.c_int), ("ancillary", c.c_int)]


class OutArgs(c.Structure):
    _fields_ = [("bytes", c.c_int), ("samples", c.c_int), ("ancillary", c.c_int), ("reservoir", c.c_int)]


def descriptor(array, identifier, element_size):
    buffers = (c.c_void_p * 1)(c.addressof(array))
    ids, sizes, widths = (c.c_int * 1)(identifier), (c.c_int * 1)(c.sizeof(array)), (c.c_int * 1)(element_size)
    value = BufferDescriptor(1, buffers, ids, sizes, widths)
    return value, (buffers, ids, sizes, widths)


def main():
    root = pathlib.Path(__file__).resolve().parent.parent
    library = c.CDLL(str(root / "libfdk-aac-2.dll"))
    library.aacEncOpen.argtypes = [c.POINTER(c.c_void_p), c.c_uint, c.c_uint]
    library.aacEncoder_SetParam.argtypes = [c.c_void_p, c.c_int, c.c_uint]
    library.aacEncEncode.argtypes = [c.c_void_p, c.POINTER(BufferDescriptor), c.POINTER(BufferDescriptor), c.POINTER(InArgs), c.POINTER(OutArgs)]
    library.aacEncClose.argtypes = [c.POINTER(c.c_void_p)]
    handle = c.c_void_p()
    assert library.aacEncOpen(c.byref(handle), 0, 2) == 0
    try:
        for param, value in [(0x100, 2), (0x101, 128000), (0x103, 44100), (0x106, 2), (0x300, 0)]:
            assert library.aacEncoder_SetParam(handle, param, value) == 0
        assert library.aacEncEncode(handle, None, None, None, None) == 0
        packets = []
        for frame in range(16):
            pcm = (c.c_int16 * 2048)()
            for i in range(1024):
                sample = int(12000 * math.sin(2 * math.pi * 440 * (frame * 1024 + i) / 44100))
                pcm[2*i] = pcm[2*i+1] = sample
            encoded = (c.c_ubyte * 8192)()
            src, src_refs = descriptor(pcm, 0, 2)
            dst, dst_refs = descriptor(encoded, 3, 1)
            incoming, outgoing = InArgs(2048, 0), OutArgs()
            assert library.aacEncEncode(handle, c.byref(src), c.byref(dst), c.byref(incoming), c.byref(outgoing)) == 0
            if outgoing.bytes:
                packets.append(bytes(encoded[:outgoing.bytes]))
        assert len(packets) >= 10
        path = root / "audio" / "testdata" / "sine-aac-lc.bin"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b"".join(struct.pack(">H", len(p)) + p for p in packets))
        print(f"Wrote {len(packets)} AAC access units to {path}")
    finally:
        library.aacEncClose(c.byref(handle))


if __name__ == "__main__":
    main()
