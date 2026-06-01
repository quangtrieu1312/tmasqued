package repository

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"

	"github.com/quangtrieu1312/tmasqued/db"
	"github.com/quangtrieu1312/tmasqued/domain"
	"github.com/quangtrieu1312/tmasqued/request"
)

// resourceIDByName resolves a resource name to its id within an existing
// transaction; returns an error (rolling back the tx) if the name doesn't exist.
func resourceIDByName(tx *sql.Tx, name string) (int64, error) {
	var id int64
	if err := tx.QueryRow("SELECT id FROM resources WHERE name = ?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("no resource named %q: %w", name, err)
	}
	return id, nil
}


func GetAllResources() (*[]domain.Resource, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    resources := []domain.Resource{}
    rows, err := tx.Query("SELECT id, name, value FROM resources")
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

func GetResourceByID(resourceID int64) (*domain.Resource, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    resource := domain.Resource{}
    err = tx.QueryRow("SELECT id, name, value FROM resources WHERE id = ?", resourceID).Scan(&resource.ID, &resource.Name, &resource.Value)
    if err != nil {
        return nil, err
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &resource, nil
}

func UpsertResources(resources []request.Resource) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    // Plain insert: a duplicate name hits the UNIQUE(name) constraint and fails,
    // so creating an existing resource is rejected (no silent overwrite/upsert).
    stmt, err := tx.Prepare(`INSERT INTO resources(name, value) VALUES(?, ?)`)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()

    resourceIDs := []int64{}
    for _, resource := range(resources) {
		result, err := stmt.Exec(resource.Name, resource.Value)
        if err != nil {
	        return nil, err
        }
        id, err := result.LastInsertId()

        if err != nil {
	        return nil, err
        }
        resourceIDs = append(resourceIDs, id)
    }
    err = tx.Commit()
    if err != nil {
        return nil, err
    }
    return &resourceIDs, nil
}

func UpdateResourceName(resourceID int64, newName string) (bool, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return false, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`UPDATE resources SET name = ? WHERE id = ?`)
    if err != nil {
	    return false, err
    }
    defer stmt.Close()

    _, err = stmt.Exec(newName, resourceID)

    if err != nil {
	    return false, err
    }
    err = tx.Commit()
    if err != nil {
        return false, err
    }
    return true, nil
}

// DeleteResources removes the given resources and returns the IDs actually deleted.
func DeleteResources(resourceIDs []int64) (*[]int64, error) {
    tx, err := db.GetConnection().Begin()
    if err != nil {
        return nil, err
    }
	defer tx.Rollback()
    stmt, err := tx.Prepare(`DELETE FROM resources WHERE id = ?`)
    if err != nil {
	    return nil, err
    }
    defer stmt.Close()

    deleted := []int64{}
    for _, id := range resourceIDs {
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
