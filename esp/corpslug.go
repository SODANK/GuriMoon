package esp

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// A corporation's Galnet address is its name: http://myeve.eve-online.com/Saang-Foundation
// (evedb.CorpSlug), which the corp info window shows as its website (db/corppageurl.go). The page
// is served in place, as the /ig/ aliases are, so the address bar keeps the clean address (the
// client builds it from the URL it requested).
//
// Only a path no site has registered reaches here (redispatch is the mux's catch-all), so /galnet,
// /scope, /corp and the rest win; the stamp refuses a slug one of them holds (GalnetPathTaken).

// corpSlugTable is slug key → corporation IDs, rebuilt from the corporation
// table. A few hundred rows, so a full reload is cheaper than keeping it in
// step with every rename and founding.
type corpSlugTable struct {
	mu     sync.Mutex
	byKey  map[string][]int64
	loaded time.Time
}

const (
	corpSlugTTL       = 5 * time.Minute
	corpSlugMissFloor = 5 * time.Second // a miss reloads at most this often
)

var corpSlugs corpSlugTable

// lookup answers the corporation a slug addresses. A slug two corporations share ("Foo Bar" /
// "Foo-Bar") goes to the lowest ID; the other has the ID address. A miss on a table older than
// the floor reloads once, so a new corporation resolves on its first visit.
func (t *corpSlugTable) lookup(ctx context.Context, s *NewsServer, key string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	age := time.Since(t.loaded)
	if t.byKey == nil || age > corpSlugTTL {
		t.reload(ctx, s)
	} else if _, ok := t.byKey[key]; !ok && age > corpSlugMissFloor {
		t.reload(ctx, s)
	}
	ids := t.byKey[key]
	if len(ids) == 0 {
		return 0
	}
	best := ids[0]
	for _, id := range ids[1:] {
		if id < best {
			best = id
		}
	}
	return best
}

func (t *corpSlugTable) reload(ctx context.Context, s *NewsServer) {
	t.loaded = time.Now() // a failed load waits out the floor too
	rows, err := evedb.ListCorpNames(ctx, s.db)
	if err != nil {
		log.Printf("galnet: corp slugs: %v", err)
		return
	}
	m := make(map[string][]int64, len(rows))
	for _, r := range rows {
		if k := evedb.CorpSlugKey(r.Name); k != "" {
			m[k] = append(m[k], r.ID)
		}
	}
	t.byKey = m
}

// serveCorpSlug answers /<Corp-Name> with that corporation's Galnet page, and
// reports false for anything that is not one.
func (s *NewsServer) serveCorpSlug(w http.ResponseWriter, r *http.Request) bool {
	seg := strings.Trim(r.URL.Path, "/")
	if seg == "" || strings.Contains(seg, "/") || strings.Contains(seg, ".") {
		return false // a file (favicon.ico, robots.txt) or a deeper path
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	corpID := corpSlugs.lookup(ctx, s, evedb.CorpSlugKey(seg))
	if corpID == 0 {
		return false
	}
	r2 := r.Clone(r.Context())
	q := r2.URL.Query()
	q.Set("id", strconv.FormatInt(corpID, 10))
	r2.URL.RawQuery = q.Encode()
	if corpID >= 1000000 && corpID < 2000000 {
		s.handleConcordCorporation(w, r2) // the portal renders player corps only
	} else {
		s.handleCorpProfile(w, r2)
	}
	return true
}

// galnetPathTaken reports whether a top-level segment is a registered path on
// this server, with or without the trailing slash a site root carries.
func (s *NewsServer) galnetPathTaken(seg string) bool {
	for _, p := range []string{"/" + seg, "/" + seg + "/"} {
		for _, v := range []string{p, strings.ToLower(p)} {
			r, err := http.NewRequest(http.MethodGet, v, nil)
			if err != nil {
				return true
			}
			if _, pattern := s.mux.Handler(r); pattern != "/" && pattern != "" {
				return true
			}
		}
	}
	return false
}
