package repository

import (
	"database/sql"
	"fmt"
    _ "github.com/mattn/go-sqlite3"

    "github.com/quangtrieu1312/tmasqued/db"
	"github.com/quangtrieu1312/tmasqued/domain"
	"github.com/quangtrieu1312/tmasqued/utility"
)

// clientIDByName resolves a client name to its id within an existing transaction;
// returns an error (rolling back the tx) if the name doesn't exist.
func clientIDByName(tx *sql.Tx, name string) (int64, error) {
	var id int64
	if err := tx.QueryRow("SELECT id FROM clients WHERE name = ?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("no client named %q: %w", name, err)
	}
	return id, nil
}

func GetAllClients() (*[]domain.Client, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    clients := []domain.Client{}
    rows, err := tx.Query("SELECT id, name, ip, last_seen FROM clients")
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    for rows.Next() {
        c := domain.Client{}
	    err := rows.Scan(&c.ID, &c.Name, &c.IP, &c.LastSeen)
	    if err != nil {
		    return nil, err
	    }
        clients = append(clients, c)
    }
    err = rows.Err()
    if err != nil {
	    return nil, err
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &clients, nil
}

func GetClientByID(clientID int64) (*domain.Client, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    client := domain.Client{}
    err = tx.QueryRow("SELECT id, name, ip, last_seen FROM clients WHERE id = ?", clientID).Scan(&client.ID, &client.Name, &client.IP, &client.LastSeen)
    if err != nil {
        return nil, err
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &client, nil
}

// CreateClientsWithRoles creates each named client (allocating a DHCP IP) together
// with a same-named default role and the link between them — all in ONE transaction,
// so any failure rolls back the whole batch (no orphaned client with a consumed IP
// but no role). Duplicate client or role names fail via the UNIQUE constraints.
// Returns the new client ids.
func CreateClientsWithRoles(clientNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()

    clientStmt, err := tx.Prepare(`INSERT INTO clients(name, ip) VALUES(?, ?)`)
    if err != nil {
        return nil, err
    }
    defer clientStmt.Close()
    roleStmt, err := tx.Prepare(`INSERT INTO roles(name) VALUES(?)`)
    if err != nil {
        return nil, err
    }
    defer roleStmt.Close()
    linkStmt, err := tx.Prepare(`INSERT INTO clients_roles(client_id, role_id) VALUES(?, ?)`)
    if err != nil {
        return nil, err
    }
    defer linkStmt.Close()

    clientIDs := []int64{}
    for _, name := range clientNames {
        // Allocate the lowest free IP from the DHCP pool (same logic as UpsertClients).
        dhcp := domain.DHCP{}
        if err = tx.QueryRow("SELECT id, first_ip, last_ip FROM dhcp ORDER BY first_ip ASC").Scan(&dhcp.ID, &dhcp.FirstIP, &dhcp.LastIP); err != nil {
            return nil, err
        }
        ip := utility.IntToIPv4(uint32(dhcp.FirstIP)).String()
        if dhcp.FirstIP == dhcp.LastIP {
            if _, err = tx.Exec("DELETE FROM dhcp WHERE id = ?", dhcp.ID); err != nil {
                return nil, err
            }
        } else {
            if _, err = tx.Exec("UPDATE dhcp SET first_ip = ?, last_ip = ? WHERE id = ?", dhcp.FirstIP+1, dhcp.LastIP, dhcp.ID); err != nil {
                return nil, err
            }
        }
        cres, err := clientStmt.Exec(name, ip)
        if err != nil {
            return nil, err
        }
        clientID, err := cres.LastInsertId()
        if err != nil {
            return nil, err
        }
        rres, err := roleStmt.Exec(name)
        if err != nil {
            return nil, err
        }
        roleID, err := rres.LastInsertId()
        if err != nil {
            return nil, err
        }
        if _, err = linkStmt.Exec(clientID, roleID); err != nil {
            return nil, err
        }
        clientIDs = append(clientIDs, clientID)
    }
    if err = tx.Commit(); err != nil {
        return nil, err
    }
    return &clientIDs, nil
}

// AssignRolesToClientByName resolves the client + role names and links them in ONE
// transaction; returns the role ids newly linked. A missing name rolls the tx back.
func AssignRolesToClientByName(clientName string, roleNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    clientID, err := clientIDByName(tx, clientName)
    if err != nil {
        return nil, err
    }
    stmt, err := tx.Prepare(`
        INSERT INTO clients_roles(client_id, role_id)
        VALUES (?, ?)
        ON CONFLICT (client_id, role_id)
        DO NOTHING
    `)
    if err != nil {
        return nil, err
    }
    defer stmt.Close()
    added := []int64{}
    for _, name := range roleNames {
        roleID, err := roleIDByName(tx, name)
        if err != nil {
            return nil, err
        }
        res, err := stmt.Exec(clientID, roleID)
        if err != nil {
            return nil, err
        }
        if n, _ := res.RowsAffected(); n > 0 {
            added = append(added, roleID)
        }
    }
    if err = tx.Commit(); err != nil {
        return nil, err
    }
    return &added, nil
}

// UnassignRolesToClientByName resolves names and unlinks them in ONE transaction;
// returns the role ids actually removed.
func UnassignRolesToClientByName(clientName string, roleNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    clientID, err := clientIDByName(tx, clientName)
    if err != nil {
        return nil, err
    }
    stmt, err := tx.Prepare(`DELETE FROM clients_roles WHERE client_id = ? AND role_id = ?`)
    if err != nil {
        return nil, err
    }
    defer stmt.Close()
    removed := []int64{}
    for _, name := range roleNames {
        roleID, err := roleIDByName(tx, name)
        if err != nil {
            return nil, err
        }
        res, err := stmt.Exec(clientID, roleID)
        if err != nil {
            return nil, err
        }
        if n, _ := res.RowsAffected(); n > 0 {
            removed = append(removed, roleID)
        }
    }
    if err = tx.Commit(); err != nil {
        return nil, err
    }
    return &removed, nil
}

func UpsertClients(clientNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`
        INSERT INTO clients(name, ip)
        VALUES(?, ?)
        `)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()
    clientIDs := []int64{}
    for i := 0; i < len(clientNames); i++ {
        client := domain.Client{}
        client.Name = clientNames[i]

        dhcp := domain.DHCP{}
        err = tx.QueryRow("SELECT id, first_ip, last_ip FROM dhcp ORDER BY first_ip ASC").Scan(&dhcp.ID, &dhcp.FirstIP, &dhcp.LastIP)
        if err != nil {
	        return nil, err
        }

        client.IP = utility.IntToIPv4(uint32(dhcp.FirstIP)).String()
        if dhcp.FirstIP == dhcp.LastIP {
            _, err = tx.Exec("DELETE FROM dhcp WHERE id = ?", dhcp.ID)
            if err != nil {
	            return nil, err
            }
        } else {
            _, err = tx.Exec("UPDATE dhcp SET first_ip = ?, last_ip = ? WHERE id = ?", dhcp.FirstIP + 1, dhcp.LastIP, dhcp.ID)
            if err != nil {
	            return nil, err
            }
        }

        result, err := stmt.Exec(client.Name, client.IP)
        if err != nil {
	        return nil, err
        }
        id, err := result.LastInsertId()
        if err != nil {
            return nil, err
        }
        clientIDs = append(clientIDs, id)
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &clientIDs, nil
}

func AssignIPToClient(clientID int64) (string, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return "", err
    }
	defer tx.Rollback()
    var oldIP sql.NullString
    err = tx.QueryRow("SELECT ip FROM clients WHERE id = ?", clientID).Scan(&oldIP)
    if err != nil {
	    return "", err
    }
    if oldIP.Valid && oldIP.String != "" {
        return oldIP.String, nil
    }
    dhcp := domain.DHCP{}
    err = tx.QueryRow("SELECT id, first_ip, last_ip FROM dhcp ORDER BY first_ip ASC").Scan(&dhcp.ID, &dhcp.FirstIP, &dhcp.LastIP)
    if err != nil {
	    return "", err
    }
    _, err = tx.Exec("UPDATE dhcp SET first_ip = ?, last_ip = ? WHERE id = ?", dhcp.FirstIP + 1, dhcp.LastIP, dhcp.ID)
    if err != nil {
	    return "", err
    }
	newIP := utility.IntToIPv4(uint32(dhcp.FirstIP)).String()
    _, err = tx.Exec("UPDATE clients SET ip = ? WHERE id = ?", newIP, clientID)
    if err != nil {
	    return "", err
    }
    err = tx.Commit()
    if err != nil {
        return "", err
    }
    return newIP, nil
}

// DeleteClients removes the given clients (returning each freed IP to the DHCP
// pool, merging adjacent ranges) and returns the IDs that were actually deleted.
// Each client's same-named default role is deleted along with it; all OTHER roles
// and resources the client was linked to are left intact (no cascade — the schema
// declares ON DELETE CASCADE but FK enforcement is off by design, so removing an
// entity never deletes the data it was linked to).
func DeleteClients(clientIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare("DELETE FROM clients WHERE id = ?")
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()

    deleted := []int64{}
    for i := 0; i < len(clientIDs); i++ {
        client := domain.Client{}
        err = tx.QueryRow("SELECT id, name, ip, last_seen FROM clients WHERE id = ?", clientIDs[i]).Scan(&client.ID, &client.Name, &client.IP, &client.LastSeen)
        if err != nil {
	        return nil, err
        }

        _, ipInt, err := utility.ParseIP(client.IP)
        if err != nil {
            return nil, err
        }
        dhcp1 := domain.DHCP{}
        err1 := tx.QueryRow("SELECT first_ip, last_ip FROM dhcp WHERE last_ip = ?", ipInt-1).Scan(&dhcp1.FirstIP, &dhcp1.LastIP)

        dhcp2 := domain.DHCP{}
        err2 := tx.QueryRow("SELECT first_ip, last_ip FROM dhcp WHERE first_ip = ?", ipInt+1).Scan(&dhcp2.FirstIP, &dhcp2.LastIP)

        // Return IP to DHCP pool
        if err1 == sql.ErrNoRows && err2 == sql.ErrNoRows {
            // There is nothing to merge
            _, err = tx.Exec("INSERT INTO dhcp(first_ip, last_ip) VALUES(?, ?)", ipInt, ipInt)
            if err != nil {
	            return nil, err
            }
        } else if err1 == nil && err2 == sql.ErrNoRows {
            // Merge [dhcp1.FirstIP, ipInt-1] with [ipInt, ipInt]
            _, err = tx.Exec("UPDATE dhcp SET first_ip = ?, last_ip = ? WHERE first_ip = ? and last_ip = ?", dhcp1.FirstIP, dhcp1.LastIP + 1, dhcp1.FirstIP, dhcp1.LastIP)
            if err != nil {
	            return nil, err
            }
        } else if err1 == sql.ErrNoRows && err2 == nil {
            // Merge [ipInt, ipInt] with [ipInt+1, dhcp2.LastIP]
            _, err = tx.Exec("UPDATE dhcp SET first_ip = ?, last_ip = ? WHERE first_ip = ? and last_ip = ?", dhcp2.FirstIP-1, dhcp2.LastIP, dhcp2.FirstIP, dhcp2.LastIP)
            if err != nil {
	            return nil, err
            }
        } else if err1 == nil && err2 == nil {
            // Merge [dhcp1.FirstIP, ipInt-1] with [ipInt, ipInt] with [ipInt+1, dhcp2.LastIP]
            _, err = tx.Exec("DELETE FROM dhcp WHERE (first_ip = ? and last_ip = ?) or (first_ip = ? and last_ip = ?)", dhcp1.FirstIP, dhcp1.LastIP, dhcp2.FirstIP, dhcp2.LastIP)
            if err != nil {
	            return nil, err
            }
            _, err = tx.Exec("INSERT INTO dhcp(first_ip, last_ip) VALUES(?, ?)", dhcp1.FirstIP, dhcp2.LastIP)
            if err != nil {
	            return nil, err
            }
        } else if err1 != nil {
            return nil, err1
        } else {
            return nil, err2
        }

        res, err := stmt.Exec(clientIDs[i])
        if err != nil {
	        return nil, err
        }
        if n, _ := res.RowsAffected(); n > 0 {
            deleted = append(deleted, clientIDs[i])
        }

        // The same-named default role is owned by the client → remove it too.
        // (No-op if it was already removed/renamed.) Other linked roles stay.
        _, err = tx.Exec("DELETE FROM roles WHERE name = ?", client.Name)
        if err != nil {
            return nil, err
        }
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &deleted, nil
}

// UnassignRolesToClients unlinks the given roles from the given clients and
// returns the role IDs whose link was actually removed.
func UnassignRolesToClients(roleIDs []int64, clientIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`DELETE FROM clients_roles WHERE client_id = ? AND role_id = ?`)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()
    removed := []int64{}
    seen := map[int64]bool{}
    for _, clientID := range clientIDs {
        for _, roleID := range roleIDs {
            res, err := stmt.Exec(clientID, roleID)
            if err != nil {
                return nil, err
            }
            if n, _ := res.RowsAffected(); n > 0 && !seen[roleID] {
                removed = append(removed, roleID)
                seen[roleID] = true
            }
        }
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &removed, nil
}

// AssignRolesToClients links the given roles to the given clients and returns
// the role IDs that were newly linked (already-present links are skipped).
func AssignRolesToClients(roleIDs []int64, clientIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`
        INSERT INTO clients_roles(client_id, role_id)
        VALUES (?, ?)
        ON CONFLICT (client_id, role_id)
        DO NOTHING
    `)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()
    added := []int64{}
    seen := map[int64]bool{}
    for _, clientID := range(clientIDs) {
        for _, roleID := range(roleIDs) {
            res, err := stmt.Exec(clientID, roleID)
            if err != nil {
                return nil, err
            }
            if n, _ := res.RowsAffected(); n > 0 && !seen[roleID] {
                added = append(added, roleID)
                seen[roleID] = true
            }
        }
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &added, nil
}

// UpdateClientName renames a client and, in the same transaction, renames the
// client's default role — the same-named role auto-created with the client. The
// role rename matches on the client's OLD name; if that role no longer exists
// (deleted or already renamed) it is simply a no-op.
func UpdateClientName(clientID int64, newName string) (bool, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return false, err
    }
	defer tx.Rollback()

    var oldName string
    err = tx.QueryRow("SELECT name FROM clients WHERE id = ?", clientID).Scan(&oldName)
    if err != nil {
	    return false, err
    }

    _, err = tx.Exec(`UPDATE clients SET name = ? WHERE id = ?`, newName, clientID)
    if err != nil {
	    return false, err
    }

    // Cascade the rename to the client's default role (shares the old name).
    _, err = tx.Exec(`UPDATE roles SET name = ? WHERE name = ?`, newName, oldName)
    if err != nil {
	    return false, err
    }

    err = tx.Commit()
    if err != nil {
        return false, err
    }
    return true, nil
}

func GetClientRoles(clientID int64) (*[]domain.Role, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    roles := []domain.Role{}

    rows, err := tx.Query(`
        SELECT r.id, r.name
		FROM clients_roles as cr
		JOIN roles as r
		ON cr.role_id = r.id
        WHERE cr.client_id = ?`, clientID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    for rows.Next() {
        r := domain.Role{}
	    err := rows.Scan(&r.ID, &r.Name)
	    if err != nil {
		    return nil, err
	    }
        roles = append(roles, r)
    }
    err = rows.Err()
    if err != nil {
	    return nil, err
    }

    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &roles, nil
}

// GetClientResources returns a client's effective resources — the union of the
// resources granted by all roles linked to the client.
func GetClientResources(clientID int64) (*[]domain.Resource, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    resources := []domain.Resource{}

    rows, err := tx.Query(`
        SELECT r.id, r.name, r.value
        FROM resources as r
        JOIN roles_resources as rr
        ON r.id = rr.resource_id
        JOIN clients_roles as cr
        ON cr.role_id = rr.role_id
        WHERE cr.client_id = ?`, clientID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    for rows.Next() {
        r := domain.Resource{}
	    err := rows.Scan(&r.ID, &r.Name, &r.Value)
	    if err != nil {
		    return nil, err
	    }
        resources = append(resources, r)
    }
    err = rows.Err()
    if err != nil {
	    return nil, err
    }

    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &resources, nil
}
