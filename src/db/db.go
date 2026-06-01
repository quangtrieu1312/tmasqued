package db

import (
    "fmt"
    "context"
    "database/sql"
    _ "github.com/mattn/go-sqlite3"
    "github.com/quangtrieu1312/tmasqued/constants"
    "github.com/quangtrieu1312/tmasqued/logger"
    "github.com/quangtrieu1312/tmasqued/config"
)

type DB struct {
    conn *sql.DB
}

var dbInstance *DB = generateInstance()

func GetInstance() *DB {
    return dbInstance
}

func GetConnection() *sql.DB {
    return dbInstance.conn
}

func CloseConnection() {
    dbInstance.conn.Close()
}

func generateInstance() *DB {
    ctx := context.Background()
    config.Load(&ctx)
    // DSN params:
    //   _busy_timeout=5000  — wait up to 5s for a held lock instead of failing
    //                         immediately with "database is locked" (SQLITE_BUSY).
    //   _journal_mode=WAL   — readers don't block the single writer.
    //   _foreign_keys=on    — enforce the declared FKs. ON DELETE CASCADE on the
    //                         junction tables then cleans up a deleted entity's *link*
    //                         rows (the linked roles/resources themselves are kept),
    //                         so no dangling junction rows / id-reuse anomalies.
    dsn := constants.DB_INFO + "?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on"
    dbConn, err := sql.Open("sqlite3", dsn)
    if err != nil {
        logger.Fatal(fmt.Sprintf("cannot connect to DB: %v",err))
    }
    // SQLite is single-writer; funnel all access through one connection so concurrent
    // management/connect requests serialize in-process instead of racing the file lock.
    // The control plane is low-traffic, so serialization is fine.
    dbConn.SetMaxOpenConns(1)
    instance := &DB{dbConn}
    return instance
}

