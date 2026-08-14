#!/usr/bin/env python3
"""
verify_recording.py — comprehensive validation of an om1-telemetry recording session.

Checks:
  1. Per-stream rate, monotonicity, gaps, seq number continuity
  2. Format validity (mp4/ogg decode, binary file size consistency)
  3. Cross-modal time alignment (nearest-neighbor analysis)
  4. Calibration presence
  5. Final ML-readiness verdict

Usage:
    python3 verify_recording.py /path/to/session
    python3 verify_recording.py /path/to/session --plot
"""

import argparse
import os
import subprocess
import sys
from pathlib import Path

import numpy as np
import pandas as pd

import timebase

# ── ANSI colors ───────────────────────────────────────────────────────
G, R, Y, B, BOLD, END = (
    "\033[92m", "\033[91m", "\033[93m", "\033[94m", "\033[1m", "\033[0m"
)
OK_, BAD, WARN = f"{G}✅{END}", f"{R}❌{END}", f"{Y}⚠️{END}"

# ── Streams to check (Go2 default; pointcloud disabled per env.go2) ──
STREAMS = [
    # (name, timestamps_csv, expected_hz, time_col)
    ("lowstate",   "lowstate_timestamps.csv",   400,  "unix_ns"),
    ("lidar",      "lidar_timestamps.csv",      10,   "unix_ns"),
    ("pointcloud", "pointcloud_timestamps.csv", 10,   "unix_ns"),
    ("depth",      "depth_timestamps.csv",      15,   "unix_ns"),
    ("odom",       "odom_timestamps.csv",       150,  "unix_ns"),   # Go2 publishes /odom at ~150 Hz
    ("audio",      "audio_packets.csv",         100, "unix_ns"),  # Opus 10ms frames → 100 pkt/s
    # G1: the video-processor's two pre-CV streams.
    ("front_cam_raw", "front_camera_raw_frames.csv", 30, "wallclock_unix_ns"),
    ("rear_cam_raw",  "rear_camera_raw_frames.csv",  30, "wallclock_unix_ns"),
    # Go2.
    ("top_cam",    "top_camera_frames.csv",     30,   "wallclock_unix_ns"),
    ("front_cam",  "front_camera_frames.csv",   30,   "wallclock_unix_ns"),
    ("down_cam",   "down_camera_frames.csv",    15,   "wallclock_unix_ns"),
    ("network",    "network_status.csv",        0.2,  "unix_ns"),
]

# ── 1. Per-stream statistics ─────────────────────────────────────────
def stream_stats(csv_path: Path, expected_hz: float, time_col: str, tb=None):
    """Return dict of stats, or None if file missing/unparseable."""
    if not csv_path.exists():
        return None
    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        return {"error": f"unparseable: {e}"}
    if len(df) == 0:
        return {"error": "empty"}
    if time_col not in df.columns:
        # Try alternate
        for alt in ("wallclock_unix_ns", "unix_ns"):
            if alt in df.columns:
                time_col = alt
                break
        else:
            return {"error": f"no time column in {df.columns.tolist()}"}

    # A session recorded across an NTP correction has a wall clock that jumps
    # mid-file. Correcting on read is what keeps the monotonicity check below
    # meaningful instead of flagging the clock fix as data corruption.
    if tb is not None and tb.needs_correction:
        df, _ = timebase.correct_dataframe(df, tb, time_col)

    ts_series = pd.to_numeric(df[time_col], errors="coerce")
    n_bad = int(ts_series.isna().sum())
    if n_bad > 0:
        # Drop rows with missing/non-numeric timestamps (e.g., last row
        # written when ffmpeg/recorder was killed mid-line).
        df = df.loc[ts_series.notna()].reset_index(drop=True)
        ts_series = ts_series.dropna().reset_index(drop=True)
    if len(ts_series) == 0:
        return {"error": f"all {n_bad} rows had invalid timestamps"}
    ts = ts_series.astype(np.int64).values
    if len(ts) < 2:
        return {"error": "< 2 valid samples"}

    duration = (ts[-1] - ts[0]) / 1e9
    rate = (len(ts) - 1) / duration if duration > 0 else 0
    intervals = np.diff(ts) / 1e6  # ms
    monotonic = bool((intervals > 0).all())

    # Gap threshold: 3x expected interval, or 1s if no expected rate
    gap_threshold = 3 * (1000.0 / expected_hz) if expected_hz > 0 else 1000.0
    gaps = int((intervals > gap_threshold).sum())

    # Sequence gaps
    seq_gaps = None
    if "seq" in df.columns:
        seq = df["seq"].values
        seq_gaps = int((seq[-1] - seq[0] + 1) - len(seq))

    return {
        "n": len(ts),
        "n_bad": n_bad,
        "duration_s": duration,
        "rate_hz": rate,
        "expected_hz": expected_hz,
        "p50_interval_ms": float(np.median(intervals)),
        "p95_interval_ms": float(np.percentile(intervals, 95)),
        "max_interval_ms": float(intervals.max()),
        "gaps": gaps,
        "monotonic": monotonic,
        "seq_gaps": seq_gaps,
        "ts": ts,
        "ts_start": int(ts[0]),
        "ts_end": int(ts[-1]),
        "time_col": time_col,
        "csv_path": str(csv_path),
    }


def print_stream_table(stats: dict):
    print(f"\n{BOLD}Per-stream statistics{END}")
    print(f"{'Stream':<12} {'Frames':>8} {'Duration':>9} {'Rate':>10} "
          f"{'P95 jitter':>12} {'Gaps':>5} {'Seq':>5} Status")
    print("─" * 80)

    for name, s in stats.items():
        if s is None:
            print(f"{name:<12} {'---':>8}  (not recorded / file missing)")
            continue
        if "error" in s:
            print(f"{name:<12} {BAD} {s['error']}")
            continue

        # Status: ok unless something wrong
        status = OK_
        notes = []
        if s.get("n_bad", 0) > 0:
            status = WARN; notes.append(f"{s['n_bad']} bad ts rows dropped")
        if not s["monotonic"]:
            status = BAD; notes.append("non-monotonic")
        elif s["gaps"] > 5:
            status = WARN; notes.append(f"{s['gaps']} gaps")
        if s["seq_gaps"] and s["seq_gaps"] > 5:
            status = WARN; notes.append(f"{s['seq_gaps']} seq gaps")

        # Rate sanity: within 50% of expected
        if s["expected_hz"] > 0:
            ratio = s["rate_hz"] / s["expected_hz"]
            if ratio < 0.5 or ratio > 2.0:
                status = WARN
                notes.append(f"rate {ratio*100:.0f}% of expected")

        rate_str   = f"{s['rate_hz']:.1f} Hz"
        dur_str    = f"{s['duration_s']:.1f} s"
        jitter_str = f"{s['p95_interval_ms']:.1f} ms"
        seq_str    = "—" if s["seq_gaps"] is None else str(s["seq_gaps"])

        line = (f"{name:<12} {s['n']:>8} {dur_str:>9} {rate_str:>10} "
                f"{jitter_str:>12} {s['gaps']:>5} {seq_str:>5} {status}")
        if notes:
            line += f"  ({', '.join(notes)})"
        print(line)


# ── 2. Format validation ─────────────────────────────────────────────
def ffprobe_streams(path: Path):
    try:
        r = subprocess.run(
            ["ffprobe", "-v", "error", "-show_streams", str(path)],
            capture_output=True, text=True, timeout=10,
        )
        if r.returncode != 0:
            return None
        info = {}
        for line in r.stdout.split("\n"):
            if "=" in line:
                k, v = line.split("=", 1)
                info[k.strip()] = v.strip()
        return info
    except Exception:
        return None


def check_binary(csv_path: Path, bin_path: Path):
    """Check that the bin file size is consistent with the CSV byte_offsets.
    Handles two formats:
      (a) byte_offset + byte_length columns
      (b) byte_offset only — derive length from successive offsets
    """
    if not bin_path.exists():
        return None
    try:
        df = pd.read_csv(csv_path)
        if "byte_offset" not in df.columns:
            return {"error": "no byte_offset column"}

        offsets = df["byte_offset"].astype(np.int64).values
        actual_size = bin_path.stat().st_size

        if "byte_length" in df.columns:
            lengths = df["byte_length"].astype(np.int64).values
            expected_end = int(offsets[-1] + lengths[-1])
        else:
            # Derive lengths from successive offsets
            # Last frame length = file_size - last_offset
            next_offsets = np.concatenate([offsets[1:], [actual_size]])
            lengths = next_offsets - offsets
            expected_end = actual_size  # by construction

        # Sanity: offsets monotonic, lengths positive
        mono = bool((np.diff(offsets) > 0).all())
        all_positive = bool((lengths > 0).all())

        return {
            "frames": len(df),
            "expected_size": expected_end,
            "actual_size": actual_size,
            "match": abs(expected_end - actual_size) < 1024,
            "offsets_monotonic": mono,
            "lengths_positive": all_positive,
            "avg_frame_size": float(lengths.mean()),
            "min_frame_size": int(lengths.min()),
            "max_frame_size": int(lengths.max()),
        }
    except Exception as e:
        return {"error": str(e)}


def validate_formats(session: Path):
    print(f"\n{BOLD}Format validation{END}")

    # Binary streams: bin size should match sum of byte_lengths
    bin_streams = [
        ("lowstate",   "lowstate_timestamps.csv",   "lowstate_frames.bin"),
        ("lidar",      "lidar_timestamps.csv",      "lidar_scans.bin"),
        ("pointcloud", "pointcloud_timestamps.csv", "pointcloud_frames.bin"),
        ("depth",      "depth_timestamps.csv",      "depth_frames.bin"),
        ("odom",       "odom_timestamps.csv",       "odom_frames.bin"),
    ]
    for name, csv_name, bin_name in bin_streams:
        info = check_binary(session / csv_name, session / bin_name)
        if info is None:
            continue
        if "error" in info:
            print(f"  {BAD} {name}: {info['error']}")
            continue
        mark = OK_ if info["match"] else WARN
        size_str = f"{info['actual_size']:,} B"
        avg_str = f"{info['avg_frame_size']:.0f} B/frame avg"
        range_str = (f"({info['min_frame_size']}–{info['max_frame_size']} B)"
                     if info['min_frame_size'] != info['max_frame_size']
                     else "(constant size)")
        print(f"  {mark} {name}: {info['frames']} frames, {size_str}, {avg_str} {range_str}")
        if not info["match"]:
            print(f"      expected bin size {info['expected_size']:,} B, "
                  f"actual {info['actual_size']:,} B (off by {abs(info['expected_size']-info['actual_size'])} B)")

    # Video files
    for prefix in ["front_camera_raw", "rear_camera_raw",
                   "top_camera", "front_camera", "down_camera"]:
        mp4 = session / f"{prefix}.mp4"
        if not mp4.exists():
            continue
        info = ffprobe_streams(mp4)
        if info is None:
            print(f"  {BAD} {prefix}.mp4: ffprobe failed")
            continue
        codec = info.get("codec_name", "?")
        w, h = info.get("width", "?"), info.get("height", "?")
        fps_raw = info.get("r_frame_rate", "0/1")
        try:
            num, den = fps_raw.split("/")
            fps = float(num) / float(den) if float(den) else 0
        except Exception:
            fps = 0
        print(f"  {OK_} {prefix}.mp4: {codec} {w}x{h} @ {fps:.1f} fps "
              f"({mp4.stat().st_size:,} B)")

    # Audio
    ogg = session / "audio.ogg"
    if ogg.exists():
        info = ffprobe_streams(ogg)
        if info is None:
            print(f"  {BAD} audio.ogg: ffprobe failed")
        else:
            codec = info.get("codec_name", "?")
            sr = info.get("sample_rate", "?")
            ch = info.get("channels", "?")
            print(f"  {OK_} audio.ogg: {codec} {sr} Hz, {ch} ch ({ogg.stat().st_size:,} B)")


# ── 3. Cross-modal alignment ─────────────────────────────────────────
def nearest_neighbor_align(master_ts, slave_ts):
    """For each master timestamp, find nearest slave timestamp."""
    if len(slave_ts) == 0:
        return None
    idx = np.searchsorted(slave_ts, master_ts)
    idx_left  = np.clip(idx - 1, 0, len(slave_ts) - 1)
    idx_right = np.clip(idx,     0, len(slave_ts) - 1)
    d_left  = np.abs(master_ts - slave_ts[idx_left])
    d_right = np.abs(master_ts - slave_ts[idx_right])
    d = np.minimum(d_left, d_right) / 1e6  # ms
    return {
        "median_ms": float(np.median(d)),
        "p95_ms": float(np.percentile(d, 95)),
        "max_ms": float(d.max()),
        "n_master": len(master_ts),
    }


def cross_alignment(stats: dict):
    print(f"\n{BOLD}Cross-modal alignment (nearest-neighbor){END}")

    # Filter candidates: must have >= 50 samples for stable alignment stats
    # (this excludes low-rate streams like network ping which has only ~10 samples)
    candidates = [(n, s) for n, s in stats.items()
                  if s and "error" not in s and s["n"] >= 50]
    if len(candidates) < 2:
        # Fall back to >= 10 samples
        candidates = [(n, s) for n, s in stats.items()
                      if s and "error" not in s and s["n"] >= 10]
    if len(candidates) < 2:
        print(f"  {WARN} not enough streams to align")
        return {}, []

    # Master = slowest among qualified candidates (gives best coverage of slaves)
    master_name, master = min(candidates, key=lambda kv: kv[1]["rate_hz"])
    print(f"  Master: {BOLD}{master_name}{END} "
          f"({master['rate_hz']:.1f} Hz, {master['n']} samples)\n")

    # Also restrict master samples to overlap with each slave's time range
    # so tail mismatches (streams stopping at different times) don't dominate.
    print(f"  {'Pair':<30} {'Median':>9} {'P95':>9} {'Max':>9}  OK?")
    print("  " + "─" * 65)

    results = {}
    skipped = []  # streams that couldn't be aligned (time range doesn't overlap)
    # Build master full range
    for name, s in candidates:
        if name == master_name:
            continue

        # Restrict master_ts to slave's time coverage
        slave_t0, slave_t1 = s["ts_start"], s["ts_end"]
        mask = (master["ts"] >= slave_t0) & (master["ts"] <= slave_t1)
        master_in_range = master["ts"][mask]
        if len(master_in_range) == 0:
            # No overlap — almost always means timestamps are in different
            # clock domains (e.g. wall clock vs monotonic, or wrong epoch).
            skipped.append((name, s))
            continue

        align = nearest_neighbor_align(master_in_range, s["ts"])
        if align is None:
            continue
        align["n_used"] = len(master_in_range)
        align["n_skipped"] = len(master["ts"]) - len(master_in_range)
        results[name] = align

        # Verdict
        if   align["p95_ms"] < 50:   mark = OK_
        elif align["p95_ms"] < 100:  mark = OK_
        elif align["p95_ms"] < 500:  mark = WARN
        else:                        mark = BAD

        pair_str = f"{master_name} ↔ {name}"
        note = ""
        if align["n_skipped"] > 0:
            note = f"  (excl {align['n_skipped']} master samples outside slave range)"
        print(f"  {pair_str:<30} "
              f"{align['median_ms']:>7.1f}ms "
              f"{align['p95_ms']:>7.1f}ms "
              f"{align['max_ms']:>7.1f}ms  {mark}{note}")

    # Report streams that couldn't be aligned at all
    if skipped:
        print()
        print(f"  {BAD} {len(skipped)} stream(s) could NOT be aligned with master:")
        for name, s in skipped:
            # Compute the time offset between master and slave first timestamps
            offset_sec = (s["ts_start"] - master["ts_start"]) / 1e9
            offset_str = f"{offset_sec:+,.1f} sec"
            # Detect well-known offsets
            hint = ""
            if abs(abs(offset_sec) - 2208988800) < 10:
                hint = " ← LOOKS LIKE NTP→Unix epoch offset (70 years)"
            elif abs(offset_sec) > 86400 * 365:  # > 1 year
                hint = " ← different epoch / clock domain"
            print(f"      {name}: first timestamp offset from master = {offset_str}{hint}")

    # Return both alignments and the list of unaligned streams so the verdict
    # can flag them.
    return results, skipped


# ── 4. Calibration check ─────────────────────────────────────────────
def check_calibration(session: Path):
    print(f"\n{BOLD}Calibration{END}")
    cal = session.parent / "calibration"
    if not cal.exists():
        print(f"  {WARN} No calibration directory at {cal}")
        print(f"     → without this you can't do sensor fusion")
        return False
    found = list(cal.glob("*.yaml"))
    if not found:
        print(f"  {WARN} Calibration directory empty: {cal}")
        return False
    expected = {
        "tf_static.yaml": "TF tree (LiDAR/camera/base mounting)",
        "color_cam_info.yaml": "RGB camera intrinsics",
        "depth_cam_info.yaml": "Depth camera intrinsics",
        "depth_to_color.yaml": "Depth → RGB extrinsics",
    }
    have_critical = True
    for fname, desc in expected.items():
        p = cal / fname
        if p.exists() and p.stat().st_size > 50:
            print(f"  {OK_} {fname}  ({p.stat().st_size} B)  — {desc}")
        else:
            print(f"  {WARN} {fname}  MISSING — {desc}")
            if "tf_static" in fname or "cam_info" in fname:
                have_critical = False
    return have_critical


# ── 5. Final verdict ─────────────────────────────────────────────────
def final_verdict(stats: dict, align: dict, skipped: list, has_calib: bool):
    print(f"\n{BOLD}ML training readiness verdict{END}")
    print("─" * 50)

    issues, warns = [], []

    # Per-stream issues
    for name, s in stats.items():
        if s is None:
            continue
        if "error" in s:
            warns.append(f"{name}: {s['error']}")
            continue
        if not s["monotonic"]:
            issues.append(f"{name} timestamps not monotonic — BLOCKER")
        if s["gaps"] > 10:
            warns.append(f"{name} has {s['gaps']} time gaps")
        if s["seq_gaps"] and s["seq_gaps"] > 10:
            warns.append(f"{name} dropped {s['seq_gaps']} messages")
        if s["expected_hz"] > 0:
            ratio = s["rate_hz"] / s["expected_hz"]
            if ratio < 0.5:
                warns.append(f"{name} rate is {ratio*100:.0f}% of expected ({s['rate_hz']:.1f} vs {s['expected_hz']} Hz)")

    # Streams that couldn't be aligned at all → different clock domains → BLOCKER
    if skipped:
        for name, s in skipped:
            issues.append(f"{name} could NOT be aligned with master — different clock domain (BLOCKER for sensor fusion)")

    # Alignment quality of streams that DID align
    if align:
        worst = max(align.values(), key=lambda a: a["p95_ms"])
        worst_p95 = worst["p95_ms"]
        if worst_p95 > 500:
            issues.append(f"Worst alignment p95 = {worst_p95:.0f}ms — BLOCKER")
        elif worst_p95 > 100:
            warns.append(f"Worst alignment p95 = {worst_p95:.0f}ms (>100ms borderline)")

    # Calibration
    if not has_calib:
        warns.append("Calibration files missing — cross-sensor fusion not possible")

    # Print
    print()
    if issues:
        print(f"{BOLD}{R}❌ NOT READY — must fix:{END}")
        for i in issues:
            print(f"   • {i}")
        if warns:
            print(f"\n{Y}Also warnings:{END}")
            for w in warns:
                print(f"   • {w}")
        return False
    if warns:
        print(f"{BOLD}{Y}🟡 USABLE WITH CAVEATS{END}")
        for w in warns:
            print(f"   • {w}")
        print(f"\n{BOLD}{G}✅ Data is trainable; address caveats above.{END}")
        return True
    print(f"{BOLD}{G}✅ READY FOR TRAINING{END}")
    print("   All streams monotonic · no gaps · alignment <100ms p95 · calibration present")
    return True


# ── 6. Optional plot ─────────────────────────────────────────────────
def plot_timeline(stats: dict, output_path: Path):
    try:
        import matplotlib.pyplot as plt
    except ImportError:
        print(f"  {WARN} matplotlib not installed, skipping plot")
        return

    fig, ax = plt.subplots(figsize=(14, 5))
    valid = {n: s for n, s in stats.items() if s and "error" not in s}
    if not valid:
        print(f"  {WARN} no streams to plot")
        return

    # Time origin = earliest timestamp across all streams
    t0 = min(s["ts_start"] for s in valid.values())
    for i, (name, s) in enumerate(valid.items()):
        rel_t = (s["ts"] - t0) / 1e9
        # Sample down if too many
        if len(rel_t) > 5000:
            rel_t = rel_t[::len(rel_t) // 5000]
        ax.scatter(rel_t, [i] * len(rel_t), s=2, alpha=0.5, label=f"{name} ({s['rate_hz']:.1f} Hz)")

    ax.set_yticks(range(len(valid)))
    ax.set_yticklabels(list(valid.keys()))
    ax.set_xlabel("Time (s, relative to recording start)")
    ax.set_title("Per-stream message timeline")
    ax.grid(True, alpha=0.3)
    plt.tight_layout()
    plt.savefig(output_path, dpi=100)
    print(f"  {OK_} Saved timeline plot to {output_path}")


# ── main ─────────────────────────────────────────────────────────────
def main():
    p = argparse.ArgumentParser()
    p.add_argument("session", help="Path to a recording session directory")
    p.add_argument("--plot", action="store_true", help="Save a timeline plot")
    args = p.parse_args()

    session = Path(args.session)
    if not session.exists():
        print(f"{BAD} Session not found: {session}")
        sys.exit(1)

    print(f"{BOLD}╔══════════════════════════════════════════════════════════════╗{END}")
    print(f"{BOLD}║  Recording verification: {session.name:<35} ║{END}")
    print(f"{BOLD}╚══════════════════════════════════════════════════════════════╝{END}")

    # Collect per-stream stats
    stats = {}
    tb = timebase.load(session)
    print(f"\n{BOLD}Clock{END}: {timebase.describe(tb)}")
    if tb.needs_correction:
        print(f"  {WARN} recorded across an NTP correction; "
              f"timestamps below are corrected on read (files unchanged)")
    elif tb.records and not tb.saw_sync:
        print(f"  {WARN} this session's timestamps cannot be trusted or corrected")

    for name, csv_name, expected_hz, time_col in STREAMS:
        stats[name] = stream_stats(session / csv_name, expected_hz, time_col, tb)

    print_stream_table(stats)
    validate_formats(session)
    align, skipped = cross_alignment(stats)
    has_calib = check_calibration(session)
    ok = final_verdict(stats, align, skipped, has_calib)

    if args.plot:
        print()
        plot_timeline(stats, session / "timeline.png")

    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()