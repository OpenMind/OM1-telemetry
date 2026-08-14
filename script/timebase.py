"""Read a session's clock journal and correct timestamps recorded before NTP.

A robot that boots without a network records with a wall clock that may be days
wrong. Every timestamps CSV carries a ``mono_ns`` column alongside its wall
clock value; ``mono_ns`` comes from CLOCK_BOOTTIME and does not jump when NTP
later steps the clock. ``clock_timebase.jsonl`` records each step. Together they
make the correction arithmetic rather than guesswork.

The correction is a per-row *offset*, not a replacement value::

    offset(mono) = sum of every step that happened after that row was written

which is exact because between steps the wall clock and the boot clock advance
at the same rate to within NTP's slew (parts per million -- microseconds over a
session). Applying an offset rather than recomputing the timestamp preserves
what each column meant: a Zenoh row's ``unix_ns`` is the *publisher's* stamp,
recorded on the same host clock, while ``mono_ns`` is when this recorder
received it. Both are shifted by the same amount, so the transport latency
between them survives the correction intact.

Used by fix_session_time.py, align_recording.py and verify_recording.py.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

TIMEBASE_FILE = "clock_timebase.jsonl"

# Wall-clock column names in this recorder's CSVs, in the order they are tried.
TIME_COLUMNS = ("unix_ns", "wallclock_unix_ns", "recording_start_unix_ns")
MONO_COLUMN = "mono_ns"


@dataclass(frozen=True)
class Step:
    mono_ns: int
    step_ns: int
    utc_before_ns: int
    utc_after_ns: int
    synced: bool


@dataclass
class Timebase:
    """The clock history of one session."""

    steps: list[Step]
    saw_sync: bool
    records: int

    @property
    def correctable(self) -> bool:
        """True when a correction can be computed and trusted.

        Requires evidence that the clock was eventually synchronized. Without
        it the final wall clock is as unreliable as the first, and shifting
        rows toward it would only move the error around.
        """
        return self.saw_sync

    @property
    def needs_correction(self) -> bool:
        return self.correctable and any(s.step_ns for s in self.steps)

    @property
    def total_step_ns(self) -> int:
        return sum(s.step_ns for s in self.steps)

    def offset_ns(self, mono_ns: int) -> int:
        """Nanoseconds to add to a row written at ``mono_ns``."""
        if not self.correctable:
            return 0
        return sum(s.step_ns for s in self.steps if s.mono_ns > mono_ns)

    def offsets_ns(self, mono_values: Iterable[int]) -> list[int]:
        return [self.offset_ns(int(m)) for m in mono_values]


def load(session: Path) -> Timebase:
    """Read ``clock_timebase.jsonl`` from a session directory."""
    path = Path(session) / TIMEBASE_FILE
    if not path.exists():
        return Timebase(steps=[], saw_sync=False, records=0)

    steps: list[Step] = []
    saw_sync = False
    count = 0

    for line in path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            # A power cut can truncate the final line. Everything before it is
            # still usable, and dropping the tail is better than refusing to
            # correct the session at all.
            continue
        count += 1
        if rec.get("synced"):
            saw_sync = True
        if rec.get("kind") == "step" and rec.get("step_ns"):
            steps.append(
                Step(
                    mono_ns=int(rec.get("mono_ns", 0)),
                    step_ns=int(rec["step_ns"]),
                    utc_before_ns=int(rec.get("utc_before_ns", 0)),
                    utc_after_ns=int(rec.get("utc_ns", 0)),
                    synced=bool(rec.get("synced")),
                )
            )

    steps.sort(key=lambda s: s.mono_ns)
    return Timebase(steps=steps, saw_sync=saw_sync, records=count)


def time_column(columns: Iterable[str]) -> str | None:
    """Pick the wall-clock column of a timestamps CSV."""
    cols = list(columns)
    for name in TIME_COLUMNS:
        if name in cols:
            return name
    return None


def correct_dataframe(df, tb: Timebase, time_col: str | None = None):
    """Return ``df`` with its wall-clock column corrected, plus a change count.

    The original values are preserved in ``<time_col>_recorded`` so the
    correction is reversible and the raw recording remains auditable. A session
    that needs no correction is returned untouched.
    """
    if not tb.needs_correction:
        return df, 0
    if MONO_COLUMN not in df.columns:
        # Recorded before mono_ns existed: which offset applies to which row is
        # unknowable, so leave it alone rather than shift the whole file.
        return df, 0

    col = time_col or time_column(df.columns)
    if col is None:
        return df, 0

    offsets = tb.offsets_ns(df[MONO_COLUMN].fillna(0).astype("int64"))
    changed = sum(1 for o in offsets if o)
    if not changed:
        return df, 0

    df = df.copy()
    df[f"{col}_recorded"] = df[col]
    df[col] = df[col].astype("int64") + offsets
    return df, changed


def describe(tb: Timebase) -> str:
    """One line summarising a session's clock history."""
    if tb.records == 0:
        return "no clock journal (recorded before clock tracking, or never written)"
    if not tb.saw_sync:
        return f"{tb.records} records, clock never synchronized — timestamps are NOT correctable"
    if not tb.steps:
        return f"{tb.records} records, clock synchronized, no steps — timestamps as recorded are correct"
    return (
        f"{tb.records} records, {len(tb.steps)} step(s), "
        f"total {tb.total_step_ns / 1e9:+.3f} s to apply to rows before them"
    )
