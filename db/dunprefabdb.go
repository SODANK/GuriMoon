package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Dungeon editor prefabs: a group of balls saved once and stamped into any
// room of any dungeon. A prefab is the balls' exact stored rows (loot, NULLs,
// waves, brains, factions) with positions relative to their centroid, plus the
// type/category the editor draws a ghost with. Leashes between balls of the
// prefab survive a stamp; a leash to a ball outside it is dropped at save,
// since that ball will not exist where the prefab lands. Triggers are room
// wiring and are not captured.

// EnsureDungeonPrefabTable creates the prefab table. Call once at startup.
func EnsureDungeonPrefabTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dunPrefabs (
			prefabID  INT PRIMARY KEY AUTO_INCREMENT,
			name      VARCHAR(100) NOT NULL,
			category  VARCHAR(64) NOT NULL DEFAULT '',
			ballCount INT NOT NULL DEFAULT 0,
			createdAt TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			data      MEDIUMTEXT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("EnsureDungeonPrefabTable: %w", err)
	}
	return nil
}

// PrefabItem is one ball of a prefab.
type PrefabItem struct {
	Snapshot   DunObjectSnapshot `json:"snapshot"`
	TypeName   string            `json:"typeName"`
	CategoryID int64             `json:"categoryID"`
	GroupID    int64             `json:"groupID"`
	Radius     float64           `json:"radius"`
}

// DunPrefab is a saved prefab. Items is nil in listings.
type DunPrefab struct {
	PrefabID  int64        `json:"prefabID"`
	Name      string       `json:"name"`
	Category  string       `json:"category"`
	BallCount int          `json:"ballCount"`
	CreatedAt string       `json:"createdAt"`
	Items     []PrefabItem `json:"items,omitempty"`
}

// CreatePrefabFromObjects captures the given balls as a new prefab.
func CreatePrefabFromObjects(ctx context.Context, db *sql.DB, name, category string, ids []int64) (int64, error) {
	name, category = strings.TrimSpace(name), strings.TrimSpace(category)
	if name == "" {
		return 0, fmt.Errorf("%w: a prefab needs a name", ErrObjectBatchConflict)
	}
	if len(name) > 100 {
		name = name[:100]
	}
	if len(category) > 64 {
		category = category[:64]
	}
	snaps, err := SnapshotObjects(ctx, db, ids)
	if err != nil {
		return 0, err
	}
	if len(snaps) == 0 {
		return 0, fmt.Errorf("%w: none of those balls exist", ErrObjectBatchConflict)
	}
	views, err := ESPGetObjectsByID(ctx, db, ids)
	if err != nil {
		return 0, err
	}
	viewByID := make(map[int64]ESPObjectRow, len(views))
	for _, v := range views {
		viewByID[v.ObjectID] = v
	}

	var cx, cy, cz float64
	inSet := make(map[int64]bool, len(snaps))
	for _, s := range snaps {
		cx, cy, cz = cx+s.X, cy+s.Y, cz+s.Z
		inSet[s.ObjectID] = true
	}
	n := float64(len(snaps))
	cx, cy, cz = cx/n, cy/n, cz/n

	items := make([]PrefabItem, len(snaps))
	for i, s := range snaps {
		s.X, s.Y, s.Z = s.X-cx, s.Y-cy, s.Z-cz
		if s.LeashObjectID != nil && !inSet[*s.LeashObjectID] {
			s.LeashObjectID = nil
		}
		v := viewByID[s.ObjectID]
		items[i] = PrefabItem{Snapshot: s, TypeName: v.TypeName, CategoryID: v.CategoryID, GroupID: v.GroupID, Radius: v.Radius}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO dunPrefabs (name, category, ballCount, data) VALUES (?, ?, ?, ?)`,
		name, category, len(items), string(data))
	if err != nil {
		return 0, fmt.Errorf("CreatePrefabFromObjects: %w", err)
	}
	return res.LastInsertId()
}

// ListPrefabs returns every prefab without its items.
func ListPrefabs(ctx context.Context, db *sql.DB) ([]DunPrefab, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT prefabID, name, category, ballCount, DATE_FORMAT(createdAt, '%Y-%m-%d %H:%i')
		   FROM dunPrefabs ORDER BY category, name`)
	if err != nil {
		return nil, fmt.Errorf("ListPrefabs: %w", err)
	}
	defer rows.Close()
	var out []DunPrefab
	for rows.Next() {
		var p DunPrefab
		if err := rows.Scan(&p.PrefabID, &p.Name, &p.Category, &p.BallCount, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListPrefabs scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPrefab returns one prefab with its items.
func GetPrefab(ctx context.Context, db *sql.DB, prefabID int64) (*DunPrefab, error) {
	var p DunPrefab
	var data string
	err := db.QueryRowContext(ctx,
		`SELECT prefabID, name, category, ballCount, DATE_FORMAT(createdAt, '%Y-%m-%d %H:%i'), data
		   FROM dunPrefabs WHERE prefabID = ?`, prefabID).
		Scan(&p.PrefabID, &p.Name, &p.Category, &p.BallCount, &p.CreatedAt, &data)
	if err != nil {
		return nil, fmt.Errorf("GetPrefab %d: %w", prefabID, err)
	}
	if err := json.Unmarshal([]byte(data), &p.Items); err != nil {
		return nil, fmt.Errorf("GetPrefab %d data: %w", prefabID, err)
	}
	return &p, nil
}

// DeletePrefab removes a prefab. Balls already stamped from it are untouched.
func DeletePrefab(ctx context.Context, db *sql.DB, prefabID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM dunPrefabs WHERE prefabID = ?`, prefabID); err != nil {
		return fmt.Errorf("DeletePrefab %d: %w", prefabID, err)
	}
	return nil
}
