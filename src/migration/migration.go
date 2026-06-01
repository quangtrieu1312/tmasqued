package migration

import (
    "context"
    "fmt"
    "sort"

    "github.com/quangtrieu1312/tmasqued/db"
    "github.com/quangtrieu1312/tmasqued/logger"
)

type MigrationStatus int

const (
    Pending MigrationStatus = iota
    Executed
    Succeeded
    Failed
    Unknown
)

// Migration is one ordered, atomic schema change. Run does its work in its own
// transaction and returns an error on failure, so the runner can stop and NOT
// record it as applied (it will be retried on the next boot).
type Migration interface {
    Version() int
    Description() string
    Run(ctx context.Context) error
}

func GenerateMigrationList() []Migration {
    return []Migration{
        GetMigration1(),
    }
}

// Invoke applies every not-yet-applied migration in version order, idempotently:
//   - the `migrations` table records which versions have succeeded;
//   - a version already recorded as Succeeded is skipped, so Invoke is safe to run
//     on every boot;
//   - a migration is recorded ONLY after its work commits, so a failure leaves it
//     un-recorded and it is retried next boot — never half-applied-but-marked-done.
// It stops at the first failure and returns the error; the caller should treat a
// non-nil result as fatal (a half-migrated schema must not serve traffic).
func Invoke(ctx context.Context) error {
    conn := db.GetConnection()
    // Bootstrap the bookkeeping table itself (idempotent) before consulting it.
    if _, err := conn.Exec(`
        CREATE TABLE IF NOT EXISTS migrations (
            id integer NOT NULL PRIMARY KEY,
            version integer NOT NULL UNIQUE,
            status integer NOT NULL DEFAULT 0,
            description text)`); err != nil {
        return fmt.Errorf("bootstrap migrations table: %w", err)
    }

    migrations := GenerateMigrationList()
    sort.Slice(migrations, func(i, j int) bool {
        return migrations[i].Version() < migrations[j].Version()
    })

    for _, m := range migrations {
        var applied int
        if err := conn.QueryRow(
            "SELECT COUNT(*) FROM migrations WHERE version = ? AND status = ?",
            m.Version(), Succeeded,
        ).Scan(&applied); err != nil {
            return fmt.Errorf("checking migration %d: %w", m.Version(), err)
        }
        if applied > 0 {
            continue
        }
        if logger.ShouldLog(logger.INFO) {
            logger.Info(fmt.Sprintf("Applying DB migration %d: %s", m.Version(), m.Description()))
        }
        if err := m.Run(ctx); err != nil {
            return fmt.Errorf("migration %d (%s) failed: %w", m.Version(), m.Description(), err)
        }
        if _, err := conn.Exec(
            `INSERT INTO migrations(version, status, description) VALUES(?, ?, ?)
             ON CONFLICT(version) DO UPDATE SET status = excluded.status`,
            m.Version(), Succeeded, m.Description(),
        ); err != nil {
            return fmt.Errorf("recording migration %d: %w", m.Version(), err)
        }
    }
    return nil
}
