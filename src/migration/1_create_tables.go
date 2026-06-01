package migration

import (
    "context"
    "fmt"

    "github.com/quangtrieu1312/tmasqued/logger"
    "github.com/quangtrieu1312/tmasqued/db"
    "github.com/quangtrieu1312/tmasqued/utility"
)

type Migration1 struct {
    version     int
    status      MigrationStatus
    description string
}

func GetMigration1() Migration1 {
    return Migration1{version: 1, status: Unknown, description: "Create tables"}
}

func (m Migration1) Version() int        { return m.version }
func (m Migration1) Description() string { return m.description }

// Run creates the domain tables (idempotent: CREATE TABLE IF NOT EXISTS) and seeds
// the DHCP pool. It does NOT touch the `migrations` bookkeeping table — the runner
// (Invoke) owns that. Every statement's error is checked and the work runs in one
// transaction with a deferred rollback, so a partial failure leaves nothing behind
// and the migration is retried on the next boot.
func (m Migration1) Run(ctx context.Context) error {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()

    if _, err := tx.Exec(`
        CREATE TABLE IF NOT EXISTS clients (
            id integer PRIMARY KEY,
            name text NOT NULL UNIQUE,
            last_seen integer NOT NULL DEFAULT 0,
            ip text)`); err != nil {
        return fmt.Errorf("create clients: %w", err)
    }
    if _, err := tx.Exec(`
        CREATE TABLE IF NOT EXISTS resources (
            id integer PRIMARY KEY,
            name text NOT NULL UNIQUE,
            value text NOT NULL)`); err != nil {
        return fmt.Errorf("create resources: %w", err)
    }
    if _, err := tx.Exec(`
        CREATE TABLE IF NOT EXISTS roles (
            id integer PRIMARY KEY,
            name text NOT NULL UNIQUE)`); err != nil {
        return fmt.Errorf("create roles: %w", err)
    }
    if _, err := tx.Exec(`
        CREATE TABLE IF NOT EXISTS clients_roles (
            id integer PRIMARY KEY,
            client_id integer NOT NULL,
            role_id integer NOT NULL,
			CONSTRAINT compount_unique UNIQUE (client_id, role_id),
            CONSTRAINT fk_client FOREIGN KEY (client_id)
            REFERENCES clients(id) ON DELETE CASCADE,
            CONSTRAINT fk_role FOREIGN KEY (role_id)
            REFERENCES roles(id) ON DELETE CASCADE)`); err != nil {
        return fmt.Errorf("create clients_roles: %w", err)
    }
    if _, err := tx.Exec(`
        CREATE TABLE IF NOT EXISTS roles_resources (
            id integer PRIMARY KEY,
            role_id integer NOT NULL,
            resource_id integer NOT NULL,
			CONSTRAINT compount_unique UNIQUE (role_id, resource_id),
            CONSTRAINT fk_role FOREIGN KEY (role_id)
            REFERENCES roles(id) ON DELETE CASCADE,
            CONSTRAINT fk_resource FOREIGN KEY (resource_id)
            REFERENCES resources(id) ON DELETE CASCADE)`); err != nil {
        return fmt.Errorf("create roles_resources: %w", err)
    }
    if _, err := tx.Exec(`
        CREATE TABLE IF NOT EXISTS dhcp (
            id integer PRIMARY KEY,
            first_ip bigint NOT NULL UNIQUE,
            last_ip bigint NOT NULL UNIQUE)`); err != nil {
        return fmt.Errorf("create dhcp: %w", err)
    }

    // VIRT_CIDR = virtual CIDR range for VPN traffic. The first usable IP is the
    // gateway (VPN server), so the pool starts one above it. ON CONFLICT keeps an
    // existing pool intact across reboots — re-running never resets a consumed pool.
    virtCIDR := ctx.Value("VIRT_CIDR").(string)
    firstIP, _ := utility.FirstUsableIP(virtCIDR)
    lastIP, _ := utility.LastUsableIP(virtCIDR)
    _, firstIPNum, _ := utility.ParseIP(firstIP)
    _, lastIPNum, _ := utility.ParseIP(lastIP)
    firstIPNum++ // skip the gateway IP
    if logger.ShouldLog(logger.INFO) {
        logger.Info(fmt.Sprintf("Client DHCP IP range = %v - %v", utility.IntToIPv4(uint32(firstIPNum)).String(), utility.IntToIPv4(uint32(lastIPNum)).String()))
    }
    if _, err := tx.Exec(`
        INSERT INTO dhcp(id, first_ip, last_ip)
        VALUES(1, ?, ?)
        ON CONFLICT(id)
        DO NOTHING`, firstIPNum, lastIPNum); err != nil {
        return fmt.Errorf("seed dhcp: %w", err)
    }

    return tx.Commit()
}
