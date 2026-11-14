package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/marshal"
)

var attributeTypeCols = []string{
	"attributeID", "attributeName", "attributeCategory", "description",
	"maxAttributeID", "attributeIdx", "graphicID", "chargeRechargeTimeID",
	"defaultValue", "published", "displayName", "unitID", "stackable", "highIsGood",
}

// GetAttributeTypes mirrors DogmaDB::GetAttributeTypes
// (DBResultToIndexRowset("attributeName")): a util.IndexRowset keyed by
// attributeName from dgmAttributeTypes.
func GetAttributeTypes(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT attributeID, attributeName, attributeCategory, description,
		        maxAttributeID, attributeIdx, graphicID, chargeRechargeTimeID,
		        defaultValue, published, displayName, unitID, stackable, highIsGood
		 FROM dgmAttributeTypes`)
	if err != nil {
		return nil, fmt.Errorf("GetAttributeTypes: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			attributeID          int64
			attributeName        string
			attributeCategory    sql.NullInt64
			description          sql.NullString
			maxAttributeID       sql.NullInt64
			attributeIdx         sql.NullInt64
			graphicID            sql.NullInt64
			chargeRechargeTimeID sql.NullInt64
			defaultValue         sql.NullFloat64
			published            sql.NullInt64
			displayName          sql.NullString
			unitID               sql.NullInt64
			stackable            sql.NullInt64
			highIsGood           sql.NullInt64
		)
		if err := rows.Scan(
			&attributeID, &attributeName, &attributeCategory, &description,
			&maxAttributeID, &attributeIdx, &graphicID, &chargeRechargeTimeID,
			&defaultValue, &published, &displayName, &unitID, &stackable, &highIsGood,
		); err != nil {
			return nil, fmt.Errorf("GetAttributeTypes scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(attributeID), strRep(attributeName), nullIntRep(attributeCategory), nullStrRep(description),
			nullIntRep(maxAttributeID), nullIntRep(attributeIdx), nullIntRep(graphicID), nullIntRep(chargeRechargeTimeID),
			nullFloatRep(defaultValue), nullIntRep(published), nullStrRep(displayName), nullIntRep(unitID),
			nullIntRep(stackable), nullIntRep(highIsGood),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetAttributeTypes rows: %w", err)
	}
	return makeIndexRowset("attributeName", attributeTypeCols, result), nil
}
