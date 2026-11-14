package db

// The Intergalactic Summit: Galnet's one public board (esp/summit.go). Anyone reads it; a pilot
// writes only after proving who they are with a key the Summit mails them, because the in-game
// browser's identity headers are forgeable (the SCC brokerage key's scheme, sccaccount.go). A key
// entered once opens a session the browser keeps as a cookie.
//
// Author names and corporations are snapshots taken at posting time, never the headers: a corp
// renames, a roster is deleted, and a post must still say who wrote it. Times are unix seconds.
// Moderation (sticky, lock, delete, edit, ban) is the ESP Forum page's; nothing is ever removed,
// only flagged, so a deletion can be undone.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var summitTableDDL = []string{
	`summitThreads (
	threadID       BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
	title          VARCHAR(100) NOT NULL,
	authorCharID   BIGINT NOT NULL,
	authorName     VARCHAR(64) NOT NULL DEFAULT '',
	authorCorpID   BIGINT NOT NULL DEFAULT 0,
	authorCorpName VARCHAR(100) NOT NULL DEFAULT '',
	createdAt      BIGINT NOT NULL,
	lastPostAt     BIGINT NOT NULL,
	lastPostCharID BIGINT NOT NULL DEFAULT 0,
	lastPostName   VARCHAR(64) NOT NULL DEFAULT '',
	replyCount     INT NOT NULL DEFAULT 0,
	sticky         TINYINT NOT NULL DEFAULT 0,
	locked         TINYINT NOT NULL DEFAULT 0,
	deleted        TINYINT NOT NULL DEFAULT 0,
	KEY idx_summit_list (deleted, sticky, lastPostAt)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`summitPosts (
	postID         BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
	threadID       BIGINT NOT NULL,
	authorCharID   BIGINT NOT NULL,
	authorName     VARCHAR(64) NOT NULL DEFAULT '',
	authorCorpID   BIGINT NOT NULL DEFAULT 0,
	authorCorpName VARCHAR(100) NOT NULL DEFAULT '',
	body           TEXT NOT NULL,
	createdAt      BIGINT NOT NULL,
	editedAt       BIGINT NOT NULL DEFAULT 0,
	deleted        TINYINT NOT NULL DEFAULT 0,
	KEY idx_summit_thread (threadID, postID),
	KEY idx_summit_author (authorCharID)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`summitAccounts (
	charID     BIGINT NOT NULL PRIMARY KEY,
	keyHash    CHAR(64) NOT NULL DEFAULT '',
	issuedAt   BIGINT NOT NULL DEFAULT 0,
	lastUsedAt BIGINT NOT NULL DEFAULT 0,
	failCount  INT NOT NULL DEFAULT 0,
	failAt     BIGINT NOT NULL DEFAULT 0,
	banned     TINYINT NOT NULL DEFAULT 0,
	banReason  VARCHAR(200) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`summitSessions (
	tokenHash CHAR(64) NOT NULL PRIMARY KEY,
	charID    BIGINT NOT NULL,
	expiresAt BIGINT NOT NULL,
	KEY idx_summit_session_char (charID)
) ENGINE=InnoDB`,
}

// SummitTableDefinitions is the body of every Summit table after "CREATE TABLE IF NOT EXISTS ",
// so tests can create TEMPORARY copies.
func SummitTableDefinitions() []string { return append([]string(nil), summitTableDDL...) }

// EnsureSummitTables creates the Summit's tables. Call once at boot.
func EnsureSummitTables(ctx context.Context, db *sql.DB) error {
	for _, ddl := range summitTableDDL {
		if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+ddl); err != nil {
			return fmt.Errorf("EnsureSummitTables: %w", err)
		}
	}
	return nil
}

// summitNow is the Summit's clock, a variable so tests can pin it.
var summitNow = func() int64 { return time.Now().Unix() }

const (
	SummitTitleMax       = 100 // runes
	SummitBodyMax        = 6000
	SummitThreadsPerPage = 30
	SummitPostsPerPage   = 20
	SummitSessionSecs    = 12 * 3600
	summitKeyPrefix      = "IGS"
	summitKeyScope       = "summit-key:"
)

// Summit refusals. Their text is shown to the pilot as written.
var (
	ErrSummitNoAccount = errors.New("the Summit has not issued you a key yet")
	ErrSummitBadKey    = errors.New("that Summit key is not right")
	ErrSummitKeyLocked = errors.New("too many wrong keys: try again in fifteen minutes")
	ErrSummitBanned    = errors.New("the moderators have withdrawn your right to address the Summit")
	ErrSummitLocked    = errors.New("this thread has been closed by the moderators")
	ErrSummitNoThread  = errors.New("no such thread")
	ErrSummitEmpty     = errors.New("there is nothing to post")
	ErrSummitNoPilot   = errors.New("the Summit does not know that capsuleer")
)

// ── keys and sessions ───────────────────────────────────────────────────────

func hashSummitKey(k string) string {
	return hashMailedKey(summitKeyScope, normalizeMailedKey(k, summitKeyPrefix))
}

// IssueSummitKey gives a pilot a new key (the old one stops working) and returns it in clear, the
// only time it exists in clear. The caller mails it. A ban survives a re-key.
func IssueSummitKey(ctx context.Context, db *sql.DB, charID int64) (string, error) {
	if charID <= 0 {
		return "", ErrSummitNoPilot
	}
	key, err := newMailedKey(summitKeyPrefix)
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO summitAccounts (charID, keyHash, issuedAt, failCount, failAt) VALUES (?, ?, ?, 0, 0)
		ON DUPLICATE KEY UPDATE keyHash = VALUES(keyHash), issuedAt = VALUES(issuedAt), failCount = 0, failAt = 0`,
		charID, hashSummitKey(key), summitNow())
	if err != nil {
		return "", fmt.Errorf("IssueSummitKey: %w", err)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM summitSessions WHERE charID = ?`, charID)
	return key, nil
}

// HasSummitKey reports whether a pilot holds a live key.
func HasSummitKey(ctx context.Context, db DBTX, charID int64) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitAccounts WHERE charID = ? AND keyHash <> ''`, charID).Scan(&n)
	return n > 0
}

// SummitBan reports whether a pilot is barred from posting, and why.
func SummitBan(ctx context.Context, db DBTX, charID int64) (bool, string) {
	var banned int
	var reason string
	_ = db.QueryRowContext(ctx, `SELECT banned, banReason FROM summitAccounts WHERE charID = ?`, charID).Scan(&banned, &reason)
	return banned != 0, reason
}

// VerifySummitKey checks a key against a pilot's account. Five wrong keys lock it for fifteen
// minutes, as the SCC's do.
func VerifySummitKey(ctx context.Context, db *sql.DB, charID int64, key string) error {
	var hash string
	var fails, banned int
	var failAt int64
	err := db.QueryRowContext(ctx, `SELECT keyHash, failCount, failAt, banned FROM summitAccounts WHERE charID = ?`, charID).
		Scan(&hash, &fails, &failAt, &banned)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && hash == "") {
		return ErrSummitNoAccount
	}
	if err != nil {
		return err
	}
	if banned != 0 {
		return ErrSummitBanned
	}
	now := summitNow()
	if fails >= mailedKeyMaxFails && now-failAt < mailedKeyLockSecs {
		return ErrSummitKeyLocked
	}
	if normalizeMailedKey(key, summitKeyPrefix) != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(hashSummitKey(key))) == 1 {
		_, _ = db.ExecContext(ctx, `UPDATE summitAccounts SET lastUsedAt = ?, failCount = 0 WHERE charID = ?`, now, charID)
		return nil
	}
	if now-failAt >= mailedKeyLockSecs {
		fails = 0
	}
	_, _ = db.ExecContext(ctx, `UPDATE summitAccounts SET failCount = ?, failAt = ? WHERE charID = ?`, fails+1, now, charID)
	return ErrSummitBadKey
}

// NewSummitSession opens a session for a pilot whose key was just verified and returns its token
// in clear (the cookie value); only its hash is stored. Expired sessions are swept on the way.
func NewSummitSession(ctx context.Context, db *sql.DB, charID int64) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	now := summitNow()
	_, _ = db.ExecContext(ctx, `DELETE FROM summitSessions WHERE expiresAt < ?`, now)
	if _, err := db.ExecContext(ctx, `INSERT INTO summitSessions (tokenHash, charID, expiresAt) VALUES (?, ?, ?)`,
		hashMailedKey("summit-session:", token), charID, now+SummitSessionSecs); err != nil {
		return "", fmt.Errorf("NewSummitSession: %w", err)
	}
	return token, nil
}

// SummitSessionChar is the pilot a live session token belongs to, or 0.
func SummitSessionChar(ctx context.Context, db DBTX, token string) int64 {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0
	}
	var charID int64
	_ = db.QueryRowContext(ctx, `SELECT charID FROM summitSessions WHERE tokenHash = ? AND expiresAt >= ?`,
		hashMailedKey("summit-session:", token), summitNow()).Scan(&charID)
	return charID
}

// ── threads and posts ───────────────────────────────────────────────────────

// SummitThread is one thread's header row.
type SummitThread struct {
	ThreadID       int64
	Title          string
	AuthorCharID   int64
	AuthorName     string
	AuthorCorpID   int64
	AuthorCorpName string
	CreatedAt      int64
	LastPostAt     int64
	LastPostCharID int64
	LastPostName   string
	ReplyCount     int
	Sticky         bool
	Locked         bool
	Deleted        bool
}

// SummitPost is one post.
type SummitPost struct {
	PostID         int64
	ThreadID       int64
	AuthorCharID   int64
	AuthorName     string
	AuthorCorpID   int64
	AuthorCorpName string
	Body           string
	CreatedAt      int64
	EditedAt       int64
	Deleted        bool
}

const summitThreadCols = `threadID, title, authorCharID, authorName, authorCorpID, authorCorpName, createdAt,
	lastPostAt, lastPostCharID, lastPostName, replyCount, sticky, locked, deleted`

func scanSummitThread(sc interface{ Scan(...any) error }) (SummitThread, error) {
	var t SummitThread
	var sticky, locked, deleted int
	err := sc.Scan(&t.ThreadID, &t.Title, &t.AuthorCharID, &t.AuthorName, &t.AuthorCorpID, &t.AuthorCorpName,
		&t.CreatedAt, &t.LastPostAt, &t.LastPostCharID, &t.LastPostName, &t.ReplyCount, &sticky, &locked, &deleted)
	t.Sticky, t.Locked, t.Deleted = sticky != 0, locked != 0, deleted != 0
	return t, err
}

const summitPostCols = `postID, threadID, authorCharID, authorName, authorCorpID, authorCorpName, body, createdAt, editedAt, deleted`

func scanSummitPost(sc interface{ Scan(...any) error }) (SummitPost, error) {
	var p SummitPost
	var deleted int
	err := sc.Scan(&p.PostID, &p.ThreadID, &p.AuthorCharID, &p.AuthorName, &p.AuthorCorpID, &p.AuthorCorpName,
		&p.Body, &p.CreatedAt, &p.EditedAt, &deleted)
	p.Deleted = deleted != 0
	return p, err
}

// summitPoster is who a post is signed by, read from the database at posting time.
func summitPoster(ctx context.Context, tx *sql.Tx, charID int64) (name string, corpID int64, corpName string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT characterName, corporationID FROM character_ WHERE characterID = ?`, charID).
		Scan(&name, &corpID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", ErrSummitNoPilot
	}
	if err != nil {
		return "", 0, "", err
	}
	_ = tx.QueryRowContext(ctx, `SELECT corporationName FROM corporation WHERE corporationID = ?`, corpID).Scan(&corpName)
	return name, corpID, corpName, nil
}

// CreateSummitThread opens a thread with its first post, in one transaction. title and body are
// already cleaned (esp/summitform.go); this only refuses empties and enforces the caps.
func CreateSummitThread(ctx context.Context, db *sql.DB, charID int64, title, body string) (int64, error) {
	title, body = clampRunes(strings.TrimSpace(title), SummitTitleMax), clampRunes(strings.TrimSpace(body), SummitBodyMax)
	if title == "" || body == "" {
		return 0, ErrSummitEmpty
	}
	if banned, _ := SummitBan(ctx, db, charID); banned {
		return 0, ErrSummitBanned
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	name, corpID, corpName, err := summitPoster(ctx, tx, charID)
	if err != nil {
		return 0, err
	}
	now := summitNow()
	res, err := tx.ExecContext(ctx, `INSERT INTO summitThreads (title, authorCharID, authorName, authorCorpID, authorCorpName,
		createdAt, lastPostAt, lastPostCharID, lastPostName) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		title, charID, name, corpID, corpName, now, now, charID, name)
	if err != nil {
		return 0, fmt.Errorf("CreateSummitThread: %w", err)
	}
	threadID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO summitPosts (threadID, authorCharID, authorName, authorCorpID, authorCorpName,
		body, createdAt) VALUES (?, ?, ?, ?, ?, ?, ?)`, threadID, charID, name, corpID, corpName, body, now); err != nil {
		return 0, fmt.Errorf("CreateSummitThread post: %w", err)
	}
	return threadID, tx.Commit()
}

// ReplySummitThread adds a post to an open thread and returns the 0-based page it landed on. The
// thread row is locked on its own (no JOIN, which would lock joined rows too) so two replies bump
// the counters one after the other.
func ReplySummitThread(ctx context.Context, db *sql.DB, threadID, charID int64, body string) (page int, err error) {
	body = clampRunes(strings.TrimSpace(body), SummitBodyMax)
	if body == "" {
		return 0, ErrSummitEmpty
	}
	if banned, _ := SummitBan(ctx, db, charID); banned {
		return 0, ErrSummitBanned
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var locked, deleted int
	err = tx.QueryRowContext(ctx, `SELECT locked, deleted FROM summitThreads WHERE threadID = ? FOR UPDATE`, threadID).
		Scan(&locked, &deleted)
	if errors.Is(err, sql.ErrNoRows) || deleted != 0 {
		return 0, ErrSummitNoThread
	}
	if err != nil {
		return 0, err
	}
	if locked != 0 {
		return 0, ErrSummitLocked
	}
	name, corpID, corpName, err := summitPoster(ctx, tx, charID)
	if err != nil {
		return 0, err
	}
	now := summitNow()
	if _, err := tx.ExecContext(ctx, `INSERT INTO summitPosts (threadID, authorCharID, authorName, authorCorpID, authorCorpName,
		body, createdAt) VALUES (?, ?, ?, ?, ?, ?, ?)`, threadID, charID, name, corpID, corpName, body, now); err != nil {
		return 0, fmt.Errorf("ReplySummitThread: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE summitThreads SET replyCount = replyCount + 1, lastPostAt = ?,
		lastPostCharID = ?, lastPostName = ? WHERE threadID = ?`, now, charID, name, threadID); err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitPosts WHERE threadID = ? AND deleted = 0`, threadID).Scan(&n); err != nil {
		return 0, err
	}
	return SummitLastPage(n), tx.Commit()
}

// SummitLastPage is the 0-based page the last of n posts sits on.
func SummitLastPage(n int) int {
	if n <= 0 {
		return 0
	}
	return (n - 1) / SummitPostsPerPage
}

// ListSummitThreads is one page of the board: sticky threads first, then by latest activity.
// withDeleted is the moderators' view.
func ListSummitThreads(ctx context.Context, db DBTX, page int, withDeleted bool) ([]SummitThread, int, error) {
	where := "deleted = 0"
	if withDeleted {
		where = "1 = 1"
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitThreads WHERE `+where).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+summitThreadCols+` FROM summitThreads WHERE `+where+`
		ORDER BY sticky DESC, lastPostAt DESC, threadID DESC LIMIT ? OFFSET ?`,
		SummitThreadsPerPage, max(page, 0)*SummitThreadsPerPage)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []SummitThread
	for rows.Next() {
		t, err := scanSummitThread(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// LatestSummitThreads is the n most recently active threads, sticky or not (the Galnet front door).
func LatestSummitThreads(ctx context.Context, db DBTX, n int) ([]SummitThread, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+summitThreadCols+` FROM summitThreads WHERE deleted = 0
		ORDER BY lastPostAt DESC, threadID DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SummitThread
	for rows.Next() {
		t, err := scanSummitThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetSummitThread reads one thread. A deleted thread is ErrSummitNoThread unless withDeleted.
func GetSummitThread(ctx context.Context, db DBTX, threadID int64, withDeleted bool) (*SummitThread, error) {
	t, err := scanSummitThread(db.QueryRowContext(ctx, `SELECT `+summitThreadCols+` FROM summitThreads WHERE threadID = ?`, threadID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && t.Deleted && !withDeleted) {
		return nil, ErrSummitNoThread
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListSummitPosts is one page of a thread, oldest first, and the thread's post total. page < 0
// lists every post (the moderators' view).
func ListSummitPosts(ctx context.Context, db DBTX, threadID int64, page int, withDeleted bool) ([]SummitPost, int, error) {
	where := "threadID = ? AND deleted = 0"
	if withDeleted {
		where = "threadID = ?"
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitPosts WHERE `+where, threadID).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT ` + summitPostCols + ` FROM summitPosts WHERE ` + where + ` ORDER BY postID`
	args := []any{threadID}
	if page >= 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, SummitPostsPerPage, page*SummitPostsPerPage)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []SummitPost
	for rows.Next() {
		p, err := scanSummitPost(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// ── moderation (ESP Forum) ──────────────────────────────────────────────────

// SetSummitThreadFlags sets a thread's sticky and locked flags.
func SetSummitThreadFlags(ctx context.Context, db *sql.DB, threadID int64, sticky, locked bool) error {
	_, err := db.ExecContext(ctx, `UPDATE summitThreads SET sticky = ?, locked = ? WHERE threadID = ?`,
		boolInt(sticky), boolInt(locked), threadID)
	return err
}

// SetSummitThreadDeleted hides or restores a whole thread.
func SetSummitThreadDeleted(ctx context.Context, db *sql.DB, threadID int64, deleted bool) error {
	_, err := db.ExecContext(ctx, `UPDATE summitThreads SET deleted = ? WHERE threadID = ?`, boolInt(deleted), threadID)
	return err
}

// SetSummitPostDeleted hides or restores one post and recounts its thread, so the reply count and
// the "last post" line never name a post nobody can see.
func SetSummitPostDeleted(ctx context.Context, db *sql.DB, postID int64, deleted bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var threadID int64
	if err := tx.QueryRowContext(ctx, `SELECT threadID FROM summitPosts WHERE postID = ?`, postID).Scan(&threadID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT threadID FROM summitThreads WHERE threadID = ? FOR UPDATE`, threadID).Scan(&threadID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE summitPosts SET deleted = ? WHERE postID = ?`, boolInt(deleted), postID); err != nil {
		return err
	}
	if err := recountSummitThread(ctx, tx, threadID); err != nil {
		return err
	}
	return tx.Commit()
}

// recountSummitThread rebuilds a thread's reply count and last-post line from its visible posts. A
// thread with none left keeps its old line; the moderators delete the thread itself for that.
func recountSummitThread(ctx context.Context, tx *sql.Tx, threadID int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitPosts WHERE threadID = ? AND deleted = 0`, threadID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE summitThreads SET replyCount = 0 WHERE threadID = ?`, threadID)
		return err
	}
	var at, charID int64
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT createdAt, authorCharID, authorName FROM summitPosts
		WHERE threadID = ? AND deleted = 0 ORDER BY postID DESC LIMIT 1`, threadID).Scan(&at, &charID, &name); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE summitThreads SET replyCount = ?, lastPostAt = ?, lastPostCharID = ?, lastPostName = ?
		WHERE threadID = ?`, n-1, at, charID, name, threadID)
	return err
}

// EditSummitPost replaces a post's text (moderators only) and stamps the edit.
func EditSummitPost(ctx context.Context, db *sql.DB, postID int64, body string) error {
	body = clampRunes(strings.TrimSpace(body), SummitBodyMax)
	if body == "" {
		return ErrSummitEmpty
	}
	_, err := db.ExecContext(ctx, `UPDATE summitPosts SET body = ?, editedAt = ? WHERE postID = ?`, body, summitNow(), postID)
	return err
}

// SetSummitBan bars or readmits a pilot. A ban ends their sessions; a pilot with no account yet
// gets one, keyless, so the ban is waiting for them.
func SetSummitBan(ctx context.Context, db *sql.DB, charID int64, banned bool, reason string) error {
	reason = clampRunes(strings.TrimSpace(reason), 200)
	if !banned {
		reason = ""
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO summitAccounts (charID, banned, banReason) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE banned = VALUES(banned), banReason = VALUES(banReason)`, charID, boolInt(banned), reason); err != nil {
		return err
	}
	if banned {
		_, _ = db.ExecContext(ctx, `DELETE FROM summitSessions WHERE charID = ?`, charID)
	}
	return nil
}

// RevokeSummitKey voids a pilot's key and sessions; they can ask for a new one.
func RevokeSummitKey(ctx context.Context, db *sql.DB, charID int64) error {
	if _, err := db.ExecContext(ctx, `UPDATE summitAccounts SET keyHash = '' WHERE charID = ?`, charID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM summitSessions WHERE charID = ?`, charID)
	return err
}

// SummitAccountRow is one pilot known to the Summit, for the moderators.
type SummitAccountRow struct {
	CharID     int64
	Name       string
	HasKey     bool
	IssuedAt   int64
	LastUsedAt int64
	FailCount  int
	Banned     bool
	BanReason  string
	Posts      int
}

// ListSummitAccounts lists every pilot holding a key or a ban, most recently active first.
func ListSummitAccounts(ctx context.Context, db DBTX, limit int) ([]SummitAccountRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.charID, COALESCE(c.characterName, ''), a.keyHash <> '', a.issuedAt, a.lastUsedAt,
		a.failCount, a.banned, a.banReason,
		(SELECT COUNT(*) FROM summitPosts p WHERE p.authorCharID = a.charID AND p.deleted = 0)
		FROM summitAccounts a LEFT JOIN character_ c ON c.characterID = a.charID
		ORDER BY GREATEST(a.lastUsedAt, a.issuedAt) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SummitAccountRow
	for rows.Next() {
		var r SummitAccountRow
		var hasKey, banned int
		if err := rows.Scan(&r.CharID, &r.Name, &hasKey, &r.IssuedAt, &r.LastUsedAt, &r.FailCount, &banned, &r.BanReason, &r.Posts); err != nil {
			return nil, err
		}
		r.HasKey, r.Banned = hasKey != 0, banned != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SummitCounts is the board's size, for the moderators' page.
func SummitCounts(ctx context.Context, db DBTX) (threads, posts, pilots int) {
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitThreads WHERE deleted = 0`).Scan(&threads)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM summitPosts WHERE deleted = 0`).Scan(&posts)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT authorCharID) FROM summitPosts WHERE deleted = 0`).Scan(&pilots)
	return
}
