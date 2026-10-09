package mimo

import "unicode/utf8"

// SpeechTextLimit is the upstream's hard cap on one synthesis: the Studio
// refuses any request whose spoken text is longer ("播报内容长度不能超过1024，
// 当前长度为：N" — verified live). The gateway enforces the same number as its
// own client-facing limit rather than working around it: a piece synthesised
// close to the cap makes the upstream fail the task itself, so splitting long
// text only delays the error (ADR 0010, measured live 2026-10-09: an
// ~1020-rune piece failed upstream after 278 s while 384 runes took 17.8 s).
const SpeechTextLimit = 1024

// RuneLen counts characters the way the upstream does — runes, not bytes, so
// Chinese text is measured in characters.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }
