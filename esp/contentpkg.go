package esp

// Admin → Content Packages: export dungeons, mission templates, spawners and the custom NPCs
// they use as one .gmpkg file, and install, upgrade or uninstall such files. The format and the
// install rules are in docs/content-packages.md; the work is done by package contentpkg.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gurimoon/contentpkg"
	evedb "gurimoon/db"
)

// pendingImport is an uploaded package waiting for the admin to confirm the install.
type pendingImport struct {
	data []byte
	at   time.Time
}

const pendingImportTTL = 30 * time.Minute

var (
	pendingMu      sync.Mutex
	pendingImports = map[string]pendingImport{}
)

// storePending keeps an uploaded package for the confirm step and returns its token.
func storePending(data []byte) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	pendingMu.Lock()
	defer pendingMu.Unlock()
	now := time.Now()
	for k, p := range pendingImports {
		if now.Sub(p.at) > pendingImportTTL {
			delete(pendingImports, k)
		}
	}
	// At most a few uploads wait at once; the oldest goes first.
	for len(pendingImports) >= 4 {
		oldest := ""
		for k, p := range pendingImports {
			if oldest == "" || p.at.Before(pendingImports[oldest].at) {
				oldest = k
			}
		}
		delete(pendingImports, oldest)
	}
	pendingImports[token] = pendingImport{data: data, at: now}
	return token
}

// takePending returns an uploaded package by token. remove drops it from the store.
func takePending(token string, remove bool) ([]byte, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pendingImports[token]
	if !ok || time.Since(p.at) > pendingImportTTL {
		delete(pendingImports, token)
		return nil, false
	}
	if remove {
		delete(pendingImports, token)
	}
	return p.data, true
}

// contentTypesChanged drops every type cache after a package wrote or deleted custom types.
func contentTypesChanged(ids []int64) {
	if len(ids) == 0 {
		return
	}
	evedb.InvalidateTypeStatics(ids...)
	evedb.InvalidateNPCSecurityBonus(ids...)
	evedb.InvalidateTypeInfo()
	invalidateTypeCache()
	for _, id := range ids {
		npcTypeChanged(id)
	}
}

// contentExportForm is the export form as submitted, re-rendered after a preview.
type contentExportForm struct {
	Name, Version, Author, Description, Readme, UUID string
	Dungeons, Missions, Spawners                     map[int64]bool
}

func parseIDs(vals []string) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, v := range vals {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// contentListView is one picker column of the export form.
type contentListView struct {
	Title string
	Field string // form field name of the checkboxes
	Items []contentpkg.CatalogItem
	Sel   map[int64]bool
}

// contentReportRow is one table line of an install report.
type contentReportRow struct {
	Table                     string
	Created, Updated, Removed int
}

func reportRows(rep *contentpkg.Report) []contentReportRow {
	tables := map[string]bool{}
	for _, m := range []map[string]int{rep.Created, rep.Updated, rep.Removed} {
		for t, n := range m {
			if n != 0 {
				tables[t] = true
			}
		}
	}
	order := []string{"invTypes", "npc_loot_entries", "dunDungeons", "dunRooms", "dunObjects",
		"dunObjectLoot", "dunTriggers", "missionTemplates", "dunSpawners"}
	var out []contentReportRow
	for _, t := range order {
		if tables[t] {
			out = append(out, contentReportRow{t, rep.Created[t], rep.Updated[t], rep.Removed[t]})
			delete(tables, t)
		}
	}
	var rest []string
	for t := range tables {
		rest = append(rest, t)
	}
	sort.Strings(rest)
	for _, t := range rest {
		out = append(out, contentReportRow{t, rep.Created[t], rep.Updated[t], rep.Removed[t]})
	}
	return out
}

func (s *Server) handleContentPackages(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	data := map[string]any{"Page": "content-packages"}
	form := contentExportForm{Version: "1.0"}
	q := r.URL.Query()
	form.Dungeons = idSet(parseIDs(q["dungeon"]))
	form.Missions = idSet(parseIDs(q["mission"]))
	form.Spawners = idSet(parseIDs(q["spawner"]))
	tab := "export"
	if len(form.Dungeons)+len(form.Missions)+len(form.Spawners) == 0 {
		tab = q.Get("tab")
	}
	fail := func(err error) { data["Result"], data["ResultErr"] = "Error: "+err.Error(), true }

	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, contentpkg.MaxPackageBytes+1<<20)
		var err error
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			err = r.ParseMultipartForm(8 << 20)
		} else {
			err = r.ParseForm()
		}
		if err != nil {
			fail(fmt.Errorf("reading the form: %w", err))
		} else {
			switch r.FormValue("action") {
			case "export":
				tab = "export"
				form = contentExportForm{
					Name: r.FormValue("name"), Version: r.FormValue("version"), Author: r.FormValue("author"),
					Description: r.FormValue("description"), Readme: r.FormValue("readme"), UUID: r.FormValue("uuid"),
				}
				sel := contentpkg.Selection{
					Dungeons: parseIDs(r.Form["dungeon"]),
					Missions: parseIDs(r.Form["mission"]),
					Spawners: parseIDs(r.Form["spawner"]),
				}
				form.Dungeons, form.Missions, form.Spawners = idSet(sel.Dungeons), idSet(sel.Missions), idSet(sel.Spawners)
				opt := contentpkg.ExportOptions{
					UUID: form.UUID, Name: form.Name, Version: form.Version, Author: form.Author,
					Description: form.Description, Readme: form.Readme,
				}
				pkg, warnings, err := contentpkg.Export(ctx, s.db, sel, opt)
				if err != nil {
					fail(err)
					break
				}
				if r.FormValue("mode") == "download" {
					var buf bytes.Buffer
					if err := contentpkg.Write(&buf, pkg); err != nil {
						fail(err)
						break
					}
					name := contentpkg.Slug(pkg.Manifest.Name) + "-" + contentpkg.Slug(pkg.Manifest.Version) + contentpkg.FileExt
					w.Header().Set("Content-Type", "application/zip")
					w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
					w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
					_, _ = w.Write(buf.Bytes())
					return
				}
				data["ExportPreview"] = pkg
				data["ExportWarnings"] = warnings
				if form.UUID == "" {
					if owner := contentpkg.PackageOf(ctx, s.db, sel); owner != "" {
						data["ExportOwner"] = owner
					}
				}

			case "preview":
				tab = "import"
				file, hdr, err := r.FormFile("package")
				if err != nil {
					fail(errors.New("choose a " + contentpkg.FileExt + " file"))
					break
				}
				raw, err := io.ReadAll(io.LimitReader(file, contentpkg.MaxPackageBytes+1))
				file.Close()
				if err != nil {
					fail(err)
					break
				}
				pkg, err := contentpkg.Read(raw)
				if err != nil {
					fail(fmt.Errorf("%s: %w", hdr.Filename, err))
					break
				}
				disabled := r.FormValue("disabled") == "1"
				rep, err := contentpkg.Import(ctx, s.db, pkg, contentpkg.ImportOptions{DryRun: true, Disabled: disabled})
				if err != nil {
					fail(fmt.Errorf("%s cannot be installed: %w", hdr.Filename, err))
					break
				}
				data["ImportToken"] = storePending(raw)
				data["ImportFile"] = hdr.Filename
				data["ImportPkg"] = pkg
				data["ImportReport"] = rep
				data["ImportRows"] = reportRows(rep)
				data["ImportDisabled"] = disabled
				data["ImportReadme"] = pkg.Readme

			case "install":
				tab = "installed"
				raw, ok := takePending(r.FormValue("token"), false)
				if !ok {
					fail(errors.New("the upload expired; choose the file again"))
					break
				}
				pkg, err := contentpkg.Read(raw)
				if err != nil {
					fail(err)
					break
				}
				rep, err := contentpkg.Import(ctx, s.db, pkg, contentpkg.ImportOptions{Disabled: r.FormValue("disabled") == "1"})
				if err != nil {
					fail(err)
					break
				}
				takePending(r.FormValue("token"), true)
				contentTypesChanged(rep.TypeIDs)
				verb := "Installed"
				if rep.Upgrade {
					verb = "Upgraded"
				}
				data["Result"] = fmt.Sprintf("%s %s %s: %d row(s) added, %d replaced, %d removed.", verb, rep.Name,
					rep.Version, contentpkg.Total(rep.Created), contentpkg.Total(rep.Updated), contentpkg.Total(rep.Removed))
				data["ResultReport"] = rep

			case "uninstall":
				tab = "installed"
				rep, err := contentpkg.Uninstall(ctx, s.db, r.FormValue("uuid"))
				if err != nil {
					fail(err)
					break
				}
				contentTypesChanged(rep.TypeIDs)
				data["Result"] = fmt.Sprintf("Uninstalled %s %s: %d row(s) removed.", rep.Name, rep.Version,
					contentpkg.Total(rep.Removed))
				data["ResultReport"] = rep

			default:
				fail(errors.New("unknown action"))
			}
		}
	}

	catalog, err := contentpkg.LoadCatalog(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "content-packages"})
		return
	}
	installed, err := contentpkg.List(ctx, s.db)
	if err != nil {
		fail(err)
	}
	if tab == "" {
		tab = "export"
	}
	data["Lists"] = []contentListView{
		{"Dungeons", "dungeon", catalog.Dungeons, form.Dungeons},
		{"Mission templates", "mission", catalog.Missions, form.Missions},
		{"Spawners", "spawner", catalog.Spawners, form.Spawners},
	}
	data["Installed"] = installed
	data["Form"] = form
	data["Tab"] = tab
	data["MaxMB"] = contentpkg.MaxPackageBytes >> 20
	s.render(w, "content_packages.html", data)
}
