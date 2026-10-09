"""Server-Sent Events helpers — stable framing for multi-line data events."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Dict, List, Optional, Tuple


def format_sse(payload: Any, *, event: Optional[str] = None) -> str:
    data = payload if isinstance(payload, str) else json.dumps(payload, ensure_ascii=False)
    lines: List[str] = []
    if event:
        lines.append(f"event: {event}")
    for part in data.splitlines() or [""]:
        lines.append(f"data: {part}")
    lines.append("")
    return "\n".join(lines) + "\n"


def done_frame() -> str:
    return "data: [DONE]\n\n"


def try_json(data: str) -> Optional[Dict[str, Any]]:
    if data is None or data == "[DONE]":
        return None
    try:
        value = json.loads(data)
    except Exception:
        return None
    return value if isinstance(value, dict) else None


@dataclass
class SSEEvent:
    event: Optional[str] = None
    data: str = ""
    comments: List[str] = field(default_factory=list)
    id: Optional[str] = None
    retry: Optional[int] = None

    @property
    def is_done(self) -> bool:
        return self.data.strip() == "[DONE]"

    def json(self) -> Optional[Dict[str, Any]]:
        return try_json(self.data)


class SSEParser:
    """Incremental SSE frame parser. Feed lines (without trailing \\n)."""

    def __init__(self) -> None:
        self._event: Optional[str] = None
        self._data_parts: List[str] = []
        self._comments: List[str] = []
        self._id: Optional[str] = None
        self._retry: Optional[int] = None

    def feed_line(self, line: str) -> Optional[SSEEvent]:
        # Blank line → dispatch event
        if line == "":
            if (
                self._event is None
                and not self._data_parts
                and not self._comments
                and self._id is None
                and self._retry is None
            ):
                return None
            event = SSEEvent(
                event=self._event,
                data="\n".join(self._data_parts),
                comments=list(self._comments),
                id=self._id,
                retry=self._retry,
            )
            self._reset()
            return event

        if line.startswith(":"):
            self._comments.append(line[1:].lstrip())
            return None
        if line.startswith("event:"):
            self._event = line[6:].lstrip()
            return None
        if line.startswith("data:"):
            self._data_parts.append(line[5:].lstrip())
            return None
        if line.startswith("id:"):
            self._id = line[3:].lstrip()
            return None
        if line.startswith("retry:"):
            try:
                self._retry = int(line[6:].strip())
            except Exception:
                pass
            return None
        # Bare data fallback (non-strict servers)
        self._data_parts.append(line)
        return None

    def flush(self) -> Optional[SSEEvent]:
        if (
            self._event is None
            and not self._data_parts
            and not self._comments
            and self._id is None
            and self._retry is None
        ):
            return None
        event = SSEEvent(
            event=self._event,
            data="\n".join(self._data_parts),
            comments=list(self._comments),
            id=self._id,
            retry=self._retry,
        )
        self._reset()
        return event

    def _reset(self) -> None:
        self._event = None
        self._data_parts = []
        self._comments = []
        self._id = None
        self._retry = None


async def iter_sse_events(line_iter: AsyncIterator[str]) -> AsyncIterator[SSEEvent]:
    """Yield complete SSE events from an upstream line stream."""
    parser = SSEParser()
    async for raw in line_iter:
        line = raw.rstrip("\r")
        event = parser.feed_line(line)
        if event is not None:
            yield event
    tail = parser.flush()
    if tail is not None:
        yield tail
