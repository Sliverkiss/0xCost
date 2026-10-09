# ADR 0029 — A client-supplied URL is hostile, and what we decode is bounded

Status: accepted (2026-10-09). Supersedes nothing; extends ADR 0015 (attachments)
and ADR 0028 (images travel the attachment path).

## Context

Image input made the gateway fetch a URL **the caller chooses** and turn those bytes
into work: an HTTP GET, an upload to the platform, and — for `image_url.detail:
"low"` — a full decode of the image into a pixel buffer. Three things followed from
that, and none of them were true of the gateway before images existed:

1. **The fetch was an outbound request to anywhere.** The URL was checked for shape
   (`data:image/…` or http(s)) and nothing else. Live-verified before the fix: with
   the ordinary API key, a request could point `image_url` at
   `http://172.17.0.1:443/` and read the host's own reverse proxy back as an HTTP
   status, at `http://127.0.0.1:21223/v2/models` and read a 401, and could dial the
   host's SSH/frp/sing-box ports well enough to distinguish "open" from "closed" by
   the error text. The public API port is reachable from the internet, so this was a
   probe into the operator's own host with a legitimate key as the only requirement.
2. **The decode was unbounded.** PNG headers declare dimensions, so a small payload
   can describe a huge image. Measured on a **fresh** container, one request carrying a
   198 KB PNG that declares 8000×8000 took its resident memory from **5.3 MiB to
   498.9 MiB and it stayed there** — the 64 MP RGBA buffer is 244 MB and the heap it
   forces is not returned promptly (a second run on that heap peaked at 680 MiB, which
   is the number a sampler that polls every 0.3 s reports: the honest magnitude is the
   retained one). A 16 MiB request body can describe far more, and the container had
   **no memory limit**, so the ceiling was the host's.
3. **The redirect cap was a comment.** `attachmentMaxRedirects = 5` existed with a
   comment explaining why it was not configurable, but nothing consulted it: the
   client used Go's default policy of 10 hops.

## Decision

- **The connection is the gate, not the URL string.** A dedicated client for
  client-supplied images dials through `imageAddressAllowed`, which refuses any
  destination in loopback, private, link-local (cloud metadata at 169.254.169.254),
  CGNAT, unspecified or multicast space. Because the check runs at dial time it also
  covers **every redirect hop** and any name whose resolution lands in a blocked
  range — not just the literal the caller wrote. A refused target is a named 400
  (`image_url_blocked`, `param` naming the image URL) and is classified as a
  content-level failure: the account is never blamed for a client's bad URL.
- **An operator escape hatch, off by default.** `MIMO2API_IMAGE_ALLOW_PRIVATE=true`
  exists for a deployment that legitimately serves its own images. The default
  assumes the URL is hostile, because in a public deployment it usually is.
- **Nothing is decoded past a bound.** `MIMO2API_MAX_IMAGE_PIXELS` (default 40 MP
  ≈ 160 MB of RGBA, far above any real chat image) is read from the **header** before
  any decode; a larger image is uploaded as it arrived and the reason is logged. The
  client asked for a level, not for a promise that we would always spend unbounded
  memory to deliver it.
- **The redirect cap is enforced**, so the constant's comment is now true.
- **The container gets limits**: `mem_limit: 1g`, `pids_limit: 256`,
  `no-new-privileges`, `cap_drop: ALL`. Code-level bounds decide what we accept;
  the container's limits decide how bad a mistake in that code can get. Defence in
  depth is not a substitute for the guard, it is what stands behind it.
- **A client-supplied URL is only ever fetched from this one client.** Audio input
  (multipart) and everything else keep their own paths; there is no general-purpose
  "fetch this for me" surface, and this ADR is the reason there will not be one
  without the same guard.

## Consequences

- A request that used to work — fetching an image from a private address — now fails
  with `image_url_blocked` unless the operator opts in. That is the intent.
- A >40 MP image with `detail: "low"` is uploaded unscaled rather than decoded. The
  level is a fidelity hint, not a contract about our memory.
- IMG-014 (eight blocked address forms, no connection opened at all), IMG-015 (bomb
  header refused, level still applied under the bound) and IMG-016 (address table and
  the redirect cap) hold the behaviour; the container limits are verified by
  `docker inspect` after a deploy.
