//! Auth key reference resolution (contract §3/§4): `auth.keys` carries an
//! `env:VAR_NAME` reference that the host resolves at load time through an
//! **injected** resolver so the crate stays free of `std::env` (testability).
//!
//! Resolution outcome:
//! - `env:VAR` present      → the variable name (the host reads the value
//!   at request time; key material never passes through this crate)
//! - `env:VAR` missing      → typed error at registration time
//! - non-`env:` / malformed → typed error

use crate::manifest::PluginManifest;

/// Why auth reference resolution failed.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum AuthResolveError {
    /// `auth.keys` is not an `env:` reference (validation already rejects
    /// literals; this fires on other malformed shapes).
    #[error("auth.keys must be an `env:VAR_NAME` reference, got `{0}`")]
    NotEnvRef(String),
    /// The referenced environment variable is unset.
    #[error(
        "auth reference `{reference}` cannot be resolved: environment variable `{var}` is not set"
    )]
    MissingEnv {
        /// The raw reference, e.g. `env:MIMO2API_API_KEYS`.
        reference: String,
        /// The variable name after the `env:` prefix.
        var: String,
    },
}

/// Injected environment lookup: given a variable name, return its value.
/// The host injects `|name| std::env::var(name).ok()`; tests inject maps.
/// Generic over the callable (rather than `&dyn Fn`) so closures of any
/// lifetime satisfy the bound.
pub trait EnvResolver {
    /// Look up one variable's value; `None` = not set.
    fn lookup(&self, name: &str) -> Option<String>;
}

impl<F> EnvResolver for F
where
    F: Fn(&str) -> Option<String>,
{
    fn lookup(&self, name: &str) -> Option<String> {
        self(name)
    }
}

const ENV_PREFIX: &str = "env:";

/// Resolve one plugin's `auth.keys` reference against `resolver`.
///
/// Returns the variable name whose value the host must use as the plugin
/// key. Missing variables are registration-time errors with a clear message.
pub fn resolve_auth(
    manifest: &PluginManifest,
    resolver: impl EnvResolver,
) -> Result<String, AuthResolveError> {
    let reference = &manifest.auth.keys;
    let Some(var) = reference.strip_prefix(ENV_PREFIX) else {
        return Err(AuthResolveError::NotEnvRef(reference.clone()));
    };
    let var = var.trim();
    if var.is_empty() {
        return Err(AuthResolveError::NotEnvRef(reference.clone()));
    }
    if resolver.lookup(var).is_none() {
        return Err(AuthResolveError::MissingEnv {
            reference: reference.clone(),
            var: var.to_string(),
        });
    }
    Ok(var.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn manifest_with(keys: &str) -> PluginManifest {
        let mut m = crate::parse_manifest(
            r#"
manifestVersion = "0.1"
id = "dev.oxcost.mimo2api"
name = "mimo2api"
version = "1.0.0"
apiVersion = "v1"
license = "MIT"

[entry]
mode = "external"
port = 21223

[health]
path = "/healthz"
interval_secs = 30
timeout_secs = 5

[routing]
models = ["mimo2api/mimo-v2.6-pro"]
unknown_model = "pass"

[auth]
keys = "env:MIMO2API_API_KEYS"
"#,
        )
        .expect("fixture manifest must parse");
        m.auth.keys = keys.to_string();
        m
    }

    #[test]
    fn present_env_resolves_to_var_name() {
        let m = manifest_with("env:MIMO2API_API_KEYS");
        let vars = [("MIMO2API_API_KEYS".to_string(), "redacted".to_string())];
        let resolver = |name: &str| {
            vars.iter()
                .find(|(k, _)| k == name)
                .cloned()
                .map(|(_, v)| v)
        };
        assert_eq!(resolve_auth(&m, resolver).unwrap(), "MIMO2API_API_KEYS");
    }

    #[test]
    fn missing_env_is_typed_error() {
        let m = manifest_with("env:NOPE_KEY");
        let resolver = |_: &str| None;
        assert_eq!(
            resolve_auth(&m, resolver).unwrap_err(),
            AuthResolveError::MissingEnv {
                reference: "env:NOPE_KEY".into(),
                var: "NOPE_KEY".into()
            }
        );
    }

    #[test]
    fn literal_reference_is_rejected() {
        let m = manifest_with("file:/etc/keys");
        let resolver = |_: &str| Some("x".to_string());
        assert_eq!(
            resolve_auth(&m, resolver).unwrap_err(),
            AuthResolveError::NotEnvRef("file:/etc/keys".into())
        );
    }

    #[test]
    fn empty_var_name_is_rejected() {
        let m = manifest_with("env:");
        let resolver = |_: &str| Some("x".to_string());
        assert!(matches!(
            resolve_auth(&m, resolver),
            Err(AuthResolveError::NotEnvRef(_))
        ));
    }

    #[test]
    fn resolver_reporting_missing_surfaces_as_missing_env() {
        // The injected boundary decides presence, not this crate: a resolver
        // that cannot supply the variable means MissingEnv.
        let m = manifest_with("env:MIMO2API_API_KEYS");
        let resolver = |name: &str| (name == "OTHER").then(|| "v".to_string());
        assert!(matches!(
            resolve_auth(&m, resolver),
            Err(AuthResolveError::MissingEnv { .. })
        ));
    }
}
