package main

import (
	"database/sql"

	"gurimoon/esp"
	"gurimoon/tick"
)

// moduleBootHooks is appended to from init() by files in package main, core features and overlay
// modules alike. runModuleBootHooks calls each once at boot, after the tick manager and session
// registry exist. A hook must not stop the boot: log and return.
var moduleBootHooks []func(sqldb *sql.DB, sessReg *esp.SessionRegistry, tickMgr *tick.Manager)

func runModuleBootHooks(sqldb *sql.DB, sessReg *esp.SessionRegistry, tickMgr *tick.Manager) {
	for _, start := range moduleBootHooks {
		start(sqldb, sessReg, tickMgr)
	}
}
