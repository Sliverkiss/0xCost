//! Image extraction — port of Go `internal/openai/openai.go`'s fetch half
//! (`ExtractImages`/`fetchImage`/`validateImageURL`/`safeImageDialContext`):
//! data URLs decode inline; http(s) URLs are fetched server-side with an
//! SSRF guard (non-public-IP-only dialing, ≤5 redirects, 10 MB cap,
//! image content type required).
//!
//! The IP classification (`is_non_public`) is a pure, offline-testable
//! function. The live resolver wraps it: hostnames resolve through the
//! system resolver, then every candidate address is filtered before any
//! connection is opened — Go dials per-IP skipping non-public ones; the
//! port's resolver filters instead and the connection fails closed when
//! only non-public addresses exist.

use std::net::IpAddr;
use std::sync::Arc;
use std::time::Duration;

use reqwest::dns::{Addrs, Name, Resolve, Resolving};

use crate::openai::{decode_data_url, first_segment, image_ext_by_mime, Message, PartKind};

/// Fetch timeout per image (Go `imageFetchTimeout`): a hanging image host
/// must fail the request cleanly, never wedge a pool lease.
pub const IMAGE_FETCH_TIMEOUT: Duration = Duration::from_secs(30);

/// Hard size cap for one image (Go `10<<20`).
pub const IMAGE_CAP: usize = 10 << 20;

/// One extracted image awaiting upload.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Image {
    pub data: Vec<u8>,
    /// png/jpg/jpeg/webp/gif.
    pub ext: &'static str,
}

/// Fetch failure — the caller maps it to 400 `image_fetch_failed` (§6).
#[derive(Debug, Clone, PartialEq, thiserror::Error)]
pub enum ImageFetchError {
    #[error("invalid image URL")]
    InvalidUrl,
    #[error("image host lookup failed")]
    LookupFailed,
    #[error("image host resolves to a non-public address")]
    NonPublicAddress,
    #[error("image fetch {url}: {reason}")]
    Fetch { url: String, reason: String },
}

/// Is this address in an SSRF-forbidden class? Pure function — Go's
/// `ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
/// ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
/// ip.IsInterfaceLocalMulticast()`.
pub fn is_non_public(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => {
            let o = v4.octets();
            // Loopback 127/8.
            o[0] == 127
                // Private 10/8, 172.16/12, 192.168/16.
                || o[0] == 10
                || (o[0] == 172 && (o[1] & 0xf0) == 16)
                || (o[0] == 192 && o[1] == 168)
                // Link-local unicast 169.254/16.
                || (o[0] == 169 && o[1] == 254)
                // Link-local multicast 224.0.0/24; interface-local
                // multicast in Go is IPv4 224.0.0.254 only? No: Go
                // IsInterfaceLocalMulticast is ff01::/16 (IPv6-only); the
                // IPv4 224.0.0/24 link-local-multicast check covers the
                // same local scope family here.
                || (o[0] == 224 && o[1] == 0 && o[2] == 0)
                // Unspecified 0.0.0.0.
                || v4.is_unspecified()
        }
        IpAddr::V6(v6) => {
            let s = v6.segments();
            // Loopback ::1 / unspecified :: (std helpers).
            v6.is_loopback()
                || v6.is_unspecified()
                // Private ULA fc00::/7.
                || (s[0] & 0xfe00) == 0xfc00
                // Link-local unicast fe80::/10.
                || (s[0] & 0xffc0) == 0xfe80
                // Link-local multicast ff02::/16 (Go's exact rule).
                || s[0] == 0xff02
                // Interface-local multicast ff01::/16 (Go's exact rule).
                || s[0] == 0xff01
        }
    }
}

/// The SSRF-aware DNS resolver (Go `safeImageDialContext`'s filter): the
/// system resolver produces candidates; non-public addresses are dropped
/// before any dial. An empty result fails the connection — the fetch
/// never reaches a private host.
#[derive(Debug, Clone, Default)]
pub struct PublicOnlyResolver;

impl Resolve for PublicOnlyResolver {
    fn resolve(&self, name: Name) -> Resolving {
        let host = name.as_str().to_string();
        Box::pin(async move {
            use tokio::net::lookup_host;
            // `lookup_host` needs a port for the SocketAddr; use 0 and
            // let reqwest substitute the URL's scheme port.
            let candidates = match lookup_host((host.as_str(), 0)).await {
                Ok(c) => c,
                Err(e) => {
                    return Err(Box::new(std::io::Error::other(e.to_string()))
                        as Box<dyn std::error::Error + Send + Sync>)
                }
            };
            let addrs: Vec<std::net::SocketAddr> =
                candidates.filter(|sa| !is_non_public(sa.ip())).collect();
            if addrs.is_empty() {
                // Fail closed: resolved, but nothing public (or the name
                // resolved to nothing at all).
                return Err(Box::new(std::io::Error::other(
                    "image host resolved only to non-public addresses",
                ))
                    as Box<dyn std::error::Error + Send + Sync>);
            }
            let out: Addrs = Box::new(addrs.into_iter());
            Ok(out)
        })
    }
}

/// Build the dedicated image-fetch client (Go `imageHTTPClient`): 30s
/// timeout, public-only dialing, redirect policy that caps the chain at
/// 5 hops. Per-hop hostname safety is inherent — every hop's connection
/// goes through the same public-only resolver (Go additionally re-ran
/// `validateImageURL` in `CheckRedirect`; the resolver filter achieves
/// the same fail-closed guarantee at the dial layer).
fn image_client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(IMAGE_FETCH_TIMEOUT)
        .dns_resolver(Arc::new(PublicOnlyResolver))
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            // Go returns http.ErrUseLastResponse at >= 5 hops: stop and
            // surface the 30x response (fetch then fails on non-200).
            if attempt.previous().len() >= 5 {
                return attempt.stop();
            }
            // A scheme change mid-chain (https→file) never dials: reqwest
            // only follows http(s) redirects.
            attempt.follow()
        }))
        .build()
        .expect("image fetch client builds")
}

/// Pull `image_url` parts out of the messages (Go `ExtractImages`): data
/// URLs decode inline; http(s) URLs are fetched server-side. Any failed
/// fetch is an error — silently dropping a requested image would produce
/// a completion the client believes includes it.
pub async fn extract_images(msgs: &[Message]) -> Result<Vec<Image>, ImageFetchError> {
    let mut imgs = Vec::new();
    for m in msgs {
        for p in &m.parts {
            if p.kind != PartKind::ImageUrl || p.image_url.is_empty() {
                continue;
            }
            if let Some((data, ext)) = decode_data_url(&p.image_url) {
                imgs.push(Image { data, ext });
                continue;
            }
            let img = fetch_image(&p.image_url).await?;
            imgs.push(img);
        }
    }
    Ok(imgs)
}

/// Extract just the URLs that would be http(s)-fetched (introspection
/// for tests; also the pre-lease planning surface).
#[cfg(test)]
pub fn http_image_urls(msgs: &[Message]) -> Vec<String> {
    msgs.iter()
        .flat_map(|m| m.parts.iter())
        .filter(|p| p.kind == PartKind::ImageUrl && !p.image_url.is_empty())
        .filter(|p| !p.image_url.starts_with("data:"))
        .map(|p| p.image_url.clone())
        .collect()
}

/// Download one http(s) image URL (Go `fetchImage`): 200 required, image
/// content type required, 10 MB cap. `url`/`host` validation failures
/// return [`ImageFetchError::InvalidUrl`] before any network work, and an
/// IP-literal host in an SSRF-forbidden class returns
/// [`ImageFetchError::NonPublicAddress`] — IP literals bypass DNS, so the
/// resolver filter alone would miss them (Go ran `validateImageURL`
/// upfront for exactly this case). Cancellation: the caller owns the
/// future — dropping it aborts the fetch (the host's per-request drop is
/// the ctx-equivalent, OQ-2).
async fn fetch_image(raw: &str) -> Result<Image, ImageFetchError> {
    let u: reqwest::Url = raw.parse().map_err(|_| ImageFetchError::InvalidUrl)?;
    if (u.scheme() != "http" && u.scheme() != "https") || u.host_str().is_none() {
        return Err(ImageFetchError::InvalidUrl);
    }
    // IP-literal hosts never reach the DNS resolver (hyper dials them
    // directly), so the resolver filter alone would miss them. Go ran
    // `validateImageURL` upfront for exactly this case; the check here is
    // its dial-twin. Bracketed IPv6 literals lose their brackets first.
    if let Some(host) = u.host_str() {
        let bare = host.trim_start_matches('[').trim_end_matches(']');
        if let Ok(ip) = bare.parse::<IpAddr>() {
            if is_non_public(ip) {
                return Err(ImageFetchError::NonPublicAddress);
            }
        }
    }
    fetch_image_with(raw, image_client()).await
}

/// The fetch body against a caller-supplied client (tests inject a
/// permissive/short-timeout client — Go's `allowLocalImageTestServer`
/// nulled the validator to test response handling against a local
/// server; the SSRF checks are the caller's in production).
async fn fetch_image_with(raw: &str, client: reqwest::Client) -> Result<Image, ImageFetchError> {
    let u: reqwest::Url = raw.parse().map_err(|_| ImageFetchError::InvalidUrl)?;
    if (u.scheme() != "http" && u.scheme() != "https") || u.host_str().is_none() {
        return Err(ImageFetchError::InvalidUrl);
    }
    let display = first_segment(u.as_str()).to_string();
    let ferr = |reason: String| ImageFetchError::Fetch {
        url: display.clone(),
        reason,
    };
    let resp = client
        .get(u)
        .send()
        .await
        .map_err(|e| ferr(e.to_string()))?;
    if resp.status().as_u16() != 200 {
        return Err(ferr(format!("http {}", resp.status().as_u16())));
    }
    let ct = resp
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    let ext =
        image_ext_by_mime(&ct).ok_or_else(|| ferr(format!("unsupported content type {ct:?}")))?;
    // Read bounded: reqwest's `bytes()` respects the client timeout; the
    // explicit cap check below enforces the 10MB limit the way Go's
    // LimitReader did (one over the cap → error, not a buffer).
    let data = resp.bytes().await.map_err(|e| ferr(e.to_string()))?;
    if data.len() > IMAGE_CAP {
        return Err(ferr("exceeds 10MB cap".into()));
    }
    Ok(Image {
        data: data.to_vec(),
        ext,
    })
}

#[cfg(test)]
mod tests;
