package repository

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"

	"github.com/quangtrieu1312/tmasqued/db"
	"github.com/quangtrieu1312/tmasqued/domain"
)

// roleIDByName resolves a role name to its id within an existing transaction;
// returns an error (rolling back the tx) if the name doesn't exist.
func roleIDByName(tx *sql.Tx, name string) (int64, error) {
	var id int64
	if err := tx.QueryRow("SELECT id FROM roles WHERE name = ?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("no role named %q: %w", name, err)
	}
	return id, nil
}

func GetAllRoles() (*[]domain.Role, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    roles := []domain.Role{}
    rows, err := tx.Query("SELECT id, name FROM roles")
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

func GetRoleByID(roleID int64) (*domain.Role, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    role := domain.Role{}
    err = tx.QueryRow("SELECT id, name FROM roles WHERE id = ?", roleID).Scan(&role.ID, &role.Name)
    if err != nil {
        return nil, err
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &role, nil
}

// AssignResourcesToRoleByName resolves the role + resource names and grants them in
// ONE transaction; returns the resource ids newly granted. A missing name rolls back.
func AssignResourcesToRoleByName(roleName string, resourceNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    roleID, err := roleIDByName(tx, roleName)
    if err != nil {
        return nil, err
    }
    stmt, err := tx.Prepare(`
        INSERT INTO roles_resources(role_id, resource_id)
        VALUES (?, ?)
        ON CONFLICT (role_id, resource_id)
        DO NOTHING
    `)
    if err != nil {
        return nil, err
    }
    defer stmt.Close()
    added := []int64{}
    for _, name := range resourceNames {
        resourceID, err := resourceIDByName(tx, name)
        if err != nil {
            return nil, err
        }
        res, err := stmt.Exec(roleID, resourceID)
        if err != nil {
            return nil, err
        }
        if n, _ := res.RowsAffected(); n > 0 {
            added = append(added, resourceID)
        }
    }
    if err = tx.Commit(); err != nil {
        return nil, err
    }
    return &added, nil
}

// UnassignResourcesToRoleByName resolves names and revokes them in ONE transaction;
// returns the resource ids actually removed.
func UnassignResourcesToRoleByName(roleName string, resourceNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    roleID, err := roleIDByName(tx, roleName)
    if err != nil {
        return nil, err
    }
    stmt, err := tx.Prepare(`DELETE FROM roles_resources WHERE role_id = ? AND resource_id = ?`)
    if err != nil {
        return nil, err
    }
    defer stmt.Close()
    removed := []int64{}
    for _, name := range resourceNames {
        resourceID, err := resourceIDByName(tx, name)
        if err != nil {
            return nil, err
        }
        res, err := stmt.Exec(roleID, resourceID)
        if err != nil {
            return nil, err
        }
        if n, _ := res.RowsAffected(); n > 0 {
            removed = append(removed, resourceID)
        }
    }
    if err = tx.Commit(); err != nil {
        return nil, err
    }
    return &removed, nil
}

// AssignResourcesToRoles grants the given resources to the given roles and
// returns the resource IDs that were newly granted (already-present links skipped).
func AssignResourcesToRoles(resourceIDs []int64, roleIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`
        INSERT INTO roles_resources(role_id, resource_id)
        VALUES (?, ?)
        ON CONFLICT (role_id, resource_id)
        DO NOTHING
    `)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()
    added := []int64{}
    seen := map[int64]bool{}
    for _, roleID := range(roleIDs) {
        for _, resourceID := range(resourceIDs) {
            res, err := stmt.Exec(roleID, resourceID)
            if err != nil {
                return nil, err
            }
            if n, _ := res.RowsAffected(); n > 0 && !seen[resourceID] {
                added = append(added, resourceID)
                seen[resourceID] = true
            }
        }
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &added, nil
}

// UnassignResourcesToRoles revokes the given resources from the given roles and
// returns the resource IDs whose link was actually removed.
func UnassignResourcesToRoles(resourceIDs []int64, roleIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`DELETE FROM roles_resources WHERE role_id = ? AND resource_id = ?`)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()
    removed := []int64{}
    seen := map[int64]bool{}
    for _, roleID := range(roleIDs) {
        for _, resourceID := range(resourceIDs) {
            res, err := stmt.Exec(roleID, resourceID)
            if err != nil {
                return nil, err
            }
            if n, _ := res.RowsAffected(); n > 0 && !seen[resourceID] {
                removed = append(removed, resourceID)
                seen[resourceID] = true
            }
        }
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &removed, nil
}

func UpdateRoleName(roleID int64, newName string) (bool, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return false, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`UPDATE roles SET name = ? WHERE id = ?`)
    if err != nil {
	    return false, err
    }
    defer stmt.Close()

    _, err = stmt.Exec(newName, roleID)

    if err != nil {
	    return false, err
    }
    err = tx.Commit()
    if err != nil {
        return false, err
    }
    return true, nil
}

func UpsertRoles(roleNames []string) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    // Plain insert: a duplicate name hits the UNIQUE(name) constraint and fails,
    // so creating an existing role is rejected (no silent no-op / upsert).
    stmt, err := tx.Prepare(`INSERT INTO roles(name) VALUES(?)`)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()
	roleIDs := []int64{}
    for _, role := range(roleNames) {
		result, err := stmt.Exec(role)
        if err != nil {
	        return nil, err
        }
        id, err := result.LastInsertId()
        if err != nil {
	        return nil, err
        }
		roleIDs = append(roleIDs, id)
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &roleIDs, nil
}

// DeleteRoles removes the given roles and returns the IDs actually deleted.
func DeleteRoles(roleIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`DELETE FROM roles WHERE id = ?`)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()

    deleted := []int64{}
    for _, id := range roleIDs {
        res, err := stmt.Exec(id)
        if err != nil {
	        return nil, err
        }
        if n, _ := res.RowsAffected(); n > 0 {
            deleted = append(deleted, id)
        }
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &deleted, nil
}
