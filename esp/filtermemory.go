package esp

// Remembered list filters. A browse page whose filters are GET parameters keeps the last set in a
// cookie scoped to that page, so following the sidebar link back to the bare URL shows the list as
// it was left. The cookie holds a display preference only; no database row changes on the GET.

import (
	"net/http"
	"net/url"
)

// filterMemoryMaxAge is how long a remembered filter set lives, in seconds (one year).
const filterMemoryMaxAge = 365 * 24 * 3600

// rememberListFilter returns the filters a list page should use. When the request carries any of
// keys, those values are saved to the cookie and the query is returned unchanged. When it carries
// none of them, the saved values are merged in over the query (other parameters such as page are
// kept). To remember a new filter, add its parameter name to the page's keys.
func rememberListFilter(w http.ResponseWriter, r *http.Request, cookieName, path string, keys []string) url.Values {
	query := r.URL.Query()

	given := false
	for _, k := range keys {
		if _, ok := query[k]; ok {
			given = true
			break
		}
	}

	if given {
		saved := url.Values{}
		for _, k := range keys {
			if v := query.Get(k); v != "" {
				saved.Set(k, v)
			}
		}
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: saved.Encode(), Path: path,
			MaxAge: filterMemoryMaxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		return query
	}

	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return query
	}
	saved, err := url.ParseQuery(c.Value)
	if err != nil {
		return query
	}
	for _, k := range keys {
		if v := saved.Get(k); v != "" {
			query.Set(k, v)
		}
	}
	return query
}
