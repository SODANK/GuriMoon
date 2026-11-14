package db

import (
	"context"
	"database/sql"

	"gurimoon/marshal"
)

// EnsureTutorialTables creates the chrTutorials and chrTutorialDisabled tables
// if they don't exist.  Called once at startup.
func EnsureTutorialTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrTutorials (
			characterID BIGINT NOT NULL,
			tutorialID INT NOT NULL,
			completedAt BIGINT NOT NULL,
			PRIMARY KEY (characterID, tutorialID),
			INDEX idx_char (characterID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	if err != nil {
		return err
	}
	// Characters who clicked "No" on the tutorial dismiss dialog (client-side showTutorials=0).
	// Bookkeeping only: GetTutorialInfo must always serve data, since None renders an error popup.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrTutorialDisabled (
			characterID BIGINT NOT NULL PRIMARY KEY,
			disabledAt  BIGINT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	return err
}

// DisableTutorials records that a character chose to permanently hide tutorials
// (clicked "No" on the TutorialContinueLater dialog → LogAborted).
func DisableTutorials(ctx context.Context, db *sql.DB, characterID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO chrTutorialDisabled (characterID, disabledAt) VALUES (?, UNIX_TIMESTAMP())`,
		characterID)
	return err
}

// AreTutorialsDisabled returns true if the character has permanently dismissed tutorials.
func AreTutorialsDisabled(ctx context.Context, db *sql.DB, characterID int64) bool {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chrTutorialDisabled WHERE characterID = ?`,
		characterID).Scan(&count)
	return err == nil && count > 0
}

// QueryTutorials mirrors TutorialDB::GetAllTutorials → DBResultToRowset:
//
//	SELECT tutorialID, tutorialVName, nextTutorialID FROM tutorials
func QueryTutorials(ctx context.Context, db *sql.DB, cols []string) (marshal.PyRep, error) {
	return queryRows(ctx, db, cols,
		`SELECT tutorialID, tutorialVName, nextTutorialID FROM tutorials`)
}

// QueryCriterias mirrors TutorialDB::GetAllCriterias → DBResultToRowset:
//
//	SELECT criteriaID, criteriaName, messageText, audioPath FROM tutorial_criteria
func QueryCriterias(ctx context.Context, db *sql.DB, cols []string) (marshal.PyRep, error) {
	return queryRows(ctx, db, cols,
		`SELECT criteriaID, criteriaName, messageText, audioPath FROM tutorial_criteria`)
}

// GetTutorialPageCriterias mirrors TutorialDB::GetPageCriterias.
func GetTutorialPageCriterias(ctx context.Context, db *sql.DB, tutorialID int64) (marshal.PyRep, error) {
	cols := []string{"pageID", "criteriaID"}
	return queryRows(ctx, db, cols, `
		SELECT tpc.pageID, tpc.criteriaID
		  FROM tutorial_page_criteria tpc
		  JOIN tutorial_pages tp ON tpc.pageID = tp.pageID
		 WHERE tp.tutorialID = ?`, tutorialID)
}

// GetTutorialPages mirrors TutorialDB::GetPages.
func GetTutorialPages(ctx context.Context, db *sql.DB, tutorialID int64) (marshal.PyRep, error) {
	cols := []string{"pageID", "pageNumber", "pageName", "text", "imagePath", "audioPath"}
	return queryRows(ctx, db, cols, `
		SELECT pageID, pageNumber, pageName, text, imagePath, audioPath
		  FROM tutorial_pages
		 WHERE tutorialID = ?
		 ORDER BY pageNumber`, tutorialID)
}

// GetTutorialRow mirrors TutorialDB::GetTutorial.
func GetTutorialRow(ctx context.Context, db *sql.DB, tutorialID int64) (marshal.PyRep, error) {
	cols := []string{"tutorialID", "tutorialVName", "nextTutorialID"}
	return queryRows(ctx, db, cols, `
		SELECT tutorialID, tutorialVName, nextTutorialID
		  FROM tutorials
		 WHERE tutorialID = ?`, tutorialID)
}

// TutorialExists returns true if the tutorials table has a row for tutorialID.
func TutorialExists(ctx context.Context, db *sql.DB, tutorialID int64) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tutorials WHERE tutorialID = ?`, tutorialID).Scan(&n)
	return err == nil && n > 0
}

// GetTutorialCriterias mirrors TutorialDB::GetTutorialCriterias.
func GetTutorialCriterias(ctx context.Context, db *sql.DB, tutorialID int64) (marshal.PyRep, error) {
	cols := []string{"criteriaID"}
	return queryRows(ctx, db, cols, `
		SELECT criteriaID
		  FROM tutorials_criterias
		 WHERE tutorialID = ?`, tutorialID)
}

// MarkTutorialComplete records that characterID has completed tutorialID.
// Table must exist (created at startup by EnsureTutorialTables).
func MarkTutorialComplete(ctx context.Context, db *sql.DB, characterID, tutorialID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO chrTutorials (characterID, tutorialID, completedAt) VALUES (?, ?, UNIX_TIMESTAMP())`,
		characterID, tutorialID)
	return err
}

// IsTutorialComplete returns true if characterID has completed tutorialID.
func IsTutorialComplete(ctx context.Context, db *sql.DB, characterID, tutorialID int64) bool {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chrTutorials WHERE characterID = ? AND tutorialID = ?`,
		characterID, tutorialID).Scan(&count)
	return err == nil && count > 0
}
