"""Generate the AAC-LC test fixtures in audio/testdata with ffmpeg.

Each fixture is a 440 Hz sine in the front left and right channels; every
other channel is silent. The output contains big-endian 16-bit lengths
followed by raw AAC access units, as AirPlay sends them. The script checks
that each stream's AudioSpecificConfig matches the one the receiver uses
(aacConfig in audio/buffered.go).

Requires ffmpeg on PATH. Run from any directory:

    python tools/generate_aac_fixtures.py
"""
import pathlib
import struct
import subprocess

FRAMES = 16
SINE = "0.37*sin(440*2*PI*t)"

# name, sample rate, ffmpeg channel layout, channel count, ADTS sampling
# frequency index, AAC channel configuration
FIXTURES = [
    ("sine-aac-lc.bin", 44100, "stereo", 2, 4, 2),
    ("sine-aac-lc-5.1.bin", 48000, "5.1", 6, 3, 6),
    ("sine-aac-lc-7.1.bin", 48000, "7.1", 8, 3, 7),
]


def encode(rate, layout, channels):
    exprs = "|".join([SINE, SINE] + ["0"] * (channels - 2))
    duration = FRAMES * 1024 / rate
    command = [
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
        "-f", "lavfi", "-i", f"aevalsrc=exprs={exprs}:s={rate}:c={layout}:d={duration}",
        "-c:a", "aac", "-profile:a", "aac_low", "-b:a", f"{64000 * channels}",
        "-f", "adts", "-",
    ]
    return subprocess.run(command, check=True, capture_output=True).stdout


def access_units(adts, frequency_index, channel_config):
    units = []
    while adts:
        if len(adts) < 7 or adts[0] != 0xFF or adts[1] & 0xF0 != 0xF0:
            raise ValueError("not an ADTS stream")
        profile = adts[2] >> 6
        index = (adts[2] >> 2) & 0x0F
        config = ((adts[2] & 0x01) << 2) | (adts[3] >> 6)
        if (profile, index, config) != (1, frequency_index, channel_config):
            raise ValueError(f"unexpected stream: profile {profile}, frequency index {index}, channel configuration {config}")
        header = 7 if adts[1] & 0x01 else 9  # protection_absent
        length = ((adts[3] & 0x03) << 11) | (adts[4] << 3) | (adts[5] >> 5)
        units.append(adts[header:length])
        adts = adts[length:]
    return units


def main():
    directory = pathlib.Path(__file__).resolve().parent.parent / "audio" / "testdata"
    directory.mkdir(parents=True, exist_ok=True)
    for name, rate, layout, channels, frequency_index, channel_config in FIXTURES:
        units = access_units(encode(rate, layout, channels), frequency_index, channel_config)
        if len(units) < 10:
            raise ValueError(f"{name}: ffmpeg produced {len(units)} access units")
        path = directory / name
        path.write_bytes(b"".join(struct.pack(">H", len(u)) + u for u in units))
        print(f"Wrote {len(units)} AAC access units to {path}")


if __name__ == "__main__":
    main()
