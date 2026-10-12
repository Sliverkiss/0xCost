//! Manifest validation tests, one per §3 rule (synthetic manifests only).

use oxcost_contract::{
    parse_manifest, validate, Capability, CapabilitySet, EntryMode, ManifestError,
    UnknownModelPolicy, ValidationError,
};

/// Valid manifest with sensible defaults for mutation in tests.
fn valid_toml() -> String {
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
models = ["mimo2api/mimo-v2.6-pro", "mimo2api/mimo-v2.6-flash"]
unknown_model = "pass"

[auth]
keys = "env:MIMO2API_API_KEYS"
"#
    .to_string()
}

fn parse_validate(text: &str) -> Result<(), ValidationError> {
    let manifest = parse_manifest(text).unwrap_or_else(|e: ManifestError| panic!("{e}"));
    validate(&manifest)
}

fn expect_err(text: &str) -> ValidationError {
    parse_validate(text).expect_err("validation should fail")
}

// --- Rule 1: id format -------------------------------------------------

#[test]
fn id_rejects_uppercase_and_single_segment() {
    let upper = valid_toml().replace("dev.oxcost.mimo2api", "Dev.OxCost.mimo2api");
    assert_eq!(
        expect_err(&upper),
        ValidationError::InvalidId("Dev.OxCost.mimo2api".into())
    );

    let single = valid_toml().replace("dev.oxcost.mimo2api", "mimo2api");
    // Single segment: id invalid AND name mismatch; id rule fires first.
    assert!(matches!(expect_err(&single), ValidationError::InvalidId(_)));

    let digit_segment = valid_toml().replace("dev.oxcost.mimo2api", "dev.1x.mimo2api");
    parse_validate(&digit_segment).expect("digit-start segment is legal");
}

#[test]
fn id_rejects_empty_and_dot_dot() {
    let empty_seg = valid_toml().replace("dev.oxcost.mimo2api", "dev..mimo2api");
    assert!(matches!(
        expect_err(&empty_seg),
        ValidationError::InvalidId(_)
    ));
}

#[test]
fn name_must_match_last_id_segment() {
    let mismatch = valid_toml().replace("name = \"mimo2api\"", "name = \"mimo\"");
    assert_eq!(
        expect_err(&mismatch),
        ValidationError::NameIdMismatch {
            name: "mimo".into(),
            id: "dev.oxcost.mimo2api".into()
        }
    );
}

// --- Rule 2: version sets ----------------------------------------------

#[test]
fn versions_must_be_supported() {
    let bad_manifest = valid_toml().replace("\"0.1\"", "\"9.9\"");
    assert_eq!(
        expect_err(&bad_manifest),
        ValidationError::UnsupportedManifestVersion("9.9".into())
    );

    let bad_api = valid_toml().replace("apiVersion = \"v1\"", "apiVersion = \"v2\"");
    assert_eq!(
        expect_err(&bad_api),
        ValidationError::UnsupportedApiVersion("v2".into())
    );
}

// --- Rule 3: models ----------------------------------------------------

#[test]
fn models_must_be_nonempty() {
    let empty = valid_toml().replace(
        "models = [\"mimo2api/mimo-v2.6-pro\", \"mimo2api/mimo-v2.6-flash\"]",
        "models = []",
    );
    assert_eq!(expect_err(&empty), ValidationError::EmptyModels);
}

#[test]
fn models_reject_duplicates() {
    let dup = valid_toml().replace("\"mimo2api/mimo-v2.6-flash\"", "\"mimo2api/mimo-v2.6-pro\"");
    assert_eq!(
        expect_err(&dup),
        ValidationError::DuplicateModel("mimo2api/mimo-v2.6-pro".into())
    );
}

#[test]
fn model_prefix_must_equal_plugin_name() {
    let bad_prefix = valid_toml()
        .replace("mimo2api/mimo-v2.6-pro", "other/mimo-v2.6-pro")
        .replace("mimo2api/mimo-v2.6-flash", "other/mimo-v2.6-flash");
    assert_eq!(
        expect_err(&bad_prefix),
        ValidationError::ModelPrefixMismatch {
            model: "other/mimo-v2.6-pro".into(),
            prefix: "other".into(),
            name: "mimo2api".into()
        }
    );
}

// --- Rule 4: auth / secrets --------------------------------------------

#[test]
fn auth_rejects_non_env_reference() {
    let not_env = valid_toml().replace("env:MIMO2API_API_KEYS", "file:/etc/keys");
    assert_eq!(
        expect_err(&not_env),
        ValidationError::AuthNotEnvRef("file:/etc/keys".into())
    );
}

#[test]
fn auth_rejects_sk_literal() {
    let literal = valid_toml().replace("env:MIMO2API_API_KEYS", "sk-abcdef1234567890");
    assert_eq!(expect_err(&literal), ValidationError::LiteralSecret);
}

#[test]
fn auth_rejects_dense_token_literal() {
    // 40 hex chars: matches the `^[A-Za-z0-9_\-]{32,}$` heuristic.
    let literal = valid_toml().replace(
        "env:MIMO2API_API_KEYS",
        "0123456789abcdef0123456789abcdef01234567",
    );
    assert_eq!(expect_err(&literal), ValidationError::LiteralSecret);
}

#[test]
fn auth_rejects_secret_hidden_behind_env_prefix() {
    // `env:sk-...`: still secret material in the manifest.
    let hidden = valid_toml().replace("env:MIMO2API_API_KEYS", "env:sk-1234567890abcdef");
    assert_eq!(expect_err(&hidden), ValidationError::LiteralSecret);
}

// --- Rule 5: entry mode fields ------------------------------------------

#[test]
fn host_spawn_requires_cmd() {
    let empty_cmd = valid_toml()
        .replace("mode = \"external\"", "mode = \"host-spawn\"")
        .replace("port = 21223", "cmd = []");
    assert_eq!(
        expect_err(&empty_cmd),
        ValidationError::MissingEntryField {
            mode: "host-spawn",
            field: "cmd"
        }
    );

    let ok = valid_toml()
        .replace("mode = \"external\"", "mode = \"host-spawn\"")
        .replace("port = 21223", "cmd = [\"./mimo2api\", \"serve\"]");
    parse_validate(&ok).expect("host-spawn with cmd is valid");
}

#[test]
fn external_requires_port() {
    let no_port = valid_toml().replace("port = 21223", "");
    assert_eq!(
        expect_err(&no_port),
        ValidationError::MissingEntryField {
            mode: "external",
            field: "port"
        }
    );
}

#[test]
fn remote_requires_http_url_and_forbids_health() {
    let remote_ok = r#"
manifestVersion = "0.1"
id = "dev.oxcost.example-remote"
name = "example-remote"
version = "0.1.0"
apiVersion = "v1"
license = "MIT"

[entry]
mode = "remote"
url = "https://relay.example.com/v1"

[routing]
models = ["example-remote/example-chat"]
unknown_model = "reject"

[auth]
keys = "env:EXAMPLE_REMOTE_API_KEYS"
"#;
    parse_validate(remote_ok).expect("valid remote manifest");

    let no_url = remote_ok.replace("url = \"https://relay.example.com/v1\"", "");
    assert_eq!(
        expect_err(&no_url),
        ValidationError::MissingEntryField {
            mode: "remote",
            field: "url"
        }
    );

    let bad_scheme = remote_ok.replace("https://", "ftp://");
    assert_eq!(
        expect_err(&bad_scheme),
        ValidationError::InvalidEntryUrl("ftp://relay.example.com/v1".into())
    );

    let with_health = format!(
        "{remote_ok}\n[health]\npath = \"/healthz\"\ninterval_secs = 30\ntimeout_secs = 5\n"
    );
    assert_eq!(
        expect_err(&with_health),
        ValidationError::HealthForbiddenForRemote
    );
}

// --- Rule 6: capability dependency --------------------------------------

#[test]
fn batches_requires_files() {
    let with_batches = format!(
        "{}\n[capabilities]\nchat = true\nbatches = true\n",
        valid_toml()
    );
    assert_eq!(
        expect_err(&with_batches),
        ValidationError::MissingCapabilityDependency {
            cap: "batches",
            dep: "files"
        }
    );

    let with_both = format!(
        "{}\n[capabilities]\nchat = true\nbatches = true\nfiles = true\n",
        valid_toml()
    );
    parse_validate(&with_both).expect("batches+files is valid");
}

// --- Rule 7: unknown_model ----------------------------------------------

#[test]
fn unknown_policy_parses_three_forms() {
    assert_eq!(
        UnknownModelPolicy::parse("pass"),
        Some(UnknownModelPolicy::Pass)
    );
    assert_eq!(
        UnknownModelPolicy::parse("reject"),
        Some(UnknownModelPolicy::Reject)
    );
    assert_eq!(
        UnknownModelPolicy::parse("fallback=mimo2api/mimo-v2.6-pro"),
        Some(UnknownModelPolicy::Fallback(
            "mimo2api/mimo-v2.6-pro".into()
        ))
    );
    assert_eq!(UnknownModelPolicy::parse("pas"), None);
    assert_eq!(UnknownModelPolicy::parse("fallback="), None);
    assert_eq!(UnknownModelPolicy::parse("fallback"), None);

    // Round-trip through the wire string.
    let wire: String = UnknownModelPolicy::Fallback("m".into()).into();
    assert_eq!(wire, "fallback=m");
}

// --- Capability model ---------------------------------------------------

#[test]
fn capability_defaults_are_chat_only() {
    let manifest = parse_manifest(&valid_toml()).expect("parse");
    let set = CapabilitySet::from_section(&manifest.capabilities);
    assert!(set.contains(Capability::Chat));
    let declared: Vec<Capability> = set.iter().collect();
    assert_eq!(declared, vec![Capability::Chat]);
}

#[test]
fn experimental_classification() {
    let experimental = [
        Capability::Responses,
        Capability::Files,
        Capability::Batches,
        Capability::Realtime,
    ];
    for cap in experimental {
        assert_eq!(cap.tier(), oxcost_contract::Tier::Experimental);
    }
    for cap in [
        Capability::Chat,
        Capability::Embeddings,
        Capability::Images,
        Capability::AudioSpeech,
        Capability::AudioTranscription,
        Capability::AudioTranslations,
        Capability::Video,
        Capability::Moderations,
        Capability::WebSearch,
    ] {
        assert_eq!(cap.tier(), oxcost_contract::Tier::Core);
    }
    assert_eq!(Capability::ALL.len(), 13);
}

#[test]
fn capability_set_roundtrip_and_names() {
    let mut set = CapabilitySet::none();
    for cap in [
        Capability::Chat,
        Capability::Files,
        Capability::Batches,
        Capability::Responses,
    ] {
        set.insert(cap);
    }
    let section = set.to_section();
    let back = CapabilitySet::from_section(&section);
    assert_eq!(set, back);
    // Core first, then experimental (in Capability::ALL order).
    assert_eq!(set.names(), "chat,responses,files,batches");

    let unmet = set.unmet_dependencies();
    assert!(unmet.is_empty(), "batches+files has no unmet deps");
}

#[test]
fn version_negotiation_supports() {
    let manifest = parse_manifest(&valid_toml()).expect("parse");
    assert!(oxcost_contract::supports(&manifest).is_ok());

    let future = {
        let mut m = manifest.clone();
        m.api_version = "v2".into();
        m
    };
    assert_eq!(
        oxcost_contract::supports(&future),
        Err(oxcost_contract::Unsupported::ApiVersion("v2".into()))
    );

    let unknown_format = {
        let mut m = manifest;
        m.manifest_version = "0.2".into();
        m
    };
    assert_eq!(
        oxcost_contract::supports(&unknown_format),
        Err(oxcost_contract::Unsupported::ManifestVersion("0.2".into()))
    );
}

#[test]
fn entry_mode_parses_kebab_case() {
    let manifest = parse_manifest(&valid_toml()).expect("parse");
    assert_eq!(manifest.entry.mode, EntryMode::External);
    assert_eq!(manifest.entry.port, Some(21223));
    assert!(manifest.limits.is_none());
    assert!(manifest.source.is_none());
}

#[test]
fn malformed_toml_is_a_parse_error() {
    let err = parse_manifest("not [ valid toml").expect_err("should fail");
    assert!(!err.to_string().is_empty());
}

#[test]
fn unknown_top_level_field_rejected() {
    let extra = format!("{}\nbogus_field = 1\n", valid_toml());
    assert!(parse_manifest(&extra).is_err());
}
