from __future__ import annotations

import argparse
import sys
from pathlib import Path

import pandas as pd

import timebase

G, R, Y, B, BOLD, END = (
    "\033[92m", "\033[91m", "\033[93m", "\033[94m", "\033[1m", "\033[0m"
)
OK_, BAD, WARN, INFO = f"{G}✅{END}", f"{R}❌{END}", f"{Y}⚠️{END}", f"{B}•{END}"


def timestamp_csvs(session: Path) -> list[Path]:
    """Every CSV in a session that carries a wall-clock timestamp."""
    return sorted(
        p for p in session.glob("*.csv")
        # A corrected copy is an output of this script, not an input.
        if not p.name.endswith(".corrected.csv")
    )


def process_session(session: Path, mode: str) -> bool:
    """Report (and optionally apply) corrections. True if anything changed."""
    tb = timebase.load(session)

    print(f"\n{BOLD}{session}{END}")
    print(f"  clock: {timebase.describe(tb)}")

    if not tb.correctable:
        if tb.records:
            print(f"  {WARN} nothing to do: without a synchronized anchor there is "
                  f"no true time to correct toward")
        return False

    if not tb.needs_correction:
        print(f"  {OK_} timestamps as recorded are already correct")
        return False

    for step in tb.steps:
        before = pd.to_datetime(step.utc_before_ns, unit="ns", utc=True)
        after = pd.to_datetime(step.utc_after_ns, unit="ns", utc=True)
        print(f"  {INFO} step {step.step_ns / 1e9:+.3f} s: "
              f"{before:%Y-%m-%d %H:%M:%S} → {after:%Y-%m-%d %H:%M:%S} UTC")

    touched = False
    for path in timestamp_csvs(session):
        try:
            df = pd.read_csv(path)
        except Exception as exc:  # noqa: BLE001 - a bad CSV should not stop the sweep
            print(f"  {BAD} {path.name}: unreadable ({exc})")
            continue

        col = timebase.time_column(df.columns)
        if col is None:
            continue
        if timebase.MONO_COLUMN not in df.columns:
            print(f"  {WARN} {path.name}: no {timebase.MONO_COLUMN} column; "
                  f"recorded before clock tracking, skipped")
            continue

        corrected, changed = timebase.correct_dataframe(df, tb, col)
        if not changed:
            print(f"  {OK_} {path.name}: no rows need correcting")
            continue

        touched = True
        if mode == "check":
            print(f"  {INFO} {path.name}: {changed}/{len(df)} rows would shift")
            continue

        out = path if mode == "in-place" else path.with_suffix(".corrected.csv")
        corrected.to_csv(out, index=False)
        print(f"  {OK_} {path.name}: {changed}/{len(df)} rows corrected → {out.name}")

    if mode == "check" and touched:
        print(f"  {INFO} re-run with --write or --in-place to apply")
    return touched


def find_sessions(root: Path) -> list[Path]:
    """Every session directory under a recordings root, pending ones included."""
    seen = {p.parent for p in root.rglob(timebase.TIMEBASE_FILE)}
    # Sessions predating the clock journal still have a meta.json.
    seen |= {p.parent for p in root.rglob("meta.json")}
    return sorted(seen)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("path", type=Path,
                    help="a session directory, or a recordings root with --all")
    ap.add_argument("--all", action="store_true",
                    help="treat PATH as a recordings root and sweep every session under it")
    group = ap.add_mutually_exclusive_group()
    group.add_argument("--write", action="store_true",
                       help="write <name>.corrected.csv next to each original")
    group.add_argument("--in-place", action="store_true",
                       help="rewrite the CSVs, keeping originals in a <column>_recorded column")
    args = ap.parse_args()

    if not args.path.exists():
        print(f"{BAD} no such path: {args.path}", file=sys.stderr)
        return 2

    mode = "in-place" if args.in_place else "write" if args.write else "check"
    sessions = find_sessions(args.path) if args.all else [args.path]
    if not sessions:
        print(f"{WARN} no sessions found under {args.path}")
        return 0

    changed = sum(process_session(s, mode) for s in sessions)

    print(f"\n{BOLD}{len(sessions)} session(s) examined, {changed} needing correction{END}")
    if mode == "check" and changed:
        print("nothing was written (checking is the default)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
