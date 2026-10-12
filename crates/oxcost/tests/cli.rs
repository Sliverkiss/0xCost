//! Integration tests: run the oxcost binary against real fixture dirs.

use assert_cmd::Command;
use predicates::prelude::*;

/// Repo's checked-in example (valid, remote mode), relative to the crate.
const VALID_DIR: &str = "../../plugins";
/// Invalid-manifest fixture built at test time (duplicate model).
const INVALID_TOML: &str = r#"
manifestVersion = "0.1"
id = "dev.oxcost.badplugin"
name = "badplugin"
version = "0.1.0"
apiVersion = "v1"
license = "MIT"

[entry]
mode = "external"
port = 21224

[health]
path = "/healthz"
interval_secs = 30
timeout_secs = 5

[routing]
models = ["badplugin/m", "badplugin/m"]
unknown_model = "pass"

[auth]
keys = "env:BADPLUGIN_API_KEYS"
"#;

fn bin() -> Command {
    Command::cargo_bin("oxcost").expect("binary built")
}

#[test]
fn example_remote_registers_and_exits_zero() {
    bin()
        .arg("--plugins")
        .arg(VALID_DIR)
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .env("RUST_LOG", "off")
        .assert()
        .success()
        .stdout(predicate::str::contains(
            "OxCost host — plugin registration table",
        ))
        .stdout(predicate::str::contains("dev.oxcost.example-remote"))
        .stdout(predicate::str::contains("Registered"))
        // Capabilities from the example manifest: chat + web_search.
        .stdout(predicate::str::contains("chat,web_search"));
}

#[test]
fn invalid_manifest_reports_specific_validation_error() {
    let dir = std::env::temp_dir().join("oxcost-it-invalid");
    let plugin_dir = dir.join("badplugin");
    std::fs::create_dir_all(&plugin_dir).expect("mkdir");
    std::fs::write(plugin_dir.join("plugin.toml"), INVALID_TOML).expect("write fixture");

    bin()
        .arg("--plugins")
        .arg(&dir)
        .env("RUST_LOG", "off")
        .assert()
        // Load failures are reported, not fatal: exit 0 with an empty table.
        .success()
        .stdout(predicate::str::contains("(no plugins registered)"))
        .stderr(predicate::str::contains("rejected plugin `badplugin`"))
        .stderr(predicate::str::contains(
            "duplicate model `badplugin/m` in routing.models",
        ));

    std::fs::remove_dir_all(&dir).ok();
}

#[test]
fn bad_cli_args_exit_two() {
    bin()
        .arg("--wat")
        .env("RUST_LOG", "off")
        .assert()
        .failure()
        .code(2)
        .stderr(predicate::str::contains("unknown argument"));
}
