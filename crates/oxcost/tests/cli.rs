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

// ---- --config (R1W1 A2) ----

#[test]
fn bad_config_file_exits_two_with_line_numbers() {
    let dir = std::env::temp_dir().join("oxcost-it-cfg");
    std::fs::create_dir_all(&dir).expect("mkdir");
    // Type error on line 2: bind must be a string.
    std::fs::write(dir.join("bad.toml"), "[server]\nbind = 127.0.0.1:8123\n").expect("write");

    bin()
        .arg("--check")
        .arg("--config")
        .arg(dir.join("bad.toml"))
        .env("RUST_LOG", "off")
        .assert()
        .failure()
        .code(2)
        .stdout(predicate::str::contains("registration table").not())
        .stderr(predicate::str::contains("config error"))
        .stderr(predicate::str::contains("line 2"));

    std::fs::remove_dir_all(&dir).ok();
}

#[test]
fn missing_config_file_exits_two() {
    bin()
        .arg("--check")
        .arg("--config")
        .arg("/nonexistent/oxcost.toml")
        .env("RUST_LOG", "off")
        .assert()
        .failure()
        .code(2)
        .stderr(predicate::str::contains("cannot read"));
}

#[test]
fn valid_config_check_mode_still_works() {
    let dir = std::env::temp_dir().join("oxcost-it-cfg-ok");
    std::fs::create_dir_all(&dir).expect("mkdir");
    std::fs::write(
        dir.join("ok.toml"),
        "[server]\nbind = \"127.0.0.1:8123\"\n\n[plugins.\"dev.oxcost.sample-echo\"]\nenabled = false\n",
    )
    .expect("write");

    // A well-formed config passes validation; --check still prints the
    // table (back-compat: config does not change the check flow).
    bin()
        .arg("--check")
        .arg("--config")
        .arg(dir.join("ok.toml"))
        .env("RUST_LOG", "off")
        .assert()
        .success()
        .stdout(predicate::str::contains(
            "OxCost host — plugin registration table",
        ));

    std::fs::remove_dir_all(&dir).ok();
}

#[test]
fn example_remote_registers_and_exits_zero() {
    bin()
        .arg("--check")
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
        .arg("--check")
        .arg("--plugins")
        .arg(&dir)
        .env("RUST_LOG", "off")
        .assert()
        // Load failures are reported, not fatal: exit 0; the invalid plugin
        // is rejected (stderr) and absent from the table — the built-in SDK
        // fixture (§10 sample-echo) still shows as registered.
        .success()
        .stdout(predicate::str::contains(
            "OxCost host — plugin registration table",
        ))
        .stdout(predicate::str::contains("dev.oxcost.sample-echo"))
        .stdout(predicate::str::contains("dev.oxcost.badplugin").not())
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
