"""Lightweight in-process metrics (Phase 3)."""

from __future__ import annotations

import threading
import time
from collections import defaultdict
from dataclasses import dataclass, field
from typing import Any, Dict


@dataclass
class Metrics:
    started_at: float = field(default_factory=time.time)
    requests_total: int = 0
    by_surface: Dict[str, int] = field(default_factory=lambda: defaultdict(int))
    by_fc_mode: Dict[str, int] = field(default_factory=lambda: defaultdict(int))
    by_status: Dict[str, int] = field(default_factory=lambda: defaultdict(int))
    prompt_fc_retries: int = 0
    errors: int = 0
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False)

    def record_request(self, *, surface: str, fc_mode: str, ok: bool = True) -> None:
        with self._lock:
            self.requests_total += 1
            self.by_surface[surface] += 1
            self.by_fc_mode[fc_mode] += 1
            self.by_status["ok" if ok else "error"] += 1
            if not ok:
                self.errors += 1

    def record_retry(self) -> None:
        with self._lock:
            self.prompt_fc_retries += 1

    def snapshot(self) -> Dict[str, Any]:
        with self._lock:
            return {
                "uptime_seconds": int(time.time() - self.started_at),
                "requests_total": self.requests_total,
                "errors": self.errors,
                "prompt_fc_retries": self.prompt_fc_retries,
                "by_surface": dict(self.by_surface),
                "by_fc_mode": dict(self.by_fc_mode),
                "by_status": dict(self.by_status),
            }


GLOBAL_METRICS = Metrics()
