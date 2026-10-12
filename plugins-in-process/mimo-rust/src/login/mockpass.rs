//! Mock passport for the login-chain TDD vector replay (Go MockPassport,
//! login_test.go). One wiremock server plays BOTH surfaces: the aistudio
//! upstream (base) and the account passport. Every request is recorded so
//! the DEV/OTP vectors can assert headers, cookies and form bodies.

use std::sync::{Arc, Mutex};

use wiremock::matchers::{method, path, query_param, query_param_is_missing};
use wiremock::{Mock, MockServer, ResponseTemplate};

/// One recorded incoming request (path + method + headers + body).
#[derive(Debug, Clone)]
pub(crate) struct Recorded {
    pub method: String,
    pub path: String,
    pub query: String,
    pub cookie: String,
    pub body: String,
    pub headers: Vec<(String, String)>,
}

impl Recorded {
    pub fn header(&self, name: &str) -> String {
        let lower = name.to_lowercase();
        self.headers
            .iter()
            .find(|(k, _)| k.to_lowercase() == lower)
            .map(|(_, v)| v.clone())
            .unwrap_or_default()
    }
}

#[derive(Debug)]
pub(crate) struct MockPassport {
    server: MockServer,
    recorded: Arc<Mutex<Vec<Recorded>>>,
}

impl MockPassport {
    /// Boot the server with the fallback recording mount: any unmatched
    /// request is captured and answered 404 — vectors mount their own
    /// expectations on top (wiremock tries the most recent mount first).
    pub async fn start() -> Self {
        let server = MockServer::start().await;
        let recorded: Arc<Mutex<Vec<Recorded>>> = Arc::new(Mutex::new(Vec::new()));
        let mp = Self {
            server,
            recorded: recorded.clone(),
        };
        let rec = recorded;
        Mock::given(wiremock::matchers::any())
            .respond_with(move |req: &wiremock::Request| -> ResponseTemplate {
                let headers: Vec<(String, String)> = req
                    .headers
                    .iter()
                    .map(|(k, v)| (k.to_string(), v.to_str().unwrap_or("").to_string()))
                    .collect();
                rec.lock().unwrap().push(Recorded {
                    method: req.method.to_string(),
                    path: req.url.path().to_string(),
                    query: req.url.query().unwrap_or("").to_string(),
                    cookie: headers
                        .iter()
                        .find(|(k, _)| k.eq_ignore_ascii_case("cookie"))
                        .map(|(_, v)| v.clone())
                        .unwrap_or_default(),
                    body: String::from_utf8_lossy(&req.body).to_string(),
                    headers,
                });
                ResponseTemplate::new(404)
            })
            .with_priority(255) // lowest: vector mounts take precedence
            .mount(&mp.server)
            .await;
        mp
    }

    /// The aistudio base URL (client base option).
    pub fn uri(&self) -> String {
        self.server.uri()
    }

    /// Everything recorded so far, in arrival order.
    pub fn recorded(&self) -> Vec<Recorded> {
        self.recorded.lock().unwrap().clone()
    }

    /// Recorded requests whose path is exactly `path_prefix` or begins with
    /// `path_prefix + "/"` (so "/pass/serviceLogin" does not swallow
    /// "/pass/serviceLoginAuth2").
    pub fn recorded_for(&self, path_prefix: &str) -> Vec<Recorded> {
        let with_slash = format!("{path_prefix}/");
        self.recorded()
            .into_iter()
            .filter(|r| r.path == path_prefix || r.path.starts_with(&with_slash))
            .collect()
    }

    /// Mount `template` on an exact path (optionally with one query param)
    /// with a responder that records the request before answering — vector
    /// mounts must be visible to `recorded_for` too, not only the 404
    /// fallback.
    async fn mount_recording(
        &self,
        method_req: &str,
        path_req: &str,
        query: Option<(&str, &str)>,
        template: ResponseTemplate,
    ) {
        self.mount_recording_n(method_req, path_req, query, template, None)
            .await;
    }

    /// `max_hits` caps how many requests the mount answers before it stops
    /// matching (None = uncapped).
    async fn mount_recording_n(
        &self,
        method_req: &str,
        path_req: &str,
        query: Option<(&str, &str)>,
        template: ResponseTemplate,
        max_hits: Option<u64>,
    ) {
        let rec = self.recorded.clone();
        let template = template.clone();
        let mut mock = Mock::given(method(method_req)).and(path(path_req));
        if let Some((param, value)) = query {
            mock = mock.and(query_param(param, value));
        }
        let mock = mock.respond_with(move |req: &wiremock::Request| -> ResponseTemplate {
            let headers: Vec<(String, String)> = req
                .headers
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_str().unwrap_or("").to_string()))
                .collect();
            rec.lock().unwrap().push(Recorded {
                method: req.method.to_string(),
                path: req.url.path().to_string(),
                query: req.url.query().unwrap_or("").to_string(),
                cookie: headers
                    .iter()
                    .find(|(k, _)| k.eq_ignore_ascii_case("cookie"))
                    .map(|(_, v)| v.clone())
                    .unwrap_or_default(),
                body: String::from_utf8_lossy(&req.body).to_string(),
                headers,
            });
            template.clone()
        });
        let mock = match max_hits {
            Some(n) => mock.up_to_n_times(n),
            None => mock,
        };
        mock.mount(&self.server).await;
    }

    /// Convenience: mount a one-shot 3xx redirect (first hit only).
    pub async fn mount_redirect_once(
        &self,
        method_req: &str,
        path_req: &str,
        status: u16,
        location: &str,
    ) {
        let template =
            ResponseTemplate::new(status).insert_header("Location", location.to_string());
        self.mount_recording_n(method_req, path_req, None, template, Some(1))
            .await;
    }

    /// Convenience: mount a simple JSON response on an exact path.
    pub async fn mount_json(&self, method_req: &str, path_req: &str, status: u16, body: &str) {
        let template = ResponseTemplate::new(status).set_body_string(body.to_string());
        self.mount_recording(method_req, path_req, None, template)
            .await;
    }

    /// Convenience: mount a 3xx redirect on an exact path.
    pub async fn mount_redirect(
        &self,
        method_req: &str,
        path_req: &str,
        status: u16,
        location: &str,
    ) {
        let template =
            ResponseTemplate::new(status).insert_header("Location", location.to_string());
        self.mount_recording(method_req, path_req, None, template)
            .await;
    }

    /// Convenience: mount a JSON response on a path with one query param
    /// matched.
    #[allow(dead_code)]
    pub async fn mount_json_qp(
        &self,
        method_req: &str,
        path_req: &str,
        param: &str,
        value: &str,
        status: u16,
        body: &str,
    ) {
        let template = ResponseTemplate::new(status).set_body_string(body.to_string());
        self.mount_recording(method_req, path_req, Some((param, value)), template)
            .await;
    }

    /// Mount a JSON response on an exact path for requests NOT carrying the
    /// given query param (e.g. the queryless primed login page vs the
    /// `_json=true` session-flow hit on the same path), with optional
    /// Set-Cookie plants.
    pub async fn mount_json_qp_missing_cookies(
        &self,
        method_req: &str,
        path_req: &str,
        param: &str,
        status: u16,
        body: &str,
        cookies: &[(&str, &str)],
    ) {
        let mut template = ResponseTemplate::new(status).set_body_string(body.to_string());
        for (name, value) in cookies {
            template =
                template.append_header("Set-Cookie", format!("{name}={value}; Path=/").as_str());
        }
        let rec = self.recorded.clone();
        Mock::given(method(method_req))
            .and(path(path_req))
            .and(query_param_is_missing(param))
            .respond_with(move |req: &wiremock::Request| -> ResponseTemplate {
                let headers: Vec<(String, String)> = req
                    .headers
                    .iter()
                    .map(|(k, v)| (k.to_string(), v.to_str().unwrap_or("").to_string()))
                    .collect();
                rec.lock().unwrap().push(Recorded {
                    method: req.method.to_string(),
                    path: req.url.path().to_string(),
                    query: req.url.query().unwrap_or("").to_string(),
                    cookie: headers
                        .iter()
                        .find(|(k, _)| k.eq_ignore_ascii_case("cookie"))
                        .map(|(_, v)| v.clone())
                        .unwrap_or_default(),
                    body: String::from_utf8_lossy(&req.body).to_string(),
                    headers,
                });
                template.clone()
            })
            .mount(&self.server)
            .await;
    }

    /// Convenience: mount a redirect carrying Set-Cookie plants.
    pub async fn mount_redirect_cookies(
        &self,
        method_req: &str,
        path_req: &str,
        status: u16,
        location: &str,
        cookies: &[(&str, &str)],
    ) {
        let mut template =
            ResponseTemplate::new(status).insert_header("Location", location.to_string());
        for (name, value) in cookies {
            template =
                template.append_header("Set-Cookie", format!("{name}={value}; Path=/").as_str());
        }
        self.mount_recording(method_req, path_req, None, template)
            .await;
    }

    /// Convenience: mount a JSON response carrying Set-Cookie plants.
    pub async fn mount_json_cookies(
        &self,
        method_req: &str,
        path_req: &str,
        status: u16,
        body: &str,
        cookies: &[(&str, &str)],
    ) {
        let mut template = ResponseTemplate::new(status).set_body_string(body.to_string());
        for (name, value) in cookies {
            template =
                template.append_header("Set-Cookie", format!("{name}={value}; Path=/").as_str());
        }
        self.mount_recording(method_req, path_req, None, template)
            .await;
    }
}
