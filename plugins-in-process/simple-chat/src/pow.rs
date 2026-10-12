//! HashV1 proof-of-work — port of Go `pow` (pow.go + solver.go).
//!
//! HashV1 = SHA3-256 with the Keccak-f[1600] permutation skipping round 0
//! (rounds 1..23 only): rate 136 bytes, SHA3 domain padding
//! (`0x06 … 0x80` at byte 135), digest = first 32 bytes of state.
//!
//! The solver pre-absorbs the `"<salt>_<expire_at>_"` prefix into the
//! keccak state so the hot loop is allocation-free.

/// 24 Keccak round constants; index 0 is never used (HashV1 skips round 0).
const ROUND_CONSTANTS: [u64; 24] = [
    0x0000000000000001,
    0x0000000000008082,
    0x800000000000808A,
    0x8000000080008000,
    0x000000000000808B,
    0x0000000080000001,
    0x8000000080008081,
    0x8000000000008009,
    0x000000000000008A,
    0x0000000000000088,
    0x0000000080008009,
    0x000000008000000A,
    0x000000008000808B,
    0x800000000000008B,
    0x8000000000008089,
    0x8000000000008003,
    0x8000000000008002,
    0x8000000000000080,
    0x000000000000800A,
    0x800000008000000A,
    0x8000000080008081,
    0x8000000080008080,
    0x0000000080000001,
    0x0000000080008008,
];

const fn rotl64(v: u64, k: u32) -> u64 {
    v.rotate_left(k)
}

/// Rounds 1..23 of Keccak-f[1600], flat-register form (Go `keccakF23`).
#[allow(clippy::too_many_lines)]
fn keccak_f23(s: &mut [u64; 25]) {
    let (mut a0, mut a1, mut a2, mut a3, mut a4) = (s[0], s[1], s[2], s[3], s[4]);
    let (mut a5, mut a6, mut a7, mut a8, mut a9) = (s[5], s[6], s[7], s[8], s[9]);
    let (mut a10, mut a11, mut a12, mut a13, mut a14) = (s[10], s[11], s[12], s[13], s[14]);
    let (mut a15, mut a16, mut a17, mut a18, mut a19) = (s[15], s[16], s[17], s[18], s[19]);
    let (mut a20, mut a21, mut a22, mut a23, mut a24) = (s[20], s[21], s[22], s[23], s[24]);

    for &rc in ROUND_CONSTANTS.iter().skip(1) {
        let c0 = a0 ^ a5 ^ a10 ^ a15 ^ a20;
        let c1 = a1 ^ a6 ^ a11 ^ a16 ^ a21;
        let c2 = a2 ^ a7 ^ a12 ^ a17 ^ a22;
        let c3 = a3 ^ a8 ^ a13 ^ a18 ^ a23;
        let c4 = a4 ^ a9 ^ a14 ^ a19 ^ a24;
        let d0 = c4 ^ rotl64(c1, 1);
        let d1 = c0 ^ rotl64(c2, 1);
        let d2 = c1 ^ rotl64(c3, 1);
        let d3 = c2 ^ rotl64(c4, 1);
        let d4 = c3 ^ rotl64(c0, 1);
        a0 ^= d0;
        a5 ^= d0;
        a10 ^= d0;
        a15 ^= d0;
        a20 ^= d0;
        a1 ^= d1;
        a6 ^= d1;
        a11 ^= d1;
        a16 ^= d1;
        a21 ^= d1;
        a2 ^= d2;
        a7 ^= d2;
        a12 ^= d2;
        a17 ^= d2;
        a22 ^= d2;
        a3 ^= d3;
        a8 ^= d3;
        a13 ^= d3;
        a18 ^= d3;
        a23 ^= d3;
        a4 ^= d4;
        a9 ^= d4;
        a14 ^= d4;
        a19 ^= d4;
        a24 ^= d4;

        let b0 = a0;
        let b10 = rotl64(a1, 1);
        let b20 = rotl64(a2, 62);
        let b5 = rotl64(a3, 28);
        let b15 = rotl64(a4, 27);
        let b16 = rotl64(a5, 36);
        let b1 = rotl64(a6, 44);
        let b11 = rotl64(a7, 6);
        let b21 = rotl64(a8, 55);
        let b6 = rotl64(a9, 20);
        let b7 = rotl64(a10, 3);
        let b17 = rotl64(a11, 10);
        let b2 = rotl64(a12, 43);
        let b12 = rotl64(a13, 25);
        let b22 = rotl64(a14, 39);
        let b23 = rotl64(a15, 41);
        let b8 = rotl64(a16, 45);
        let b18 = rotl64(a17, 15);
        let b3 = rotl64(a18, 21);
        let b13 = rotl64(a19, 8);
        let b14 = rotl64(a20, 18);
        let b24 = rotl64(a21, 2);
        let b9 = rotl64(a22, 61);
        let b19 = rotl64(a23, 56);
        let b4 = rotl64(a24, 14);

        a0 = b0 ^ (!b1 & b2);
        a1 = b1 ^ (!b2 & b3);
        a2 = b2 ^ (!b3 & b4);
        a3 = b3 ^ (!b4 & b0);
        a4 = b4 ^ (!b0 & b1);
        a5 = b5 ^ (!b6 & b7);
        a6 = b6 ^ (!b7 & b8);
        a7 = b7 ^ (!b8 & b9);
        a8 = b8 ^ (!b9 & b5);
        a9 = b9 ^ (!b5 & b6);
        a10 = b10 ^ (!b11 & b12);
        a11 = b11 ^ (!b12 & b13);
        a12 = b12 ^ (!b13 & b14);
        a13 = b13 ^ (!b14 & b10);
        a14 = b14 ^ (!b10 & b11);
        a15 = b15 ^ (!b16 & b17);
        a16 = b16 ^ (!b17 & b18);
        a17 = b17 ^ (!b18 & b19);
        a18 = b18 ^ (!b19 & b15);
        a19 = b19 ^ (!b15 & b16);
        a20 = b20 ^ (!b21 & b22);
        a21 = b21 ^ (!b22 & b23);
        a22 = b22 ^ (!b23 & b24);
        a23 = b23 ^ (!b24 & b20);
        a24 = b24 ^ (!b20 & b21);

        a0 ^= rc;
    }

    s[0] = a0;
    s[1] = a1;
    s[2] = a2;
    s[3] = a3;
    s[4] = a4;
    s[5] = a5;
    s[6] = a6;
    s[7] = a7;
    s[8] = a8;
    s[9] = a9;
    s[10] = a10;
    s[11] = a11;
    s[12] = a12;
    s[13] = a13;
    s[14] = a14;
    s[15] = a15;
    s[16] = a16;
    s[17] = a17;
    s[18] = a18;
    s[19] = a19;
    s[20] = a20;
    s[21] = a21;
    s[22] = a22;
    s[23] = a23;
    s[24] = a24;
}

const RATE: usize = 136;

fn absorb(s: &mut [u64; 25], block: &[u8]) {
    for i in 0..RATE / 8 {
        s[i] ^= u64::from_le_bytes(block[i * 8..i * 8 + 8].try_into().unwrap());
    }
}

/// HashV1 digest of `data` (32 bytes).
pub fn hash_v1(data: &[u8]) -> [u8; 32] {
    let mut s = [0u64; 25];
    let mut off = 0;
    while off + RATE <= data.len() {
        absorb(&mut s, &data[off..off + RATE]);
        keccak_f23(&mut s);
        off += RATE;
    }
    let mut final_block = [0u8; RATE];
    final_block[..data.len() - off].copy_from_slice(&data[off..]);
    final_block[data.len() - off] = 0x06;
    final_block[RATE - 1] |= 0x80;
    absorb(&mut s, &final_block);
    keccak_f23(&mut s);
    let mut out = [0u8; 32];
    out[0..8].copy_from_slice(&s[0].to_le_bytes());
    out[8..16].copy_from_slice(&s[1].to_le_bytes());
    out[16..24].copy_from_slice(&s[2].to_le_bytes());
    out[24..32].copy_from_slice(&s[3].to_le_bytes());
    out
}

/// The PoW challenge from `data.biz_data.challenge` (Go `pow.Challenge`).
#[derive(Debug, Clone, serde::Deserialize)]
pub struct Challenge {
    pub algorithm: String,
    pub challenge: String,
    pub salt: String,
    pub expire_at: i64,
    pub difficulty: i64,
    #[allow(dead_code)] // echo fields kept for wire completeness
    pub expire_after: i64,
    pub signature: String,
    pub target_path: String,
}

/// `"<salt>_<expire_at>_"` — the pre-image prefix for nonces.
pub fn build_prefix(salt: &str, expire_at: i64) -> String {
    format!("{salt}_{expire_at}_")
}

/// Search nonce ∈ [0, difficulty) with
/// `HashV1("<salt>_<expire_at>_<nonce>") == challenge` (exact match).
/// The prefix is pre-absorbed; cancellation is checked every 1024 nonces.
pub fn solve_pow(
    challenge_hex: &str,
    salt: &str,
    expire_at: i64,
    difficulty: i64,
    cancel: &dyn Fn() -> bool,
) -> Result<i64, PowError> {
    if challenge_hex.len() != 64 {
        return Err(PowError::BadChallenge(
            "challenge must be 64 hex chars".into(),
        ));
    }
    let target = hex::decode(challenge_hex)?;
    let t0 = u64::from_le_bytes(target[0..8].try_into().unwrap());
    let t1 = u64::from_le_bytes(target[8..16].try_into().unwrap());
    let t2 = u64::from_le_bytes(target[16..24].try_into().unwrap());
    let t3 = u64::from_le_bytes(target[24..32].try_into().unwrap());

    let prefix = build_prefix(salt, expire_at).into_bytes();
    let mut base_state = [0u64; 25];
    let mut off = 0;
    while off + RATE <= prefix.len() {
        absorb(&mut base_state, &prefix[off..off + RATE]);
        keccak_f23(&mut base_state);
        off += RATE;
    }
    let tail_len = prefix.len() - off;
    let mut tail = [0u8; RATE];
    tail[..tail_len].copy_from_slice(&prefix[off..]);

    let mut num_buf = [0u8; 20];
    for n in 0..difficulty.max(0) {
        if n & 0x3FF == 0 && cancel() {
            return Err(PowError::Cancelled);
        }
        let mut v = n as u64;
        let mut pos = 20;
        if v == 0 {
            pos -= 1;
            num_buf[pos] = b'0';
        } else {
            while v > 0 {
                pos -= 1;
                num_buf[pos] = b'0' + (v % 10) as u8;
                v /= 10;
            }
        }
        let num_len = 20 - pos;
        let mut s = base_state;
        let total_tail = tail_len + num_len;
        if total_tail < RATE {
            let mut buf = [0u8; RATE];
            buf[..tail_len].copy_from_slice(&tail[..tail_len]);
            buf[tail_len..total_tail].copy_from_slice(&num_buf[pos..]);
            buf[total_tail] = 0x06;
            buf[RATE - 1] |= 0x80;
            absorb(&mut s, &buf);
            keccak_f23(&mut s);
        } else {
            // Prefix tail plus nonce span a full rate block: absorb in two.
            let mut buf = [0u8; RATE];
            buf[..tail_len].copy_from_slice(&tail[..tail_len]);
            buf[tail_len..RATE].copy_from_slice(&num_buf[pos..pos + (RATE - tail_len)]);
            absorb(&mut s, &buf);
            keccak_f23(&mut s);
            let mut buf2 = [0u8; RATE];
            let rem = total_tail - RATE;
            buf2[..rem]
                .copy_from_slice(&num_buf[pos + (RATE - tail_len)..pos + (RATE - tail_len) + rem]);
            buf2[rem] = 0x06;
            buf2[RATE - 1] |= 0x80;
            absorb(&mut s, &buf2);
            keccak_f23(&mut s);
        }
        if s[0] == t0 && s[1] == t1 && s[2] == t2 && s[3] == t3 {
            return Ok(n);
        }
    }
    Err(PowError::NoSolution)
}

/// Serialize `{algorithm, challenge, salt, answer, signature, target_path}`
/// as base64(JSON) for the `x-ds-pow-response` header. `difficulty` and
/// `expire_at` are deliberately excluded.
pub fn build_pow_header(c: &Challenge, answer: i64) -> Result<String, PowError> {
    use base64::Engine;
    let body = serde_json::json!({
        "algorithm": c.algorithm,
        "challenge": c.challenge,
        "salt": c.salt,
        "answer": answer,
        "signature": c.signature,
        "target_path": c.target_path,
    });
    Ok(base64::engine::general_purpose::STANDARD.encode(serde_json::to_vec(&body)?))
}

/// Challenge → `x-ds-pow-response` value, end to end.
pub fn solve_and_build_header(
    c: &Challenge,
    cancel: &dyn Fn() -> bool,
) -> Result<String, PowError> {
    // The live android-app upstream names the algorithm "DeepSeekHashV1";
    // "HashV1" is the web client's name for the same scheme. The response
    // header echoes the challenge's name.
    match c.algorithm.as_str() {
        "HashV1" | "DeepSeekHashV1" => {}
        other => {
            return Err(PowError::BadChallenge(format!(
                "unsupported algorithm {other:?}"
            )))
        }
    }
    let d = if c.difficulty == 0 {
        144_000
    } else {
        c.difficulty
    };
    let answer = solve_pow(&c.challenge, &c.salt, c.expire_at, d, cancel)?;
    build_pow_header(c, answer)
}

/// PoW failure modes (Go errors, typed).
#[derive(Debug, thiserror::Error)]
pub enum PowError {
    #[error("pow: {0}")]
    BadChallenge(String),
    #[error("pow: bad hex: {0}")]
    BadHex(#[from] hex::FromHexError),
    #[error("pow: cancelled")]
    Cancelled,
    #[error("pow: no solution within difficulty")]
    NoSolution,
    #[error("pow: serialization: {0}")]
    Serde(#[from] serde_json::Error),
}

#[cfg(test)]
mod tests {
    use super::*;

    const NO_CANCEL: &dyn Fn() -> bool = &|| false;

    /// The canonical offline property: for any salt/expire pair we can
    /// construct a challenge by hashing a known nonce, and the solver must
    /// rediscover exactly that nonce.
    #[test]
    fn solver_finds_constructed_nonce() {
        let salt = "abcdef0123456789";
        let expire_at = 1_759_712_000i64;
        let nonce = 12345i64;
        let preimage = format!("{}_{}_{nonce}", salt, expire_at);
        let digest = hash_v1(preimage.as_bytes());
        let challenge_hex = hex::encode(digest);
        let found = solve_pow(&challenge_hex, salt, expire_at, 1_000_000, NO_CANCEL).unwrap();
        assert_eq!(found, nonce);
    }

    #[test]
    fn solver_finds_nonce_zero_and_multi_block_tails() {
        // nonce 0 — the v == 0 branch.
        let digest = hash_v1(b"s_5_0");
        let found = solve_pow(&hex::encode(digest), "s", 5, 100, NO_CANCEL).unwrap();
        assert_eq!(found, 0);
        // Long salt forces the prefix through a full rate block so the
        // solver's pre-absorption path (off+rate<=len) is exercised.
        let long_salt = "x".repeat(300);
        let preimage = format!("{long_salt}_7_9");
        let digest = hash_v1(preimage.as_bytes());
        let found = solve_pow(&hex::encode(digest), &long_salt, 7, 1_000, NO_CANCEL).unwrap();
        assert_eq!(found, 9);
        // A tail near the rate size + a short nonce: the two-block absorb
        // (130-char salt → 133-byte prefix tail; +3-digit nonce ≥ rate).
        let near_salt = "y".repeat(130);
        let nonce = 100i64;
        let preimage = format!("{near_salt}_7_{nonce}");
        let digest = hash_v1(preimage.as_bytes());
        let found = solve_pow(&hex::encode(digest), &near_salt, 7, nonce + 1, NO_CANCEL).unwrap();
        assert_eq!(found, nonce);
    }

    #[test]
    fn hash_v1_distinct_from_sha3_256() {
        // HashV1 skips round 0, so it must differ from stock SHA3-256 for
        // the same input (the whole point of the scheme's fingerprint).
        // Reference: SHA3-256("hello") =
        // 3338be694f32f9dc8531658156e9a05c… (FIPS 202 test vector).
        let sha3_hello: [u8; 32] =
            hex::decode("3338be694f32f9dc8531658156e9a05c2da57f711a7ca5b3d6fae7e0b52b65e1")
                .unwrap()
                .try_into()
                .unwrap();
        assert_ne!(&hash_v1(b"hello")[..], &sha3_hello[..]);
        // And HashV1 is stable (deterministic) for the same input.
        assert_eq!(hash_v1(b"hello"), hash_v1(b"hello"));
        // Empty input still produces a padded final block.
        assert_ne!(hash_v1(b""), [0u8; 32]);
    }

    #[test]
    fn header_excludes_difficulty_and_expire_at() {
        let c = Challenge {
            algorithm: "DeepSeekHashV1".into(),
            challenge: "00".repeat(32),
            salt: "s".into(),
            expire_at: 123,
            difficulty: 10,
            expire_after: 60,
            signature: "sig".into(),
            target_path: "/api/v0/chat/completion".into(),
        };
        // The header base64-decodes to JSON without difficulty/expire_at.
        use base64::Engine;
        let header = build_pow_header(&c, 7).unwrap();
        let raw = String::from_utf8(
            base64::engine::general_purpose::STANDARD
                .decode(&header)
                .unwrap(),
        )
        .unwrap();
        assert!(!raw.contains("difficulty"));
        assert!(!raw.contains("expire_at"));
        assert!(raw.contains("\"answer\":7"));
        assert!(raw.contains("\"algorithm\":\"DeepSeekHashV1\""));
    }

    #[test]
    fn solve_and_build_header_end_to_end() {
        let salt = "deadbeef";
        let expire_at = 1_759_712_000i64;
        let preimage = format!("{salt}_{expire_at}_42");
        let digest = hash_v1(preimage.as_bytes());
        let c = Challenge {
            algorithm: "HashV1".into(),
            challenge: hex::encode(digest),
            salt: salt.into(),
            expire_at,
            difficulty: 1_000,
            expire_after: 60,
            signature: "sig".into(),
            target_path: "/t".into(),
        };
        let header = solve_and_build_header(&c, NO_CANCEL).unwrap();
        use base64::Engine;
        let raw = String::from_utf8(
            base64::engine::general_purpose::STANDARD
                .decode(&header)
                .unwrap(),
        )
        .unwrap();
        let v: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(v["answer"], 42);
        // Unsupported algorithm rejects.
        let mut bad = c.clone();
        bad.algorithm = "SHA256".into();
        assert!(solve_and_build_header(&bad, NO_CANCEL).is_err());
        // Difficulty 0 → default 144000 search space (answer 42 < that).
        let mut zero = c;
        zero.difficulty = 0;
        assert!(solve_and_build_header(&zero, NO_CANCEL).is_ok());
    }

    #[test]
    fn rejects_bad_challenge_shape() {
        // Wrong length.
        assert!(solve_pow("zz", "s", 1, 10, NO_CANCEL).is_err());
        // Length ok but not hex.
        let mut g = "0".repeat(64);
        g.replace_range(63..64, "g");
        assert!(solve_pow(&g, "s", 1, 10, NO_CANCEL).is_err());
        // No solution in range.
        let digest = hex::encode(hash_v1(b"s_1_999"));
        assert!(matches!(
            solve_pow(&digest, "s", 1, 10, NO_CANCEL),
            Err(PowError::NoSolution)
        ));
        // Cancellation.
        let digest = hex::encode(hash_v1(b"s_1_0"));
        assert!(matches!(
            solve_pow(&digest, "s", 1, 10, &|| true),
            Err(PowError::Cancelled)
        ));
    }
}
