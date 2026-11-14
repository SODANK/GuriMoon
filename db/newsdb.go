package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// NewsArticle is one row of the eve_news table.  Body is a raw EVE-HTML
// fragment rendered by the in-game browser (char select news panel).
type NewsArticle struct {
	ID            int64
	Date          time.Time
	Title         string
	Author        string // shown as "reported by: <author>"
	Body          string
	ImageURL      string // optional right-floated article image
	ShowTicker    bool
	ShowBillboard bool // push article image onto in-space billboard screens
	Published     bool
	// CorpID tags the story with a corporation, whose logo The Scope prints
	// beside the headline. 0 = untagged.
	CorpID int64
	// Section is the desk the story files under (NewsSections), "" for none.
	Section string
}

// NewsSections are The Scope's desks, in menu order. A story's Section is one
// of these or empty.
var NewsSections = []string{"Politics", "Business", "Security", "Science and Industry", "Capsuleer Affairs"}

// EnsureNewsTable creates the eve_news table if it does not already exist.
// Call once at server startup after opening the DB.
func EnsureNewsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS eve_news (
			newsID      INT PRIMARY KEY AUTO_INCREMENT,
			publishDate DATETIME NOT NULL,
			title       VARCHAR(255) NOT NULL,
			author      VARCHAR(64) NOT NULL DEFAULT '',
			body        TEXT NOT NULL,
			imageURL    VARCHAR(255) NOT NULL DEFAULT '',
			showTicker  TINYINT NOT NULL DEFAULT 1,
			showBillboard TINYINT NOT NULL DEFAULT 0,
			published   TINYINT NOT NULL DEFAULT 1
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureNewsTable: %w", err)
	}

	// Add columns if table already existed without them (ignore errors).
	db.ExecContext(ctx, `ALTER TABLE eve_news ADD COLUMN author VARCHAR(64) NOT NULL DEFAULT '' AFTER title`)
	db.ExecContext(ctx, `ALTER TABLE eve_news ADD COLUMN imageURL VARCHAR(255) NOT NULL DEFAULT '' AFTER body`)
	db.ExecContext(ctx, `ALTER TABLE eve_news ADD COLUMN showBillboard TINYINT NOT NULL DEFAULT 0 AFTER showTicker`)
	db.ExecContext(ctx, `ALTER TABLE eve_news ADD COLUMN corpID INT NULL AFTER published`)
	db.ExecContext(ctx, `ALTER TABLE eve_news ADD COLUMN section VARCHAR(40) NOT NULL DEFAULT '' AFTER corpID`)
	return nil
}

func scanNewsRows(rows *sql.Rows) ([]NewsArticle, error) {
	var out []NewsArticle
	for rows.Next() {
		var a NewsArticle
		var ticker, billboard, published int
		var corpID sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Date, &a.Title, &a.Author, &a.Body, &a.ImageURL, &ticker, &billboard, &published,
			&corpID, &a.Section); err != nil {
			return nil, err
		}
		a.CorpID = corpID.Int64
		a.ShowTicker = ticker != 0
		a.ShowBillboard = billboard != 0
		a.Published = published != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// newsColumns is the SELECT column list matching scanNewsRows.
const newsColumns = `newsID, publishDate, title, author, body, imageURL, showTicker, showBillboard, published, corpID, section`

// GetPublishedNews returns published articles newest-first, up to limit.
func GetPublishedNews(db *sql.DB, limit int) ([]NewsArticle, error) {
	rows, err := db.Query(`
		SELECT `+newsColumns+`
		FROM eve_news WHERE published = 1
		ORDER BY publishDate DESC, newsID DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("GetPublishedNews: %w", err)
	}
	defer rows.Close()
	return scanNewsRows(rows)
}

// GetTickerHeadlines returns published ticker-enabled articles newest-first.
func GetTickerHeadlines(db *sql.DB, limit int) ([]NewsArticle, error) {
	rows, err := db.Query(`
		SELECT `+newsColumns+`
		FROM eve_news WHERE published = 1 AND showTicker = 1
		ORDER BY publishDate DESC, newsID DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("GetTickerHeadlines: %w", err)
	}
	defer rows.Close()
	return scanNewsRows(rows)
}

// GetBillboardArticle returns the newest published article flagged for the
// in-space billboard screen that has an image set, or nil if there is none.
func GetBillboardArticle(db *sql.DB) (*NewsArticle, error) {
	rows, err := db.Query(`
		SELECT ` + newsColumns + `
		FROM eve_news WHERE published = 1 AND showBillboard = 1 AND imageURL <> ''
		ORDER BY publishDate DESC, newsID DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("GetBillboardArticle: %w", err)
	}
	defer rows.Close()
	articles, err := scanNewsRows(rows)
	if err != nil {
		return nil, fmt.Errorf("GetBillboardArticle: %w", err)
	}
	if len(articles) == 0 {
		return nil, nil
	}
	return &articles[0], nil
}

// GetAllNews returns every article (including drafts) newest-first.
func GetAllNews(db *sql.DB) ([]NewsArticle, error) {
	rows, err := db.Query(`
		SELECT ` + newsColumns + `
		FROM eve_news ORDER BY publishDate DESC, newsID DESC`)
	if err != nil {
		return nil, fmt.Errorf("GetAllNews: %w", err)
	}
	defer rows.Close()
	return scanNewsRows(rows)
}

// GetNewsArticle returns a single article by ID.
func GetNewsArticle(db *sql.DB, id int64) (*NewsArticle, error) {
	rows, err := db.Query(`
		SELECT `+newsColumns+`
		FROM eve_news WHERE newsID = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("GetNewsArticle: %w", err)
	}
	defer rows.Close()
	articles, err := scanNewsRows(rows)
	if err != nil {
		return nil, fmt.Errorf("GetNewsArticle: %w", err)
	}
	if len(articles) == 0 {
		return nil, nil
	}
	return &articles[0], nil
}

// InsertNews creates a new article and returns its ID.
func InsertNews(db *sql.DB, a NewsArticle) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO eve_news (publishDate, title, author, body, imageURL, showTicker, showBillboard, published, corpID, section)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Date.Format("2006-01-02 15:04:05"), a.Title, a.Author, a.Body, a.ImageURL,
		boolToInt(a.ShowTicker), boolToInt(a.ShowBillboard), boolToInt(a.Published), nullCorp(a.CorpID), a.Section)
	if err != nil {
		return 0, fmt.Errorf("InsertNews: %w", err)
	}
	return res.LastInsertId()
}

// UpdateNews updates an existing article by ID.
func UpdateNews(db *sql.DB, a NewsArticle) error {
	_, err := db.Exec(`
		UPDATE eve_news SET publishDate = ?, title = ?, author = ?, body = ?, imageURL = ?, showTicker = ?, showBillboard = ?, published = ?,
		       corpID = ?, section = ?
		WHERE newsID = ?`,
		a.Date.Format("2006-01-02 15:04:05"), a.Title, a.Author, a.Body, a.ImageURL,
		boolToInt(a.ShowTicker), boolToInt(a.ShowBillboard), boolToInt(a.Published), nullCorp(a.CorpID), a.Section, a.ID)
	if err != nil {
		return fmt.Errorf("UpdateNews: %w", err)
	}
	return err
}

// DeleteNews removes an article by ID.
func DeleteNews(db *sql.DB, id int64) error {
	if _, err := db.Exec(`DELETE FROM eve_news WHERE newsID = ?`, id); err != nil {
		return fmt.Errorf("DeleteNews: %w", err)
	}
	return nil
}

// NewsCorpChoice is one entry of the admin form's corporation picker.
type NewsCorpChoice struct {
	ID   int64
	Name string
}

// NewsCorpChoices lists every corporation a story can be tagged with.
func NewsCorpChoices(ctx context.Context, db *sql.DB) ([]NewsCorpChoice, error) {
	rows, err := db.QueryContext(ctx, `SELECT corporationID, corporationName FROM corporation
		WHERE deleted = 0 ORDER BY corporationName`)
	if err != nil {
		return nil, fmt.Errorf("NewsCorpChoices: %w", err)
	}
	defer rows.Close()
	var out []NewsCorpChoice
	for rows.Next() {
		var c NewsCorpChoice
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, fmt.Errorf("NewsCorpChoices scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nullCorp stores an untagged story as NULL rather than 0.
func nullCorp(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// GetPublishedNewsInSection is GetPublishedNews narrowed to one desk.
func GetPublishedNewsInSection(db *sql.DB, section string, limit int) ([]NewsArticle, error) {
	rows, err := db.Query(`
		SELECT `+newsColumns+`
		FROM eve_news WHERE published = 1 AND section = ?
		ORDER BY publishDate DESC, newsID DESC LIMIT ?`, section, limit)
	if err != nil {
		return nil, fmt.Errorf("GetPublishedNewsInSection: %w", err)
	}
	defer rows.Close()
	return scanNewsRows(rows)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
