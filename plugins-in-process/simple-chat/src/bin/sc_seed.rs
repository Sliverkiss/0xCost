//! `sc-seed` — maintenance CLI for the simple-chat plugin's SQLite store
//! (offline, never on the request path; dossier §2.2). Two modes:
//!
//! - `import`: read-only Upstash REST GETs (index + per-identity records),
//!   validate, and upsert into SQLite. Prints the R1 concurrency warning.
//! - `register`: insert fresh account rows from a JSON file/stdin (same
//!   validation as Go's `validateAdminAccount`).
//!
//! Env (import mode): `UPSTASH_REDIS_REST_URL`, `UPSTASH_REDIS_REST_TOKEN`.
//! GET only — this binary contains no code that can write Redis.

use std::io::Read;
use std::path::PathBuf;
use std::process::ExitCode;

use clap::Parser;
use oxcost_plugin_simple_chat::seed::{
    import_records, parse_account_reply, parse_index_reply, SeedError, CONCURRENCY_WARNING,
};
use oxcost_plugin_simple_chat::{Account, SqliteStore};

/// Upstash REST GET endpoint for one key (GET only).
fn upstash_get_url(base: &str, key: &str) -> String {
    format!("{}/get/{key}", base.trim_end_matches('/'))
}

/// Fetch the index and every member record with GET-only REST calls.
/// Unparseable member records are skipped with a log line (one bad row
/// must not take the pool down — `redis.go` Load semantics).
#[tokio::main]
async fn import_upstash(
    _store: &SqliteStore,
    url: &str,
    token: &str,
) -> Result<Vec<Account>, Box<dyn std::error::Error>> {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(30))
        .build()?;
    let index_body = client
        .get(upstash_get_url(url, "simple-chat:accounts:index"))
        .bearer_auth(token)
        .send()
        .await?
        .error_for_status()?
        .text()
        .await?;
    let index = parse_index_reply(&index_body)?;
    eprintln!("index: {} member(s)", index.len());
    let mut records = Vec::with_capacity(index.len());
    for identity in &index {
        let body = client
            .get(upstash_get_url(
                url,
                &format!("simple-chat:accounts:{identity}"),
            ))
            .bearer_auth(token)
            .send()
            .await?
            .error_for_status()?
            .text()
            .await?;
        match parse_account_reply(&body) {
            Ok(Some(acct)) => records.push(acct),
            Ok(None) => eprintln!("skip {identity}: key absent (index ahead of data)"),
            Err(SeedError::BadReply(e)) => eprintln!("skip {identity}: unparseable record ({e})"),
            Err(e) => return Err(e.into()),
        }
    }
    Ok(records)
}

#[derive(Parser)]
#[command(
    name = "sc-seed",
    about = "Seed the simple-chat plugin's SQLite account store (offline maintenance CLI)"
)]
enum Cli {
    /// Import accounts from Upstash (read-only GET; R1 warning applies).
    Import {
        /// SQLite database path.
        #[arg(long)]
        db: PathBuf,
        /// Live mode requires SC_LIVE_UPSTASH=1 (gates never run live).
        #[arg(long, default_value_t = false)]
        live: bool,
        /// Read a JSON array of account records from this file instead of
        /// Upstash (offline fixture mode).
        #[arg(long)]
        from_json: Option<PathBuf>,
    },
    /// Register fresh accounts from a JSON array (stdin or --from-json).
    Register {
        #[arg(long)]
        db: PathBuf,
        #[arg(long)]
        from_json: Option<PathBuf>,
    },
}

fn read_records(path: Option<&PathBuf>) -> Result<Vec<Account>, Box<dyn std::error::Error>> {
    let raw = match path {
        Some(p) => std::fs::read_to_string(p)?,
        None => {
            let mut buf = String::new();
            std::io::stdin().read_to_string(&mut buf)?;
            buf
        }
    };
    Ok(serde_json::from_str(&raw)?)
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    let (db, records, _is_import) = match &cli {
        Cli::Import {
            db,
            live,
            from_json,
        } => {
            let records = if *live {
                if std::env::var("SC_LIVE_UPSTASH").ok().as_deref() != Some("1") {
                    eprintln!("live import requires SC_LIVE_UPSTASH=1 (never run in gates)");
                    return ExitCode::from(2);
                }
                let url = std::env::var("UPSTASH_REDIS_REST_URL")
                    .expect("UPSTASH_REDIS_REST_URL not set");
                let token = std::env::var("UPSTASH_REDIS_REST_TOKEN")
                    .expect("UPSTASH_REDIS_REST_TOKEN not set");
                let store = match SqliteStore::open(db) {
                    Ok(s) => s,
                    Err(e) => {
                        eprintln!("open db: {e}");
                        return ExitCode::FAILURE;
                    }
                };
                match import_upstash(&store, &url, &token) {
                    Ok(records) => {
                        drop(store);
                        (db.clone(), records, true)
                    }
                    Err(e) => {
                        eprintln!("upstash import: {e}");
                        return ExitCode::FAILURE;
                    }
                }
            } else {
                match read_records(from_json.as_ref()) {
                    Ok(r) => (db.clone(), r, true),
                    Err(e) => {
                        eprintln!("read records: {e}");
                        return ExitCode::FAILURE;
                    }
                }
            };
            println!("{CONCURRENCY_WARNING}");
            records
        }
        Cli::Register { db, from_json } => match read_records(from_json.as_ref()) {
            Ok(r) => (db.clone(), r, false),
            Err(e) => {
                eprintln!("read records: {e}");
                return ExitCode::FAILURE;
            }
        },
    };
    let store = match SqliteStore::open(&db) {
        Ok(s) => s,
        Err(e) => {
            eprintln!("open db: {e}");
            return ExitCode::FAILURE;
        }
    };
    match import_records(&store, records, std::time::SystemTime::now()) {
        Ok(summary) => {
            println!(
                "imported {} account(s), {} with park state, {} skipped",
                summary.imported,
                summary.parked,
                summary.skipped.len()
            );
            for (id, reason) in &summary.skipped {
                eprintln!("skip {id}: {reason}");
            }
            if summary.imported == 0 {
                eprintln!("no accounts imported — store is empty (valid starting state)");
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("import: {e}");
            ExitCode::FAILURE
        }
    }
}
