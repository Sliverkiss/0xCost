//! CLI + manifest loading + registration-table rendering.

use oxcost_contract::parse_manifest;
use oxcost_core::PluginRegistry;
use std::path::{Path, PathBuf};

/// Program config parsed from argv.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Cli {
    /// Directory scanned for `<dir>/*/plugin.toml`.
    pub plugins_dir: PathBuf,
    /// Listen address for the axum server (default `127.0.0.1:8080`).
    pub listen: String,
    /// Host config file path (R1W1 A2). `None` → defaults + env.
    pub config_path: Option<PathBuf>,
    /// Load, validate, print the registration table, and exit without
    /// serving (manifest check mode).
    pub check: bool,
}

impl Cli {
    /// Parse argv; `--plugins <dir>` (default `plugins/`), `--listen <addr>`,
    /// and `--check`. Errors are returned as message strings for `main` to
    /// print and exit 2.
    pub fn parse(args: &[String]) -> Result<Self, String> {
        let mut plugins_dir = PathBuf::from("plugins");
        let mut listen = String::from("127.0.0.1:8080");
        let mut check = false;
        let mut config_path: Option<PathBuf> = None;
        let mut iter = args.iter();
        // Skip argv[0].
        iter.next();
        while let Some(arg) = iter.next() {
            match arg.as_str() {
                "--plugins" => {
                    let Some(value) = iter.next() else {
                        return Err("--plugins requires a directory".into());
                    };
                    plugins_dir = PathBuf::from(value);
                }
                "--listen" => {
                    let Some(value) = iter.next() else {
                        return Err("--listen requires an address".into());
                    };
                    listen = value.clone();
                }
                "--check" => {
                    check = true;
                }
                "--config" => {
                    let Some(value) = iter.next() else {
                        return Err("--config requires a file path".into());
                    };
                    config_path = Some(PathBuf::from(value));
                }
                other => return Err(format!("unknown argument `{other}`")),
            }
        }
        Ok(Self {
            plugins_dir,
            listen,
            config_path,
            check,
        })
    }
}

/// Load outcomes: one per plugin directory, in sorted order.
#[derive(Debug, PartialEq)]
pub enum LoadOutcome {
    /// Registered successfully.
    Registered(String),
    /// Manifest rejected; message names the typed error.
    Rejected { plugin: String, reason: String },
    /// Directory has no plugin.toml (skipped silently).
    Skipped(String),
}

/// Scan `plugins_dir` for plugin.toml manifests and fill the registry.
pub fn load_plugins(registry: &mut PluginRegistry, plugins_dir: &Path) -> Vec<LoadOutcome> {
    let mut outcomes = Vec::new();
    let Some(dirs) = read_sorted_dirs(plugins_dir) else {
        return outcomes;
    };
    for dir in dirs {
        let name = dir
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        let manifest_path = dir.join("plugin.toml");
        if !manifest_path.is_file() {
            outcomes.push(LoadOutcome::Skipped(name));
            continue;
        }
        match std::fs::read_to_string(&manifest_path)
            .map_err(|e| format!("read: {e}"))
            .and_then(|text| parse_manifest(&text).map_err(|e| e.to_string()))
        {
            Ok(manifest) => match registry.insert(manifest) {
                Ok(()) => outcomes.push(LoadOutcome::Registered(name)),
                Err(e) => outcomes.push(LoadOutcome::Rejected {
                    plugin: name,
                    reason: e.to_string(),
                }),
            },
            Err(reason) => outcomes.push(LoadOutcome::Rejected {
                plugin: name,
                reason,
            }),
        }
    }
    outcomes
}

fn read_sorted_dirs(plugins_dir: &Path) -> Option<Vec<PathBuf>> {
    let mut dirs: Vec<PathBuf> = std::fs::read_dir(plugins_dir)
        .ok()?
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|p| p.is_dir())
        .collect();
    dirs.sort();
    Some(dirs)
}

/// Render the registration table to stdout.
pub fn print_registration_table(registry: &PluginRegistry) {
    println!("OxCost host — plugin registration table");
    println!("host supports manifestVersion 0.1, apiVersion v1");
    println!();
    if registry.list().is_empty() {
        println!("(no plugins registered)");
        return;
    }
    let header = format!(
        "{:<28} {:<10} {:<14} {:<24} {}",
        "ID", "VERSION", "STATE", "MODE", "CAPABILITIES"
    );
    println!("{header}");
    for entry in registry.list() {
        let m = &entry.manifest;
        println!(
            "{:<28} {:<10} {:<14} {:<24} {}",
            m.id,
            m.version,
            entry.state.name(),
            format!("{:?}", m.entry.mode),
            oxcost_contract::CapabilitySet::from_section(&m.capabilities).names(),
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cli_defaults_and_flags() {
        let cli = Cli::parse(&["oxcost".into()]).unwrap();
        assert_eq!(cli.plugins_dir, PathBuf::from("plugins"));
        assert_eq!(cli.listen, "127.0.0.1:8080");
        assert_eq!(cli.config_path, None);
        assert!(!cli.check);

        let cli = Cli::parse(&[
            "oxcost".into(),
            "--plugins".into(),
            "/tmp/my-plugins".into(),
        ])
        .unwrap();
        assert_eq!(cli.plugins_dir, PathBuf::from("/tmp/my-plugins"));

        let cli =
            Cli::parse(&["oxcost".into(), "--listen".into(), "127.0.0.1:9090".into()]).unwrap();
        assert_eq!(cli.listen, "127.0.0.1:9090");

        let cli = Cli::parse(&["oxcost".into(), "--check".into()]).unwrap();
        assert!(cli.check);

        // --listen requires a value.
        assert!(Cli::parse(&["oxcost".into(), "--listen".into()]).is_err());

        // --config requires a value and stores it.
        let cli = Cli::parse(&["oxcost".into(), "--config".into(), "x.toml".into()]).unwrap();
        assert_eq!(cli.config_path, Some(PathBuf::from("x.toml")));
        assert!(Cli::parse(&["oxcost".into(), "--config".into()]).is_err());
        assert!(Cli::parse(&["oxcost".into(), "--wat".into()]).is_err());
    }
}
