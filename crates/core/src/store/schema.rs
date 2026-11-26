use crate::{Error, Result, SCHEMA_USER_VERSION};
use rusqlite::Connection;
use vantry_protocol::ErrorCode;
pub(super) const DDLS: [&str; 12] = [
    include_str!("sql/v1.sql"),
    include_str!("sql/v2.sql"),
    include_str!("sql/v3.sql"),
    include_str!("sql/v4.sql"),
    include_str!("sql/v5.sql"),
    include_str!("sql/v6.sql"),
    include_str!("sql/v7.sql"),
    include_str!("sql/v8.sql"),
    include_str!("sql/v9.sql"),
    include_str!("sql/v10.sql"),
    include_str!("sql/v11.sql"),
    include_str!("sql/v12.sql"),
];
fn column_exists(db: &Connection, table: &str, column: &str) -> rusqlite::Result<bool> {
    db.query_row(
        "SELECT EXISTS(SELECT 1 FROM pragma_table_info(?1) WHERE name=?2)",
        [table, column],
        |r| r.get(0),
    )
}
fn ensure(db: &Connection, sql: &str) -> Result<()> {
    let words: Vec<_> = sql.split_whitespace().collect();
    if sql.starts_with("ALTER TABLE") {
        if !column_exists(db, words[2], words[5])? {
            db.execute_batch(sql)?;
        }
    } else if sql.starts_with("CREATE TABLE") {
        let (header, body) = sql.split_once('(').expect("canonical DDL");
        let table = header.split_whitespace().last().unwrap();
        if !column_exists(db, table, "id")? {
            db.execute_batch(sql)?;
        } else {
            for definition in body
                .trim_end_matches(')')
                .lines()
                .map(|s| s.trim().trim_end_matches(','))
                .filter(|s| !s.is_empty())
            {
                let column = definition.split_whitespace().next().unwrap();
                if !column_exists(db, table, column)? {
                    db.execute_batch(&format!("ALTER TABLE {table} ADD COLUMN {definition}"))?;
                }
            }
        }
    } else if sql.contains("IF NOT EXISTS") {
        db.execute_batch(sql)?;
    } else {
        db.execute_batch(&sql.replacen("INDEX ", "INDEX IF NOT EXISTS ", 1))?;
    }
    Ok(())
}
pub(super) fn migrate(db: &mut Connection) -> Result<()> {
    let version: u32 = db.query_row("PRAGMA user_version", [], |r| r.get(0))?;
    if version > SCHEMA_USER_VERSION {
        return Err(Error::Api(
            ErrorCode::VersionMismatch,
            format!("database schema {version} exceeds supported version {SCHEMA_USER_VERSION}"),
        ));
    }
    db.execute_batch(
        "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA synchronous=NORMAL;",
    )?;
    let tx = db.transaction()?;
    for ddl in DDLS.iter().take(2).skip(version as usize) {
        tx.execute_batch(ddl)?;
    }
    for ddl in &DDLS[2..] {
        let sql = ddl
            .lines()
            .filter(|s| !s.trim_start().starts_with("--"))
            .collect::<Vec<_>>()
            .join("\n");
        for stmt in sql.split(';').map(str::trim).filter(|s| !s.is_empty()) {
            ensure(&tx, stmt)?;
        }
    }
    tx.execute_batch(include_str!("sql/backfill.sql"))?;
    if version > 0 && version < 12 {
        super::blocks::backfill(&tx)?;
    }
    tx.execute_batch(
        "CREATE TABLE IF NOT EXISTS core_metadata(key TEXT PRIMARY KEY,int_value INTEGER,
             text_value TEXT); INSERT OR IGNORE INTO core_metadata(key,int_value) VALUES('pruned_through_sequence',
             0); PRAGMA user_version=12;",
    )?;
    for (table, index, cols) in [
        ("threads", "thread_search", vec!["title", "workspace"]),
        ("runs", "run_search", vec!["input", "output", "workspace"]),
    ] {
        let exists: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name=?)",
            [index],
            |r| r.get(0),
        )?;
        let columns = cols.join(",");
        let old = cols
            .iter()
            .map(|c| format!("old.{c}"))
            .collect::<Vec<_>>()
            .join(",");
        let new = cols
            .iter()
            .map(|c| format!("new.{c}"))
            .collect::<Vec<_>>()
            .join(",");
        let insert = format!("INSERT INTO {index}(rowid,{columns}) VALUES(new.rowid,{new});");
        let remove = format!(
            "INSERT INTO {index}({index},rowid,{columns}) VALUES('delete',old.rowid,{old});"
        );
        tx.execute_batch(&format!(
            "CREATE VIRTUAL TABLE IF NOT EXISTS {index} USING fts5({columns}, content='{table}',
             content_rowid='rowid',tokenize='trigram'); CREATE TRIGGER IF NOT EXISTS {index}_insert
             AFTER INSERT ON {table} BEGIN {insert} END; CREATE TRIGGER IF NOT EXISTS {index}_delete
             AFTER DELETE ON {table} BEGIN {remove} END; CREATE TRIGGER IF NOT EXISTS {index}_update
             AFTER UPDATE OF {columns} ON {table} BEGIN {remove} {insert} END;"
        ))?;
        if !exists {
            tx.execute_batch(&format!("INSERT INTO {index}({index}) VALUES('rebuild')"))?;
        }
    }
    tx.commit()?;
    Ok(())
}
