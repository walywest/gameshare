"""UI-agnostic formatting helpers, shared by the Tk GUI and the Textual TUI.

Kept free of any UI-toolkit import so the TUI can run without Tkinter.
"""
from __future__ import annotations


def format_bytes(n: float) -> str:
    if n is None:
        return "?"
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024:
            return f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} PB"


def format_speed(bps: float) -> str:
    return f"{format_bytes(bps)}/s"


def format_eta(seconds: float) -> str:
    if seconds is None or seconds <= 0 or seconds > 24 * 3600:
        return "--:--"
    m, s = divmod(int(seconds), 60)
    h, m = divmod(m, 60)
    if h:
        return f"{h}:{m:02d}:{s:02d}"
    return f"{m}:{s:02d}"
