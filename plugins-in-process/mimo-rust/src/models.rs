//! Pure adapter: static model catalog, normalization, fastchat routing,
//! per-channel char caps. 1:1 port of Go `internal/adapter/models.go`.

/// Static catalog floor (Go `DefaultModels`; served ids in live use are the
/// three v2.6 chat models, but the floor keeps alias resolution for older
/// ids — PORT_DESIGN §3.2).
pub const DEFAULT_MODELS: &[&str] = &[
    "mimo-v2.6-pro-ultraspeed-studio",
    "mimo-v2.6-pro",
    "mimo-v2.6-flash",
    "mimo-v2.5-pro",
    "mimo-v2.5",
    "mimo-v2.1-pro",
    "mimo-v2.1-pro-preview",
    "mimo-v2-pro",
    "mimo-v2-flash",
];

/// The three v1-served chat models (the live oracle proves daily; all other
/// floor ids resolve but are not advertised — `models()` stays consistent
/// with the manifest's "serve what you declare" rule).
pub const SERVED_MODELS: &[&str] = &[
    "mimo-v2.6-pro",
    "mimo-v2.6-flash",
    "mimo-v2.6-pro-ultraspeed-studio",
];

/// Ultraspeed ids route through the fastchat channel (Go routing rule).
pub fn is_fastchat_model(model: &str) -> bool {
    model.to_ascii_lowercase().contains("ultraspeed")
}

/// Model resolution outcome.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NormalizedModel {
    /// Resolved upstream model id.
    pub model: String,
    /// Route via `/fastchat/open-apis/bot/chat`.
    pub fastchat: bool,
}

/// `ModelNotFound` carries the requested id and available ids for the client
/// error message (Go `ErrModelNotFound`).
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("model {requested:?} not found; available models: {available:?}")]
pub struct ModelNotFound {
    pub requested: String,
    pub available: Vec<String>,
}

/// NormalizeModel: ""/"auto" → default; exact match; `-studio` suffix alias;
/// else Err listing available ids (sorted). 1:1 port of the Go function.
pub fn normalize_model(
    requested: &str,
    default_model: &str,
    catalog: Option<&[String]>,
) -> Result<NormalizedModel, ModelNotFound> {
    let floor: Vec<String> = DEFAULT_MODELS.iter().map(|s| s.to_string()).collect();
    let catalog = catalog.unwrap_or(&floor);
    if catalog.is_empty() {
        return Err(ModelNotFound {
            requested: requested.to_string(),
            available: vec![],
        });
    }
    let resolve = |id: &str| -> Option<&String> {
        catalog.iter().find(|m| m.as_str() == id).or_else(|| {
            id.strip_suffix("-studio")
                .and_then(|trimmed| catalog.iter().find(|m| m.as_str() == trimmed))
        })
    };
    let trimmed_req = requested.trim();
    let model: &String = if trimmed_req.is_empty() || trimmed_req.eq_ignore_ascii_case("auto") {
        match resolve(default_model) {
            Some(m) => m,
            None => &catalog[0],
        }
    } else {
        match resolve(trimmed_req) {
            Some(m) => m,
            None => {
                let mut available = catalog.to_vec();
                available.sort();
                return Err(ModelNotFound {
                    requested: requested.to_string(),
                    available,
                });
            }
        }
    };
    Ok(NormalizedModel {
        fastchat: is_fastchat_model(model),
        model: model.clone(),
    })
}

/// Per-channel query char cap (Go `MaxQueryChars`): 50,000 fastchat /
/// 102,400 regular, counted on the composed query text.
pub fn max_query_chars(model: &str) -> usize {
    if is_fastchat_model(model) {
        50_000
    } else {
        102_400
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn normalize(requested: &str) -> Result<NormalizedModel, ModelNotFound> {
        normalize_model(requested, "mimo-v2.6-pro", None)
    }

    #[test]
    fn empty_and_auto_resolve_to_default() {
        for req in ["", "auto", " AUTO "] {
            let m = normalize(req).unwrap();
            assert_eq!(m.model, "mimo-v2.6-pro");
            assert!(!m.fastchat);
        }
    }

    #[test]
    fn exact_match_and_fastchat_flag() {
        let m = normalize("mimo-v2.6-pro-ultraspeed-studio").unwrap();
        assert!(m.fastchat);
        let m = normalize("mimo-v2.6-flash").unwrap();
        assert!(!m.fastchat);
    }

    #[test]
    fn studio_suffix_alias() {
        // mimo-v2.6-pro-studio → mimo-v2.6-pro (alias normalization)
        let m = normalize("mimo-v2.6-pro-studio").unwrap();
        assert_eq!(m.model, "mimo-v2.6-pro");
        let m = normalize("mimo-v2-pro-studio").unwrap();
        assert_eq!(m.model, "mimo-v2-pro");
    }

    #[test]
    fn unknown_model_lists_available_sorted() {
        let err = normalize("foo").unwrap_err();
        assert_eq!(err.requested, "foo");
        assert!(err.available.contains(&"mimo-v2.6-pro".to_string()));
        let mut sorted = err.available.clone();
        sorted.sort();
        assert_eq!(err.available, sorted);
    }

    #[test]
    fn caps_per_channel() {
        assert_eq!(max_query_chars("mimo-v2.6-pro"), 102_400);
        assert_eq!(max_query_chars("mimo-v2.6-pro-ultraspeed-studio"), 50_000);
    }

    #[test]
    fn empty_catalog_fails_closed() {
        let err = normalize_model("x", "d", Some(&[])).unwrap_err();
        assert!(err.available.is_empty());
    }

    #[test]
    fn default_not_in_catalog_falls_back_to_first() {
        let catalog = vec!["other-model".to_string()];
        let m = normalize_model("auto", "missing", Some(&catalog)).unwrap();
        assert_eq!(m.model, "other-model");
    }
}
