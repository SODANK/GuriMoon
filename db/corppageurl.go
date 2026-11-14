package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gurimoon/safego"
)

// GalnetHost is the in-game browser's address for the news server, which a hosts-file
// entry on the client machine points at this server.
const GalnetHost = "http://myeve.eve-online.com"

// GalnetPathTaken reports whether a top-level path on the news server belongs to a
// site (/galnet, /scope, ...), so no corporation may be addressed by it. Set by
// esp.NewNewsServer; nil means no news server, and no corporation is pointed at one.
var GalnetPathTaken func(segment string) bool

// CorpSlug is the URL form of a corporation name: letters and digits kept, apostrophes
// dropped ("Emperors", not "Emperor-s"), every other run of punctuation or space one
// hyphen. Case is kept; lookups compare folded. A name with no alphanumerics gives "".
func CorpSlug(name string) string {
	var b strings.Builder
	sep := false
	for _, r := range name {
		switch {
		case r == '\'' || r == '`' || r == '’':
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if sep && b.Len() > 0 {
				b.WriteByte('-')
			}
			sep = false
			b.WriteRune(r)
		default:
			sep = true
		}
	}
	return b.String()
}

// CorpSlugKey is the folded form slugs are compared in.
func CorpSlugKey(s string) string { return strings.ToLower(CorpSlug(s)) }

// CorpProfileURL is a corporation's Galnet page by ID, the fallback when its name
// cannot be an address.
func CorpProfileURL(corpID int64) string {
	return GalnetHost + "/corp/profile?id=" + strconv.FormatInt(corpID, 10)
}

// CorpNameRow is one corporation's ID and name.
type CorpNameRow struct {
	ID   int64
	Name string
}

// ListCorpNames returns every corporation, NPC ones included, for the news server's
// name-address table.
func ListCorpNames(ctx context.Context, q querier) ([]CorpNameRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT corporationID, corporationName FROM corporation`)
	if err != nil {
		return nil, fmt.Errorf("ListCorpNames: %w", err)
	}
	defer rows.Close()
	var out []CorpNameRow
	for rows.Next() {
		var r CorpNameRow
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return nil, fmt.Errorf("ListCorpNames scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// querier is the read half shared by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// corpGalnetURL is the address a corporation called name gets: its slug, or the ID
// form when the slug is empty, belongs to a site, or is another corporation's (slugs are
// not unique: "Foo Bar" and "Foo-Bar" fold together, and the news server answers a shared
// slug with the lowest ID). "" with no news server.
func corpGalnetURL(ctx context.Context, q querier, corpID int64, name string) (string, error) {
	if GalnetPathTaken == nil {
		return "", nil
	}
	slug := CorpSlug(name)
	if slug == "" || GalnetPathTaken(slug) {
		return CorpProfileURL(corpID), nil
	}
	all, err := ListCorpNames(ctx, q)
	if err != nil {
		return "", err
	}
	key := strings.ToLower(slug)
	for _, c := range all {
		if c.ID != corpID && CorpSlugKey(c.Name) == key {
			return CorpProfileURL(corpID), nil
		}
	}
	return GalnetHost + "/" + slug, nil
}

// StampCorpPageURL points a just-created corporation's url at its Galnet page.
func StampCorpPageURL(ctx context.Context, db *sql.DB, corpID int64) error {
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT corporationName FROM corporation WHERE corporationID = ?`, corpID).Scan(&name); err != nil {
		return fmt.Errorf("StampCorpPageURL read: %w", err)
	}
	u, err := corpGalnetURL(ctx, db, corpID, name)
	if err != nil || u == "" {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE corporation SET url = ? WHERE corporationID = ?`, u, corpID); err != nil {
		return fmt.Errorf("StampCorpPageURL: %w", err)
	}
	return nil
}

// isStampedCorpURL reports whether u is an address this server handed the corporation
// rather than one its directors typed: the old ESP admin page, the ID form, or the slug
// of name.
func isStampedCorpURL(u string, corpID int64, name string) bool {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if m := espCorpPageRE.FindStringSubmatch(u); m != nil {
		return m[1] == strconv.FormatInt(corpID, 10)
	}
	if u == CorpProfileURL(corpID) {
		return true
	}
	rest, ok := strings.CutPrefix(u, GalnetHost+"/")
	return ok && rest != "" && strings.EqualFold(rest, CorpSlug(name))
}

// espCorpPageRE matches the old url written by StampCorpPageURL, the corporation's ESP
// admin page, which the in-game browser cannot draw.
var espCorpPageRE = regexp.MustCompile(`^https?://[^/]+/corporation/(\d+)$`)

// MigrateCorpPageURLs moves every corporation still carrying the old ESP admin address
// onto its Galnet one. Idempotent: a moved row no longer matches.
func MigrateCorpPageURLs(ctx context.Context, db *sql.DB) (int, error) {
	if GalnetPathTaken == nil {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT corporationID, corporationName, url FROM corporation WHERE url LIKE '%/corporation/%'`)
	if err != nil {
		return 0, fmt.Errorf("MigrateCorpPageURLs: %w", err)
	}
	type row struct {
		id        int64
		name, url string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.url); err != nil {
			rows.Close()
			return 0, fmt.Errorf("MigrateCorpPageURLs scan: %w", err)
		}
		if m := espCorpPageRE.FindStringSubmatch(strings.TrimRight(strings.TrimSpace(r.url), "/")); m != nil &&
			m[1] == strconv.FormatInt(r.id, 10) {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("MigrateCorpPageURLs rows: %w", err)
	}
	n := 0
	for _, r := range todo {
		u, err := corpGalnetURL(ctx, db, r.id, r.name)
		if err != nil {
			return n, err
		}
		// Guarded on the old value: a director who saved a url meanwhile wins.
		res, err := db.ExecContext(ctx,
			`UPDATE corporation SET url = ? WHERE corporationID = ? AND url = ?`, u, r.id, r.url)
		if err != nil {
			return n, fmt.Errorf("MigrateCorpPageURLs update %d: %w", r.id, err)
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, nil
}

// GoMigrateCorpPageURLs runs MigrateCorpPageURLs off the caller's goroutine.
func GoMigrateCorpPageURLs(db *sql.DB) {
	go func() {
		defer safego.Recover("GoMigrateCorpPageURLs")
		n, err := MigrateCorpPageURLs(context.Background(), db)
		if err != nil {
			log.Printf("corp urls: %v", err)
		} else if n > 0 {
			log.Printf("corp urls: %d corporation(s) moved onto their Galnet address", n)
		}
	}()
}
