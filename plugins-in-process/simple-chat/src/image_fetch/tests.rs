//! Go test port: `image_ctx_test.go` — the SSRF IP classification table,
//! scheme validation, and (offline-substituted) fetch behavior tests.
//!
//! The Go tests used `httptest` + a stubbed validator to exercise the
//! live fetch; the offline port keeps the pure classification tests
//! verbatim and replaces the network fixtures with reqwest against a
//! local ephemeral listener — `127.0.0.1` is a *non-public* address, so
//! the SSRF-guarded production client would reject it; the tests use a
//! permissive client (`fetch_image_with`) to drive the response-shape
//! logic (non-200 / wrong content type / oversize), exactly what Go's
//! `allowLocalImageTestServer` achieved by nulling the validator.

use std::net::IpAddr;
use std::time::Duration;

use serde_json::json;

use super::*;
use crate::openai::{ContentPart, Message};

// ---- TestValidateImageURLRejectsPrivateAddresses (pure) ----

#[test]
fn validate_image_url_rejects_private_addresses() {
    for raw in [
        "http://127.0.0.1/image.png",
        "http://10.0.0.1/image.png",
        "http://192.168.1.1/image.png",
        "http://172.16.0.9/image.png",
        "http://[::1]/image.png",
        "http://169.254.169.254/latest/meta-data/",
        "http://[fe80::1]/image.png",
        "http://[fc00::1]/image.png",
        "http://[ff02::1]/image.png",
        "http://[ff01::1]/image.png",
        "http://0.0.0.0/image.png",
        "http://[::]/image.png",
    ] {
        let u: reqwest::Url = raw.parse().unwrap();
        let host = u.host_str().unwrap();
        let ip: IpAddr = host.trim_matches(['[', ']']).parse().unwrap();
        assert!(is_non_public(ip), "{raw}: private address was accepted");
    }
}

#[test]
fn public_addresses_pass() {
    for raw in [
        "8.8.8.8",
        "1.1.1.1",
        "93.184.216.34",
        "2606:4700::6810:84e4",
    ] {
        let ip: IpAddr = raw.parse().unwrap();
        assert!(!is_non_public(ip), "{raw}: public address rejected");
    }
}

// ---- TestValidateImageURLRequiresHTTP (offline form) ----

#[test]
fn fetch_rejects_non_http_schemes() {
    // file:// and gopher:// URLs parse but never pass the scheme gate;
    // data: URLs were already handled inline by extract_images.
    for raw in [
        "file:///etc/passwd",
        "gopher://127.0.0.1:6379/INFO",
        "ftp://x/y.png",
    ] {
        let err = tokio_test_block_on(async { fetch_image_with(raw, permissive_client()).await })
            .unwrap_err();
        assert_eq!(err, ImageFetchError::InvalidUrl, "{raw}");
    }
}

// ---- response-shape fixtures (Go TestExtractImagesBadResponsesError) ----

/// A tiny single-response HTTP server on an ephemeral local port: writes
/// the given status/headers/body raw, then closes. No axum/hyper dev-dep
/// — the responses are fixed bytes.
async fn one_shot_server(status: &str, headers: &[(&str, &str)], body: &'static [u8]) -> u16 {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let status = status.to_string();
    let headers: Vec<(String, String)> = headers
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
    tokio::spawn(async move {
        if let Ok((mut sock, _)) = listener.accept().await {
            use tokio::io::AsyncWriteExt;
            let mut resp = format!("{status}\r\nContent-Length: {}\r\n", body.len());
            for (k, v) in &headers {
                resp.push_str(&format!("{k}: {v}\r\n"));
            }
            resp.push_str("Connection: close\r\n\r\n");
            let _ = sock.write_all(resp.as_bytes()).await;
            let _ = sock.write_all(body).await;
        }
    });
    port
}

/// Go `allowLocalImageTestServer`: a client with the SSRF layers
/// disabled so the response-shape logic is testable against local
/// fixtures.
fn permissive_client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(Duration::from_secs(5))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap()
}

fn tokio_test_block_on<F: std::future::Future>(fut: F) -> F::Output {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(fut)
}

#[tokio::test]
async fn extract_data_url_via_extract_images() {
    let msgs = vec![Message {
        role: "user".into(),
        content: String::new(),
        parts: vec![ContentPart {
            kind: PartKind::ImageUrl,
            image_url: "data:image/png;base64,aGVsbG8=".into(),
            text: String::new(),
        }],
    }];
    let imgs = extract_images(&msgs).await.unwrap();
    assert_eq!(imgs.len(), 1);
    assert_eq!(imgs[0].data, b"hello");
    assert_eq!(imgs[0].ext, "png");
}

#[tokio::test]
async fn fetch_image_happy_path() {
    let png: &'static [u8] = b"pngbytes";
    let port = one_shot_server("HTTP/1.1 200 OK", &[("Content-Type", "image/png")], png).await;
    let img = fetch_image_with(
        &format!("http://127.0.0.1:{port}/photo.png"),
        permissive_client(),
    )
    .await
    .unwrap();
    assert_eq!(img.data, png);
    assert_eq!(img.ext, "png");
}

#[tokio::test]
async fn bad_responses_error() {
    // Non-200.
    let port = one_shot_server(
        "HTTP/1.1 404 Not Found",
        &[("Content-Type", "image/png")],
        b"",
    )
    .await;
    let err = fetch_image_with(
        &format!("http://127.0.0.1:{port}/gone"),
        permissive_client(),
    )
    .await
    .unwrap_err();
    match &err {
        ImageFetchError::Fetch { reason, .. } => assert!(reason.contains("404"), "{err}"),
        other => panic!("expected Fetch, got {other:?}"),
    }

    // Wrong content type.
    let port = one_shot_server(
        "HTTP/1.1 200 OK",
        &[("Content-Type", "text/html")],
        b"<html>",
    )
    .await;
    let err = fetch_image_with(
        &format!("http://127.0.0.1:{port}/evil.html"),
        permissive_client(),
    )
    .await
    .unwrap_err();
    match &err {
        ImageFetchError::Fetch { reason, .. } => {
            assert!(reason.contains("unsupported content type"), "{err}")
        }
        other => panic!("expected Fetch, got {other:?}"),
    }

    // Missing content type entirely.
    let port = one_shot_server("HTTP/1.1 200 OK", &[], b"x").await;
    assert!(fetch_image_with(
        &format!("http://127.0.0.1:{port}/noct"),
        permissive_client()
    )
    .await
    .is_err());
}

#[tokio::test]
async fn oversize_capped() {
    // Over the cap: the server claims cap+2 bytes but sends only a few
    // with Connection: close — reqwest surfaces the short body as an
    // error OR the length check fires on what arrived; either way the
    // fetch must fail, never succeed truncated (Go's LimitReader
    // one-over-the-cap semantics).
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move {
        if let Ok((mut sock, _)) = listener.accept().await {
            use tokio::io::AsyncWriteExt;
            let resp = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: image/png\r\nContent-Length: {}\r\nConnection: close\r\n\r\nPNG",
                IMAGE_CAP + 2
            );
            let _ = sock.write_all(resp.as_bytes()).await;
        }
    });
    let res = fetch_image_with(
        &format!("http://127.0.0.1:{port}/huge"),
        permissive_client(),
    )
    .await;
    assert!(res.is_err(), "oversize/short image must error, got {res:?}");
}

#[tokio::test]
async fn ssrf_guarded_client_rejects_local_host() {
    // The PRODUCTION path must refuse 127.0.0.1 — IP-literal check +
    // resolver filter (Go TestValidateImageURLRejectsPrivateAddresses's
    // dial-level twin). A listener exists to prove the block is ours, not
    // a refused connection.
    let png: &'static [u8] = b"pngbytes";
    let port = one_shot_server("HTTP/1.1 200 OK", &[("Content-Type", "image/png")], png).await;
    let err = fetch_image(&format!("http://127.0.0.1:{port}/p.png"))
        .await
        .unwrap_err();
    match err {
        ImageFetchError::NonPublicAddress => {}
        other => panic!("expected NonPublicAddress, got {other:?}"),
    }
}

#[tokio::test]
async fn query_string_never_in_error_url() {
    let port = one_shot_server(
        "HTTP/1.1 404 Not Found",
        &[("Content-Type", "image/png")],
        b"",
    )
    .await;
    let err = fetch_image_with(
        &format!("http://127.0.0.1:{port}/p.png?token=sekrit"),
        permissive_client(),
    )
    .await
    .unwrap_err();
    match &err {
        ImageFetchError::Fetch { url, .. } => {
            assert!(!url.contains("sekrit"), "token leaked: {url}");
            assert!(url.contains("/p.png"), "{url}");
        }
        other => panic!("expected Fetch, got {other:?}"),
    }
}

#[test]
fn http_image_urls_filters_data_urls() {
    let msgs = vec![Message {
        role: "user".into(),
        content: String::new(),
        parts: vec![
            ContentPart {
                kind: PartKind::ImageUrl,
                image_url: "data:image/png;base64,aA==".into(),
                text: String::new(),
            },
            ContentPart {
                kind: PartKind::ImageUrl,
                image_url: "https://example.com/a.png".into(),
                text: String::new(),
            },
            ContentPart {
                kind: PartKind::Text,
                text: "not an image".into(),
                image_url: String::new(),
            },
        ],
    }];
    let urls = http_image_urls(&msgs);
    assert_eq!(urls, vec!["https://example.com/a.png"]);
}

// A JSON-level sanity guard on the message plumbing (guards the
// openai::Message → fetch seam).
#[test]
fn message_parts_from_json_shape() {
    let v = json!({
        "model": "deepseek-flash",
        "messages": [{"role": "user", "content": [
            {"type": "image_url", "image_url": {"url": "https://example.com/a.png"}}
        ]}]
    });
    let req = crate::openai::parse_request(&v).unwrap();
    assert_eq!(
        http_image_urls(&req.messages),
        vec!["https://example.com/a.png"]
    );
}
