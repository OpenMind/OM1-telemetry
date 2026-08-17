#!/usr/bin/env python3
"""
align_recording.py — align all streams of a recording to a common timeline.

Strategy: for each "master" timestamp (default: lidar @ 10 Hz),
find the nearest sample in every other stream.  This yields a single
table where each row is a synchronized multimodal observation.

Output: a pandas DataFrame (saved as parquet for fast loading).

Usage:
    python3 align_recording.py /path/to/session
    python3 align_recording.py /path/to/session --master lowstate --rate 20
"""

import argparse
from pathlib import Path
import numpy as np
import pandas as pd

import timebase


# Per-stream timestamp column names + bin file (None = no bin, just timestamps)
STREAMS = {
    "lowstate":   ("lowstate_timestamps.csv",   "lowstate_frames.bin",   "unix_ns"),
    "lidar":      ("lidar_timestamps.csv",      "lidar_scans.bin",       "unix_ns"),
    "depth":      ("depth_timestamps.csv",      "depth_frames.bin",      "unix_ns"),
    "odom":       ("odom_timestamps.csv",       "odom_frames.bin",       "unix_ns"),
    "audio":      ("audio_packets.csv",         None,                    "unix_ns"),
    # Capture-time-stamped, video-derived feature events from the
    # video-processor (VVAD/speaking, etc.). unix_ns is true capture time, so
    # this aligns precisely with the source-stamped DDS streams — unlike the
    # camera streams, which are anchored to record-start wall clock.
    "video_features": ("video_features_timestamps.csv", None,            "unix_ns"),
    # G1 records the video-processor's two pre-CV streams. Go2 records
    # top/front/down. Both sets are listed; a session simply lacks the files
    # for the cameras its robot does not have.
    "front_cam_raw": ("front_camera_raw_frames.csv", None,               "wallclock_unix_ns"),
    "rear_cam_raw":  ("rear_camera_raw_frames.csv",  None,               "wallclock_unix_ns"),
    "top_cam":    ("top_camera_frames.csv",     None,                    "wallclock_unix_ns"),
    "front_cam":  ("front_camera_frames.csv",   None,                    "wallclock_unix_ns"),
    "down_cam":   ("down_camera_frames.csv",    None,                    "wallclock_unix_ns"),
}


def load_stream(session: Path, csv_name: str, time_col: str, tb=None):
    """Load a timestamp CSV.  Returns (timestamps_ns, row_indices, df).

    When the session was recorded across an NTP correction (a robot that booted
    offline), the wall-clock column is shifted back to true UTC using the clock
    journal. The file on disk is not modified.
    """
    path = session / csv_name
    if not path.exists():
        return None, None, None
    df = pd.read_csv(path)
    if tb is not None and tb.needs_correction:
        df, _ = timebase.correct_dataframe(df, tb)
    if time_col not in df.columns:
        # fall through to alternative column names
        for alt in ("wallclock_unix_ns", "unix_ns"):
            if alt in df.columns:
                time_col = alt
                break
        else:
            return None, None, None
    ts = df[time_col].astype(np.int64).values
    idx = np.arange(len(ts))
    return ts, idx, df


def align_to_master(master_ts: np.ndarray, slave_ts: np.ndarray,
                    slave_idx: np.ndarray, max_dt_ms: float = 100):
    """For each master timestamp, return the slave index of the nearest
    sample, or -1 if no slave sample within max_dt_ms.
    """
    # searchsorted gives the insertion point; we check both neighbors.
    pos = np.searchsorted(slave_ts, master_ts)
    pos_left = np.clip(pos - 1, 0, len(slave_ts) - 1)
    pos_right = np.clip(pos, 0, len(slave_ts) - 1)

    d_left = np.abs(master_ts - slave_ts[pos_left])
    d_right = np.abs(master_ts - slave_ts[pos_right])

    pick_right = d_right < d_left
    best_pos = np.where(pick_right, pos_right, pos_left)
    best_dt = np.where(pick_right, d_right, d_left)

    # Mask out samples beyond max_dt
    too_far = best_dt > int(max_dt_ms * 1e6)  # ms → ns
    aligned_idx = np.where(too_far, -1, slave_idx[best_pos])
    aligned_dt_ms = best_dt / 1e6
    aligned_dt_ms[too_far] = np.nan

    return aligned_idx, aligned_dt_ms


def main():
    p = argparse.ArgumentParser()
    p.add_argument("session", help="Path to a recording session directory")
    p.add_argument("--master", default="lidar",
                   help="Stream to use as the master timeline (default: lidar)")
    p.add_argument("--max-dt-ms", type=float, default=100,
                   help="Max alignment distance to accept; samples further than "
                        "this from master become -1/NaN (default: 100ms)")
    p.add_argument("--out", default="aligned.parquet",
                   help="Output filename (parquet) relative to session dir")
    p.add_argument("--out-csv", default=None,
                   help="Also write a CSV (slower to load but human-readable)")
    args = p.parse_args()

    session = Path(args.session)
    if not session.exists():
        print(f"❌ session not found: {session}")
        return 1

    # ── Load master ──────────────────────────────────────────────────────
    if args.master not in STREAMS:
        print(f"❌ unknown master '{args.master}'. choices: {list(STREAMS)}")
        return 1
    master_csv, _, master_time_col = STREAMS[args.master]
    tb = timebase.load(session)
    if tb.needs_correction:
        print(f"clock: {timebase.describe(tb)}")
        print("       timestamps below are corrected; the CSVs on disk are unchanged")

    master_ts, master_idx, master_df = load_stream(session, master_csv, master_time_col, tb)
    if master_ts is None:
        print(f"❌ master stream '{args.master}' not found in session")
        return 1
    print(f"Master: {args.master}  ({len(master_ts)} samples)")

    # Build the master table: timestamp + frame index in master file
    aligned = pd.DataFrame({
        "unix_ns": master_ts,
        f"{args.master}_idx": master_idx,
    })

    # ── For each other stream, find nearest neighbor ─────────────────────
    for name, (csv_name, _, time_col) in STREAMS.items():
        if name == args.master:
            continue
        slave_ts, slave_idx, slave_df = load_stream(session, csv_name, time_col, tb)
        if slave_ts is None:
            print(f"  - {name}: not found, skipping")
            continue

        aligned_idx, aligned_dt = align_to_master(
            master_ts, slave_ts, slave_idx, args.max_dt_ms
        )

        aligned[f"{name}_idx"] = aligned_idx
        aligned[f"{name}_dt_ms"] = aligned_dt

        n_matched = (aligned_idx >= 0).sum()
        coverage = 100 * n_matched / len(master_ts)
        median_dt = np.nanmedian(aligned_dt)
        print(f"  + {name}: {n_matched}/{len(master_ts)} matched "
              f"({coverage:.1f}% coverage, median Δt={median_dt:.1f}ms)")

    # ── Save ─────────────────────────────────────────────────────────────
    out_path = session / args.out
    aligned.to_parquet(out_path, index=False)
    print(f"\n✅ aligned table saved to: {out_path}")
    print(f"   shape: {aligned.shape}")

    if args.out_csv:
        csv_path = session / args.out_csv
        aligned.to_csv(csv_path, index=False)
        print(f"✅ also saved CSV to: {csv_path}")

    print("\nFirst 3 rows:")
    print(aligned.head(3).to_string())

    print("\nUsage example for training data loader:")
    print("""
    import pandas as pd
    aligned = pd.read_parquet("aligned.parquet")

    for _, row in aligned.iterrows():
        t_ns = row["unix_ns"]
        lidar_frame_idx = int(row["lidar_idx"])     # which frame in lidar_scans.bin
        lowstate_idx = int(row["lowstate_idx"])     # which row in lowstate_frames.bin
        depth_idx = int(row["depth_idx"])
        top_cam_idx = int(row["top_cam_idx"])       # which frame in top_camera.mp4
        # ... use these indices to fetch the actual sensor data
    """)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
