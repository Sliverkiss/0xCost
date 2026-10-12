//! OxCost host binary (M1): CLI, manifest loading, registration table.
//! HTTP serving and proxying arrive in M1.5/M2.

mod loader;

use loader::{load_plugins, print_registration_table, Cli};
use oxcost_core::PluginRegistry;
use tracing_subscriber::EnvFilter;

fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env())
        .init();

    let args: Vec<String> = std::env::args().collect();
    let cli = match Cli::parse(&args) {
        Ok(cli) => cli,
        Err(msg) => {
            eprintln!("oxcost: {msg}");
            std::process::exit(2);
        }
    };

    let mut registry = PluginRegistry::new();
    let outcomes = load_plugins(&mut registry, &cli.plugins_dir);
    for outcome in &outcomes {
        if let loader::LoadOutcome::Rejected { plugin, reason } = outcome {
            eprintln!("oxcost: rejected plugin `{plugin}`: {reason}");
        }
    }
    print_registration_table(&registry);
}
