// Command clientpatch installs optional client script patches into an RMR client.
//
//	go run ./cmd/clientpatch [-apply] [-uninstall] [-client dir]   (no -apply is a dry run)
//
// The server works without them. Patches: bountyportraits (a docked client renders Most
// Wanted faces, server/charmgr/portraitjobs.go), researchfacility (lobby button,
// cmd/gurimoon/researchslots.go), asteroidlock (keeps svc.bracket's asteroid bracket
// during a lock), jukebox (adds tracks inflight.m3u omits). Each file is
// script/gurimoon_<name>.py, run from a marker block in my_autoexec.py (backed up once
// to .pre-gurimoon); -uninstall removes both, and any pre-rename eveclassic leftovers (legacy*).
package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gurimoon/config"
)

//go:embed patches/*.py
var patchFS embed.FS

const (
	blockBegin = "# >>> gurimoon client patches (managed by cmd/clientpatch -- do not edit) >>>"
	blockEnd   = "# <<< gurimoon client patches <<<"
	autoexec   = "my_autoexec.py"
	backupName = "my_autoexec.py.pre-gurimoon"
	filePrefix = "gurimoon_"

	// Names written by clients patched before the rename. Never written again, only
	// recognised so a reinstall or -uninstall leaves no orphan block or script behind.
	legacyBlockBegin = "# >>> eveclassic client patches (managed by cmd/clientpatch -- do not edit) >>>"
	legacyBlockEnd   = "# <<< eveclassic client patches <<<"
	legacyBackupName = "my_autoexec.py.pre-eveclassic"
	legacyFilePrefix = "eveclassic_"
)

type patch struct {
	name string // e.g. "researchfacility"
	data []byte // CRLF, like the client's own scripts
}

func loadPatches() ([]patch, error) {
	entries, err := patchFS.ReadDir("patches")
	if err != nil {
		return nil, err
	}
	var out []patch
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		b, err := patchFS.ReadFile("patches/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, patch{name: strings.TrimSuffix(e.Name(), ".py"), data: toCRLF(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func toCRLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

// hookBlock is the Python 2.3 run in my_autoexec.py's globals, the autoexec module's,
// where ExecFile and Import live.
func hookBlock(patches []patch, eol string) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + eol) }
	w(blockBegin)
	w("# Optional GuriMoon patches; remove with: go run ./cmd/clientpatch -uninstall -apply")
	for _, p := range patches {
		w("try:")
		w("    ExecFile('script:/" + filePrefix + p.name + ".py')")
		w("except:")
		w("    import traceback")
		w("    traceback.print_exc()")
		w("    print '>>> ERROR: GuriMoon patch " + p.name + " could not be run'")
	}
	w(blockEnd)
	return b.String()
}

// stripBlock removes the marker block, current or legacy, and the blank line before it,
// and reports whether there was one.
func stripBlock(s string) (string, bool) {
	s, cur := stripMarked(s, blockBegin, blockEnd)
	s, old := stripMarked(s, legacyBlockBegin, legacyBlockEnd)
	return s, cur || old
}

// stripMarked removes one begin..end marker block and the blank line before it.
func stripMarked(s, begin, end string) (string, bool) {
	i := strings.Index(s, begin)
	if i < 0 {
		return s, false
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return s, false // half a block: leave it for a human
	}
	return cutBlock(s, i, i+j+len(end)), true
}

// cutBlock drops s[i:stop] plus the line breaks after it, keeping the text before it
// separated from what follows by one line break.
func cutBlock(s string, i, stop int) string {
	end := stop
	for end < len(s) && (s[end] == '\r' || s[end] == '\n') {
		end++
	}
	head := strings.TrimRight(s[:i], "\r\n")
	if head != "" {
		head += detectEOL(s)
	}
	return head + s[end:]
}

func detectEOL(s string) string {
	if strings.Contains(s, "\r\n") || s == "" {
		return "\r\n"
	}
	return "\n"
}

func main() {
	apply := flag.Bool("apply", false, "write changes (default: dry-run report)")
	uninstall := flag.Bool("uninstall", false, "remove the patches and the my_autoexec.py block")
	cfgPath := flag.String("config", "config.json", "config.json path")
	clientFlag := flag.String("client", "", "client folder (default: esp.clientPath from config)")
	flag.Parse()

	client := *clientFlag
	if client == "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("need -client, or a readable config: %v", err)
		}
		client = cfg.ESP.ClientPath
	}
	if client == "" {
		log.Fatal("no client path: set esp.clientPath in config.json or pass -client")
	}
	scriptDir := filepath.Join(client, "script")

	// The client's autoexec must run my_autoexec.py.
	ax, err := os.ReadFile(filepath.Join(scriptDir, "sys", "autoexec.py"))
	if err != nil {
		log.Fatalf("%s does not look like an RMR client folder (no script/sys/autoexec.py): %v", client, err)
	}
	if !bytes.Contains(ax, []byte("my_autoexec.py")) {
		log.Fatalf("script/sys/autoexec.py in %s does not run my_autoexec.py; refusing to guess a hook", client)
	}

	patches, err := loadPatches()
	if err != nil {
		log.Fatalf("embedded patches: %v", err)
	}

	axPath := filepath.Join(scriptDir, autoexec)
	orig, err := os.ReadFile(axPath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("read %s: %v", axPath, err)
	}
	cur := string(orig)
	eol := detectEOL(cur)
	stripped, hadBlock := stripBlock(cur)

	var actions []string
	var writes []func() error

	if *uninstall {
		if hadBlock {
			actions = append(actions, "remove the patch block from "+autoexec)
			writes = append(writes, func() error { return os.WriteFile(axPath, []byte(stripped), 0o644) })
		}
		for _, p := range patches {
			for _, prefix := range []string{filePrefix, legacyFilePrefix} {
				path := filepath.Join(scriptDir, prefix+p.name+".py")
				if _, err := os.Stat(path); err == nil {
					actions = append(actions, "delete "+filepath.Base(path))
					writes = append(writes, func() error { return os.Remove(path) })
				}
			}
		}
	} else {
		for _, p := range patches {
			path := filepath.Join(scriptDir, filePrefix+p.name+".py")
			have, err := os.ReadFile(path)
			switch {
			case err == nil && bytes.Equal(have, p.data):
				fmt.Printf("  %s is current\n", filepath.Base(path))
			default:
				verb := "write"
				if err == nil {
					verb = "update"
				}
				actions = append(actions, fmt.Sprintf("%s %s (%d bytes)", verb, filepath.Base(path), len(p.data)))
				data := p.data
				writes = append(writes, func() error { return os.WriteFile(path, data, 0o644) })
			}
		}

		// Scripts from a pre-rename install: the new hook block no longer runs them.
		for _, p := range patches {
			path := filepath.Join(scriptDir, legacyFilePrefix+p.name+".py")
			if _, err := os.Stat(path); err == nil {
				actions = append(actions, "delete legacy "+filepath.Base(path))
				writes = append(writes, func() error { return os.Remove(path) })
			}
		}

		var want string
		if strings.TrimSpace(stripped) == "" {
			want = hookBlock(patches, eol)
		} else {
			want = strings.TrimRight(stripped, "\r\n") + eol + eol + hookBlock(patches, eol)
		}
		if want != cur {
			switch {
			case !exists:
				actions = append(actions, "create "+autoexec+" with the patch block")
			case hadBlock:
				actions = append(actions, "update the patch block in "+autoexec)
			default:
				actions = append(actions, "append the patch block to "+autoexec+" (existing content kept)")
			}
			bak := filepath.Join(scriptDir, backupName)
			legacyBak := filepath.Join(scriptDir, legacyBackupName)
			if _, err := os.Stat(legacyBak); err == nil {
				if _, err := os.Stat(bak); os.IsNotExist(err) {
					actions = append(actions, "rename "+legacyBackupName+" to "+backupName)
					writes = append(writes, func() error { return os.Rename(legacyBak, bak) })
				}
			}
			if exists && !hadBlock {
				if _, err := os.Stat(bak); os.IsNotExist(err) {
					actions = append(actions, "back up the original "+autoexec+" to "+backupName)
					writes = append(writes, func() error { return os.WriteFile(bak, orig, 0o644) })
				}
			}
			writes = append(writes, func() error { return os.WriteFile(axPath, []byte(want), 0o644) })
		} else {
			fmt.Printf("  %s hook is current\n", autoexec)
		}
	}

	fmt.Printf("client: %s\n", client)
	if len(actions) == 0 {
		fmt.Println("nothing to do")
		return
	}
	for _, a := range actions {
		fmt.Println("  " + a)
	}
	if !*apply {
		fmt.Println("DRY RUN — rerun with -apply")
		return
	}
	for _, w := range writes {
		if err := w(); err != nil {
			log.Fatalf("write: %v", err)
		}
	}
	if *uninstall {
		fmt.Println("removed. Restart the client.")
	} else {
		fmt.Println("installed. Restart the client for the patches to take effect.")
	}
}
