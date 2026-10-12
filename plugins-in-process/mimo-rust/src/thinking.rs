//! Thinking-marker splitter: 3-state machine separating reasoning from
//! content deltas. 1:1 port of Go `internal/adapter/thinking.go`.

/// Opening marker as it appears in MiMo message deltas; the raw stream sends
/// the marker followed by a NUL byte (SDD §2.5, PROTOCOL.md:79-84).
pub const THINK_OPEN: &str = "<think>";
/// Closing marker (with leading slash).
pub const THINK_CLOSE: &str = "</think>";

/// One fragment emitted by the splitter: reasoning (inside a think block) or
/// content (outside).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Piece {
    /// "reasoning" | "content".
    pub kind: PieceKind,
    pub text: String,
}

impl Piece {
    pub fn reasoning(text: impl Into<String>) -> Self {
        Self {
            kind: PieceKind::Reasoning,
            text: text.into(),
        }
    }
    pub fn content(text: impl Into<String>) -> Self {
        Self {
            kind: PieceKind::Content,
            text: text.into(),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PieceKind {
    Reasoning,
    Content,
}

impl PieceKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            PieceKind::Reasoning => "reasoning",
            PieceKind::Content => "content",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
enum State {
    #[default]
    Outside,
    Thinking,
    Answer,
}

/// ThinkSplitter splits mixed MiMo deltas into reasoning/content pieces.
/// Tolerates marker bytes split across chunk boundaries by holding back a
/// partial-marker tail, and treats an unterminated think block at stream end
/// as reasoning (SDD §2.5, TDD THINK/SSE-009).
#[derive(Debug, Default)]
pub struct ThinkSplitter {
    buf: String,
    state: State,
}

impl ThinkSplitter {
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed one delta; returns the pieces that can be emitted now. NUL bytes
    /// are stripped on entry (marker location never depends on them, TDD
    /// SSE-008); output never contains NUL or markers.
    pub fn push(&mut self, delta: &str) -> Vec<Piece> {
        if delta.is_empty() {
            return Vec::new();
        }
        let cleaned: String = delta.chars().filter(|c| *c != '\x00').collect();
        self.buf.push_str(&cleaned);
        self.drain(false)
    }

    /// Stream end: emits whatever remains, according to current state. An
    /// unterminated think block counts entirely as reasoning.
    pub fn flush(&mut self) -> Vec<Piece> {
        let mut out = self.drain(true);
        let rem = std::mem::take(&mut self.buf);
        if rem.is_empty() {
            return out;
        }
        let piece = match self.state {
            State::Thinking => Piece::reasoning(rem),
            _ => Piece::content(rem),
        };
        out.push(piece);
        out
    }

    /// Scans the buffer, emitting complete segments and holding back a
    /// partial-marker suffix so a marker split across pushes is recognized
    /// next round; with `is_final=true` everything is emitted.
    fn drain(&mut self, is_final: bool) -> Vec<Piece> {
        let mut out = Vec::new();
        let mut data = std::mem::take(&mut self.buf);

        loop {
            match self.state {
                State::Thinking => match data.find(THINK_CLOSE) {
                    Some(idx) => {
                        emit(&mut out, PieceKind::Reasoning, &data[..idx]);
                        data.drain(..idx + THINK_CLOSE.len());
                        self.state = State::Answer;
                    }
                    None => {
                        let mut hold = partial_marker_suffix(&data, THINK_CLOSE);
                        if is_final {
                            hold = 0;
                        }
                        hold_back(
                            &mut out,
                            &mut data,
                            &mut self.buf,
                            hold,
                            PieceKind::Reasoning,
                        );
                        return out;
                    }
                },
                State::Outside | State::Answer => match data.find(THINK_OPEN) {
                    Some(idx) => {
                        emit(&mut out, PieceKind::Content, &data[..idx]);
                        data.drain(..idx + THINK_OPEN.len());
                        self.state = State::Thinking;
                    }
                    None => {
                        let mut hold = partial_marker_suffix(&data, THINK_OPEN);
                        if is_final {
                            hold = 0;
                        }
                        hold_back(&mut out, &mut data, &mut self.buf, hold, PieceKind::Content);
                        return out;
                    }
                },
            }
        }
    }
}

fn emit(out: &mut Vec<Piece>, kind: PieceKind, text: &str) {
    if !text.is_empty() {
        out.push(Piece {
            kind,
            text: text.to_string(),
        });
    }
}

/// Longest suffix of `data` that is a proper prefix of `marker` (0 when
/// none). Only that suffix needs holding back for the next push — everything
/// before it is safe to emit now (TDD THINK-004).
fn partial_marker_suffix(data: &str, marker: &str) -> usize {
    let data_b = data.as_bytes();
    let marker_b = marker.as_bytes();
    let max = (marker_b.len() - 1).min(data_b.len());
    for n in (1..=max).rev() {
        if data_b.ends_with(&marker_b[..n]) {
            return n;
        }
    }
    0
}

/// Emits everything except a possible partial-marker suffix (which stays in
/// the buffer for the next push). With `hold == 0` everything is emitted.
fn hold_back(
    out: &mut Vec<Piece>,
    data: &mut String,
    buf: &mut String,
    hold: usize,
    kind: PieceKind,
) {
    let d_len = data.len();
    if hold == 0 || hold >= d_len {
        if hold >= d_len && hold > 0 {
            // Entire tail might be a partial marker.
            buf.push_str(data);
        } else {
            emit(out, kind, data);
        }
        data.clear();
        return;
    }
    let split = d_len - hold;
    let tail = data.split_off(split);
    emit(out, kind, data);
    buf.push_str(&tail);
}

/// Folds pieces into (reasoning, content) strings — non-streaming collection
/// and tests.
pub fn merge_pieces(pieces: &[Piece]) -> (String, String) {
    let mut r = String::new();
    let mut c = String::new();
    for p in pieces {
        match p.kind {
            PieceKind::Reasoning => r.push_str(&p.text),
            PieceKind::Content => c.push_str(&p.text),
        }
    }
    (r, c)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kinds(pieces: &[Piece]) -> Vec<&'static str> {
        pieces.iter().map(|p| p.kind.as_str()).collect()
    }

    #[test]
    fn plain_delta_is_content() {
        let mut s = ThinkSplitter::new();
        assert_eq!(s.push("hello"), vec![Piece::content("hello")]);
        assert!(s.flush().is_empty());
    }

    #[test]
    fn open_close_markers_split() {
        let mut s = ThinkSplitter::new();
        let out = s.push("<think>abc</think>def");
        assert_eq!(kinds(&out), vec!["reasoning", "content"]);
        assert_eq!(out[0].text, "abc");
        assert_eq!(out[1].text, "def");
    }

    #[test]
    fn unterminated_block_is_reasoning() {
        // TDD SSE-009: no close marker → everything after open is reasoning.
        let mut s = ThinkSplitter::new();
        assert_eq!(s.push("<think>partial"), vec![Piece::reasoning("partial")]);
        assert!(s.flush().is_empty());
    }

    #[test]
    fn unterminated_answer_tail_is_content() {
        let mut s = ThinkSplitter::new();
        s.push("<think>r</think>");
        s.flush();
        let out = s.push("tail");
        assert_eq!(out, vec![Piece::content("tail")]);
        assert!(s.flush().is_empty());
    }

    #[test]
    fn marker_split_across_pushes() {
        // "<thi" + "l..." — the partial marker suffix is held back.
        let mut s = ThinkSplitter::new();
        assert!(s.push("<thi").is_empty());
        let out = s.push("nk>reason</think>done");
        assert_eq!(kinds(&out), vec!["reasoning", "content"]);
        assert_eq!(out[0].text, "reason");
        assert_eq!(out[1].text, "done");
    }

    #[test]
    fn close_marker_split_across_pushes() {
        let mut s = ThinkSplitter::new();
        s.push("<think>abc</th");
        let out = s.push("ink>def");
        assert_eq!(out, vec![Piece::content("def")]);
    }

    #[test]
    fn nul_bytes_stripped() {
        // TDD SSE-008: NULs never affect marker location.
        let mut s = ThinkSplitter::new();
        let out = s.push("<think>\x00a\x00b</think>\x00c");
        assert_eq!(out, vec![Piece::reasoning("ab"), Piece::content("c")]);
    }

    #[test]
    fn multiple_blocks() {
        let mut s = ThinkSplitter::new();
        let out = s.push("a<think>1</think>b<think>2</think>c");
        assert_eq!(
            kinds(&out),
            vec!["content", "reasoning", "content", "reasoning", "content"]
        );
        assert_eq!(
            merge_pieces(&out),
            (String::from("12"), String::from("abc"))
        );
    }

    #[test]
    fn short_delta_not_swallowed() {
        // TDD THINK-004: a 1-byte delta with no marker resemblance emits
        // immediately — only a true partial-marker suffix is held back.
        let mut s = ThinkSplitter::new();
        assert_eq!(s.push("x"), vec![Piece::content("x")]);
    }

    #[test]
    fn full_marker_prefix_tail_held() {
        // "<think" without the trailing byte is a proper prefix of the open
        // marker: the whole tail is held back.
        let mut s = ThinkSplitter::new();
        assert!(s.push("<thi").is_empty());
        let out = s.push("nk>r</think>d");
        assert_eq!(kinds(&out), vec!["reasoning", "content"]);
    }

    #[test]
    fn merge_pieces_empty() {
        assert_eq!(merge_pieces(&[]), (String::new(), String::new()));
    }
}
