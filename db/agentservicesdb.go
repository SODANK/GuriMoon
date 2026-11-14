package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ── Agent services ───────────────────────────────────────────────────────────
//
// Non-mission agent services: locating pilots, repairing a negative security status, and stub
// gifts and rumours. Which agent offers what is server-authored: agtServiceDivisions grants a
// service to a division from minLevel, agtServiceOverrides forces one agent on or off.
// See docs/agent-services.md.

// Service keys. They are stored in agtServiceDivisions/agtServiceOverrides rows and used as
// text-key prefixes: migrate both tables before renaming one.
const (
	ServiceLocator   = "locator"
	ServiceSecStatus = "secstatus"
	ServiceGifts     = "gifts"  // stub: listed in the info window, no economy yet
	ServiceRumors    = "rumors" // stub: listed in the info window, no intel yet
)

// ServiceKeys is the display/iteration order everywhere (ESP grid, info
// window sections).
var ServiceKeys = []string{ServiceLocator, ServiceSecStatus, ServiceGifts, ServiceRumors}

// ServiceNames maps a service key to its player-facing header.
var ServiceNames = map[string]string{
	ServiceLocator:   "Locator Services",
	ServiceSecStatus: "Security Status Services",
	ServiceGifts:     "Donations/Gifts/Bribes",
	ServiceRumors:    "Rumour Services",
}

// Settings (server_settings; edited on the ESP Agents → Services page).
const (
	settingLocatorFeeBase = "agents.locatorFeeBase"
	settingSecRepairPer01 = "agents.secRepairPer01"
	settingServicesSeeded = "agents.servicesSeeded"
)

// Fee defaults. The locator fee is base x level^2; the security repair price is per 0.1 of
// negative status.
const (
	DefaultLocatorFeeBase = 15000
	DefaultSecRepairPer01 = 1500000
)

// GetLocatorFeeBase returns the level-1 locator fee in ISK.
func GetLocatorFeeBase(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingLocatorFeeBase, DefaultLocatorFeeBase, 0, 100000000)
}

// SetLocatorFeeBase stores the level-1 locator fee.
func SetLocatorFeeBase(ctx context.Context, db *sql.DB, v int) error {
	return SetServerSetting(ctx, db, settingLocatorFeeBase, fmt.Sprintf("%d", v))
}

// GetSecRepairPer01 returns the ISK price per 0.1 of negative security status.
func GetSecRepairPer01(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingSecRepairPer01, DefaultSecRepairPer01, 0, 1000000000)
}

// SetSecRepairPer01 stores the per-0.1 security repair price.
func SetSecRepairPer01(ctx context.Context, db *sql.DB, v int) error {
	return SetServerSetting(ctx, db, settingSecRepairPer01, fmt.Sprintf("%d", v))
}

// LocatorFee is the quote for one search from an agent of the given level.
func LocatorFee(level, feeBase int) int64 {
	if level < 1 {
		level = 1
	}
	if level > 4 {
		level = 4
	}
	return int64(feeBase) * int64(level) * int64(level)
}

// LocatorRangeName is the reach a locator agent's level buys, in the words
// the conversation and the info window use.
func LocatorRangeName(level int) string {
	switch {
	case level <= 1:
		return "this solar system"
	case level == 2:
		return "this constellation"
	case level == 3:
		return "this region"
	default:
		return "anywhere in the galaxy"
	}
}

// EnsureAgentServiceTables creates the service tables and seeds the default division
// distribution once, guarded by a settings marker so an emptied grid stays empty.
func EnsureAgentServiceTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agtServiceDivisions (
			serviceKey VARCHAR(24) NOT NULL,
			divisionID INT NOT NULL,
			minLevel   INT NOT NULL DEFAULT 1,
			PRIMARY KEY (serviceKey, divisionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS agtServiceOverrides (
			agentID    INT NOT NULL,
			serviceKey VARCHAR(24) NOT NULL,
			enabled    TINYINT(1) NOT NULL DEFAULT 1,
			PRIMARY KEY (agentID, serviceKey)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS agtLocatorJobs (
			jobID       BIGINT AUTO_INCREMENT PRIMARY KEY,
			charID      INT NOT NULL,
			agentID     INT NOT NULL,
			targetID    INT NOT NULL,
			fee         BIGINT NOT NULL DEFAULT 0,
			requestedAt DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			dueAt       DATETIME NOT NULL,
			delivered   TINYINT(1) NOT NULL DEFAULT 0,
			INDEX idx_due (delivered, dueAt),
			INDEX idx_char_agent (charID, agentID, delivered)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`CREATE TABLE IF NOT EXISTS agtServiceTexts (
			divisionID INT NOT NULL DEFAULT 0,
			textKey    VARCHAR(48) NOT NULL,
			body       TEXT NOT NULL,
			PRIMARY KEY (divisionID, textKey)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("EnsureAgentServiceTables: %w", err)
		}
	}

	// Migrate agtServiceTexts to the division-scoped key (0 = every agent). Errors mean it already ran.
	db.ExecContext(ctx, `ALTER TABLE agtServiceTexts ADD COLUMN divisionID INT NOT NULL DEFAULT 0 FIRST`)
	db.ExecContext(ctx, `ALTER TABLE agtServiceTexts DROP PRIMARY KEY, ADD PRIMARY KEY (divisionID, textKey)`)

	if _, ok, _ := GetServerSetting(ctx, db, settingServicesSeeded); !ok {
		for _, row := range defaultServiceDivisions {
			if _, err := db.ExecContext(ctx, `
				INSERT IGNORE INTO agtServiceDivisions (serviceKey, divisionID, minLevel)
				VALUES (?, ?, ?)`, row.ServiceKey, row.DivisionID, row.MinLevel); err != nil {
				return fmt.Errorf("EnsureAgentServiceTables seed: %w", err)
			}
		}
		if err := SetServerSetting(ctx, db, settingServicesSeeded, "1"); err != nil {
			return err
		}
	}
	return nil
}

// ServiceDivisionRow is one grant in the distribution grid.
type ServiceDivisionRow struct {
	ServiceKey string
	DivisionID int64
	MinLevel   int
}

// defaultServiceDivisions is the shipped distribution (crpNPCDivisions IDs). Divisions not
// listed, such as Storage and Manufacturing, get nothing.
var defaultServiceDivisions = []ServiceDivisionRow{
	// Finding people is spy work: Intelligence and Surveillance from level 1,
	// the police-adjacent divisions once they are senior enough, Command last.
	{ServiceLocator, 9, 1},  // Intelligence
	{ServiceLocator, 21, 1}, // Surveillance
	{ServiceLocator, 10, 2}, // Internal Security
	{ServiceLocator, 19, 2}, // Security
	{ServiceLocator, 6, 3},  // Command

	// Cleaning a criminal record is paperwork: Legal from level 1, Internal
	// Security only at the top (they wrote the record).
	{ServiceSecStatus, 11, 1}, // Legal
	{ServiceSecStatus, 10, 3}, // Internal Security

	// Gifts/bribes (stub): the schmoozing divisions.
	{ServiceGifts, 17, 1}, // Public Relations
	{ServiceGifts, 15, 1}, // Personnel
	{ServiceGifts, 3, 1},  // Advisory

	// Rumours / site information (stub): the people who chart space or hear
	// things.
	{ServiceRumors, 5, 1},  // Astrosurveying
	{ServiceRumors, 3, 1},  // Advisory
	{ServiceRumors, 17, 2}, // Public Relations
	{ServiceRumors, 9, 3},  // Intelligence
}

// AgentServiceSet resolves the effective services for one agent: a per-agent
// override wins outright; otherwise the agent's division grants the service
// when the agent's level clears the row's minLevel.
func AgentServiceSet(ctx context.Context, db *sql.DB, agent *AgentInfo) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := db.QueryContext(ctx,
		`SELECT serviceKey, minLevel FROM agtServiceDivisions WHERE divisionID = ?`,
		agent.DivisionID)
	if err != nil {
		return out, fmt.Errorf("AgentServiceSet divisions: %w", err)
	}
	for rows.Next() {
		var key string
		var minLevel int
		if err := rows.Scan(&key, &minLevel); err != nil {
			rows.Close()
			return out, fmt.Errorf("AgentServiceSet scan: %w", err)
		}
		if agent.Level >= minLevel {
			out[key] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	orows, err := db.QueryContext(ctx,
		`SELECT serviceKey, enabled FROM agtServiceOverrides WHERE agentID = ?`,
		agent.AgentID)
	if err != nil {
		return out, fmt.Errorf("AgentServiceSet overrides: %w", err)
	}
	defer orows.Close()
	for orows.Next() {
		var key string
		var enabled bool
		if err := orows.Scan(&key, &enabled); err != nil {
			return out, fmt.Errorf("AgentServiceSet override scan: %w", err)
		}
		if enabled {
			out[key] = true
		} else {
			delete(out, key)
		}
	}
	return out, orows.Err()
}

// ── Locator jobs ─────────────────────────────────────────────────────────────

// LocatorJob is one paid search, delivered by the locator sweep as an
// EVE-mail from the agent once dueAt passes.
type LocatorJob struct {
	JobID       int64
	CharID      int64
	AgentID     int64
	TargetID    int64
	Fee         int64
	RequestedAt time.Time
	DueAt       time.Time
	Delivered   bool
}

// CreateLocatorJob records a paid search due after delay.
func CreateLocatorJob(ctx context.Context, db *sql.DB, charID, agentID, targetID, fee int64, delay time.Duration) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO agtLocatorJobs (charID, agentID, targetID, fee, dueAt)
		VALUES (?, ?, ?, ?, ?)`,
		charID, agentID, targetID, fee, time.Now().Add(delay))
	if err != nil {
		return fmt.Errorf("CreateLocatorJob: %w", err)
	}
	return nil
}

// HasPendingLocatorJob reports whether the character has an undelivered search with this agent;
// only one runs at a time.
func HasPendingLocatorJob(ctx context.Context, db *sql.DB, charID, agentID int64) bool {
	var n int
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agtLocatorJobs
		 WHERE charID = ? AND agentID = ? AND delivered = 0`,
		charID, agentID).Scan(&n)
	return n > 0
}

// DueLocatorJobs returns undelivered jobs whose dueAt has passed.
func DueLocatorJobs(ctx context.Context, db *sql.DB) ([]LocatorJob, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT jobID, charID, agentID, targetID, fee, requestedAt, dueAt, delivered
		  FROM agtLocatorJobs
		 WHERE delivered = 0 AND dueAt <= NOW()
		 ORDER BY dueAt LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("DueLocatorJobs: %w", err)
	}
	defer rows.Close()
	var out []LocatorJob
	for rows.Next() {
		var j LocatorJob
		if err := rows.Scan(&j.JobID, &j.CharID, &j.AgentID, &j.TargetID, &j.Fee,
			&j.RequestedAt, &j.DueAt, &j.Delivered); err != nil {
			return nil, fmt.Errorf("DueLocatorJobs scan: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkLocatorJobDelivered stamps a job done.
func MarkLocatorJobDelivered(ctx context.Context, db *sql.DB, jobID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE agtLocatorJobs SET delivered = 1 WHERE jobID = ?`, jobID)
	if err != nil {
		return fmt.Errorf("MarkLocatorJobDelivered: %w", err)
	}
	return nil
}

// ── Locator target resolution ────────────────────────────────────────────────
//
// Target lookup reuses FindPlayerCharByName (recustomdb.go): exact player name with accountID > 0,
// so an NPC agent cannot be located and the roster does not leak.

// CharLastKnownLocation is the offline half of the locate: where the character record says they
// are. The caller answers an online target from the session registry first.
func CharLastKnownLocation(ctx context.Context, db *sql.DB, charID int64) (stationID, solarSystemID int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(stationID, 0), COALESCE(solarSystemID, 0)
		  FROM character_ WHERE characterID = ?`, charID).Scan(&stationID, &solarSystemID)
	if err != nil {
		err = fmt.Errorf("CharLastKnownLocation charID=%d: %w", charID, err)
	}
	return
}

// SystemConstellationRegion resolves a solar system's parents for the locator
// range check.
func SystemConstellationRegion(ctx context.Context, db *sql.DB, systemID int64) (constellationID, regionID int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT constellationID, regionID FROM mapSolarSystems WHERE solarSystemID = ?`,
		systemID).Scan(&constellationID, &regionID)
	if err != nil {
		err = fmt.Errorf("SystemConstellationRegion %d: %w", systemID, err)
	}
	return
}

// ── Security status repair ───────────────────────────────────────────────────

// SecRepairQuote prices scrubbing sec back to 0.0: one unit per 0.1 of negative status, rounded
// up. A clean or positive record quotes zero.
func SecRepairQuote(sec float64, per01 int) int64 {
	if sec >= -0.0001 {
		return 0
	}
	units := int64(-sec * 10)
	if float64(units) < -sec*10-0.0001 {
		units++
	}
	if units < 1 {
		units = 1
	}
	return units * int64(per01)
}

// RepairSecurityStatus zeroes a negative security rating and reports whether a row changed. The
// WHERE guard is the payment gate: the caller charges only after this returns true, so a
// concurrent repair or a rating that turned positive since the quote costs nothing.
func RepairSecurityStatus(ctx context.Context, db *sql.DB, charID int64) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE character_ SET securityRating = 0.0
		 WHERE characterID = ? AND securityRating < 0`, charID)
	if err != nil {
		return false, fmt.Errorf("RepairSecurityStatus: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── Mission history ──────────────────────────────────────────────────────────

// AgentMissionStats is the character's record with one agent, straight off
// the persisted characterMissions rows (quit missions land in 'failed').
type AgentMissionStats struct {
	Succeeded int
	Failed    int
	Declined  int
}

// GetAgentMissionStats counts the character's mission history with an agent.
func GetAgentMissionStats(ctx context.Context, db *sql.DB, charID, agentID int64) (AgentMissionStats, error) {
	var s AgentMissionStats
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(state = 'completed'), 0),
		       COALESCE(SUM(state = 'failed'), 0),
		       COALESCE(SUM(state = 'declined'), 0)
		  FROM characterMissions WHERE charID = ? AND agentID = ?`,
		charID, agentID).Scan(&s.Succeeded, &s.Failed, &s.Declined)
	if err != nil {
		return s, fmt.Errorf("GetAgentMissionStats: %w", err)
	}
	return s, nil
}

// ── Dialogue texts ───────────────────────────────────────────────────────────

// ServiceTextDef documents one authored dialogue node: where the text shows up and which player
// choices lead in or out of it (the flow the ESP page renders).
type ServiceTextDef struct {
	Key      string
	Category string // page grouping: "shell", "locator", "secstatus", "gifts", "rumors"
	Where    string // which window/state the text appears in
	Choices  string // the player responses offered alongside it, and where they lead
	Default  string
}

// DialogueCategories is the page display order with headers. "shell" is the conversation frame
// around missions (greeting, refusals, button captions); a mission template's own text stays in
// the mission editor.
var DialogueCategories = []struct{ Key, Name string }{
	{"shell", "Conversation Shell"},
	{"locator", "Locator Services"},
	{"secstatus", "Security Status Services"},
	{"gifts", "Donations/Gifts/Bribes"},
	{"rumors", "Rumour Services"},
}

// ServiceTextDefs is every authorable agent line outside mission templates, in flow order per
// category. Lines carry {token} placeholders (pilot, target, fee, cost, range, delay, sec, lp,
// required, effective, available, cap, reason) filled by SubstituteServiceText.
var ServiceTextDefs = []ServiceTextDef{
	// ── Conversation shell ──────────────────────────────────────────────
	{"shell.greeting", "shell", "Conversation: opening line of a mission agent with nothing pending", "[shell.btn.requestmission] → mission offer; [shell.btn.viewoffers] (when offers exist); service buttons", "Greetings. Would you like a mission?"},
	{"shell.nomissions", "shell", "Conversation: no usable template for this agent's division/level", "[shell.btn.requestmission] to try again", "I have no missions available for you at this time."},
	{"shell.cooldown", "shell", "Conversation: the pilot hit the mission cap with this agent", "[shell.btn.quit] → close; offers stay reachable", "I have handed you enough work for one day, {pilot}. My other clients need attention too — come back after <b>{available}</b>."},
	{"shell.standing.low", "shell", "Conversation: effective standing below the agent's requirement", "conversation over", "I do not deal with just anyone who walks through my door. Prove your worth to my corporation first — I require an effective standing of <b>{required}</b> toward me, my corporation or our faction before we can do business. Yours is <b>{effective}</b>."},
	{"shell.standing.criminal", "shell", "Conversation: effective standing at or below −2.0", "conversation over", "Your reputation precedes you — and not in a good way. I have nothing to say to you until you mend your ways."},
	{"shell.storyline.brushoff", "shell", "Conversation: storyline agent, no invitation earned", "[shell.btn.viewoffers] (when offers exist); [shell.btn.quit]", "My work is not the sort that gets handed out at a counter. When one of my colleagues decides you are worth my time, you will hear from me directly."},
	{"shell.offers.intro", "shell", "Conversation: the special-offers menu", "one choice per offer; [shell.btn.quit]", "You currently have {lp} loyalty points with me.<br><br>I have the following special offers available:"},
	{"shell.offers.none", "shell", "Conversation: offers menu with nothing to sell", "[shell.btn.quit]", "I have no special offers for you at this time."},
	{"shell.noactive", "shell", "Conversation: view/complete/quit clicked with no active mission", "[shell.btn.requestmission]", "You don't have an active mission with me."},
	{"shell.objectives.incomplete", "shell", "Conversation: complete clicked before the objectives are done", "[shell.btn.quit]", "Your mission objectives are not yet complete. {reason}"},
	{"shell.decline.free", "shell", "Conversation: added after the decline text when it cost nothing (no other decline with this agent inside the window)", "as the decline", "No harm done this time. Turn me down again before <b>{available}</b>, though, and my corporation will hear of it."},
	{"shell.decline.penalty", "shell", "Conversation: added after the decline text when an earlier decline is still inside the window (standing loss)", "as the decline", "That is the second job you have refused me in a short time, {pilot}. My corporation will hear of it."},
	{"shell.btn.requestmission", "shell", "Conversation: player choice caption", "→ mission offer / shell.nomissions / shell.cooldown", "Request Mission"},
	{"shell.btn.viewoffers", "shell", "Conversation: player choice caption", "→ shell.offers.intro / shell.offers.none", "View Special Offers"},
	{"shell.btn.quit", "shell", "Conversation: player choice caption", "→ closes the exchange (reopens on the greeting)", "Quit"},
	{"shell.btn.accept", "shell", "Conversation: generic caption when the mission template words no acceptLabel", "→ mission accepted", "Accept"},
	{"shell.btn.decline", "shell", "Conversation: generic caption when the template words no declineLabel", "→ mission declined: shell.decline.free, or shell.decline.penalty (standing loss) inside the window", "Decline"},
	{"shell.btn.viewmission", "shell", "Conversation: generic caption when the template words no viewMissionLabel", "→ mission details", "View Mission"},
	{"shell.btn.complete", "shell", "Conversation: generic caption when the template words no completeLabel", "→ hand-in / shell.objectives.incomplete", "Complete Mission"},
	{"shell.btn.quitmission", "shell", "Conversation: generic caption when the template words no quitMissionLabel", "→ mission abandoned (standing loss)", "Quit Mission"},
	{"shell.btn.moreinfo", "shell", "Conversation: generic caption when the template words no moreInfoLabel", "→ the template's More Information text", "More Information"},
	// Locator flow. Code appends the name-entry form input; the authored text is only the words
	// around it.
	{"locator.button", "locator", "Conversation: choice on the opening menu", "→ locator.prompt", "Locate a pilot"},
	{"locator.prompt", "locator", "Conversation: after choosing to locate", "type a name + [locator.submit] → locator.quote / locator.notfound / locator.self / locator.outofrange; [locator.cancel] → opening menu", "Certainly. Give me a name and I will make some calls — my reach extends to {range}. Who are you looking for?"},
	{"locator.submit", "locator", "Conversation: caption on the name-entry submit button", "submits the typed name", "Ask"},
	{"locator.notfound", "locator", "Conversation: the typed name matches no pilot", "type another name, or [locator.cancel] → opening menu", "I have no record of any capsuleer by the name \"{target}\". Check the spelling and try again."},
	{"locator.self", "locator", "Conversation: the pilot asked for themselves", "type another name, or [locator.cancel] → opening menu", "You are standing right in front of me, {pilot}. Very droll. Who are you actually looking for?"},
	{"locator.outofrange", "locator", "Conversation: target found but beyond the agent's reach", "type another name, or [locator.cancel] → opening menu", "{target} is beyond my reach — my contacts only cover {range}. Find an agent with longer arms."},
	{"locator.busy", "locator", "Conversation: a search for this pilot is already running", "[locator.cancel] → opening menu", "I am still working on your last request. These things take time — watch your mail."},
	{"locator.quote", "locator", "Conversation: target found, fee quoted", "[locator.pay] → locator.paid / locator.poor; [locator.cancel] → opening menu", "I can find {target} for you. It will cost you <b>{fee} ISK</b> — payable in advance, results by mail within {delay} minutes. Do we have a deal?"},
	{"locator.pay", "locator", "Conversation: caption on the pay button", "→ locator.paid / locator.poor", "Pay {fee} ISK"},
	{"locator.poor", "locator", "Conversation: cannot afford the fee", "[locator.cancel] → opening menu", "You do not have {fee} ISK. Come back when you can pay for my time."},
	{"locator.paid", "locator", "Conversation: fee taken, search started", "conversation over", "Consider it done. My people are already asking around — you will have mail from me within {delay} minutes or so."},
	{"locator.cancel", "locator", "Conversation: caption on the never-mind button", "→ opening menu", "Never mind"},
	{"locator.mail.docked", "locator", "EVE-mail: the located pilot is docked", "mail body; {station} renders as a station link", "You asked me to find <b>{target}</b>.<br><br>My sources place them docked at {station}, in the {system} system.<br><br>Information this fresh spoils quickly. Use it soon."},
	{"locator.mail.inspace", "locator", "EVE-mail: the located pilot is in space (or last seen there)", "mail body; {system} renders as a solar system link", "You asked me to find <b>{target}</b>.<br><br>My sources place them in space in the {system} system.<br><br>Information this fresh spoils quickly. Use it soon."},

	// Security status repair.
	{"sec.button", "secstatus", "Conversation: choice on the opening menu (only shown when sec < 0)", "→ sec.quote", "I need my security status repaired"},
	{"sec.quote", "secstatus", "Conversation: repair quoted", "[sec.pay] → sec.paid / sec.poor; [sec.cancel] → opening menu", "Your security status stands at <b>{sec}</b>. I can make that record read 0.0 again — the paperwork will cost you <b>{cost} ISK</b>. I do not polish clean records, I only mend broken ones."},
	{"sec.pay", "secstatus", "Conversation: caption on the pay button", "→ sec.paid / sec.poor", "Pay {cost} ISK"},
	{"sec.clean", "secstatus", "Conversation: pilot's record is not negative", "[sec.cancel] → opening menu", "Your record is clean, {pilot}. There is nothing for me to repair — and no, I will not make it any shinier than 0.0."},
	{"sec.poor", "secstatus", "Conversation: cannot afford the repair", "[sec.cancel] → opening menu", "The paperwork costs {cost} ISK and you do not have it. Crime may pay, but absolution certainly does."},
	{"sec.paid", "secstatus", "Conversation: repair done", "conversation over", "It is done. As far as CONCORD's records are concerned, you are a model citizen — sec status 0.0. Try to keep it that way, or don't; repeat business is good business."},
	{"sec.cancel", "secstatus", "Conversation: caption on the never-mind button", "→ opening menu", "Never mind"},

	// Stubs. The buttons exist so the services read as real; the replies
	// admit nothing is behind them yet.
	{"gifts.button", "gifts", "Conversation: choice on the opening menu", "→ gifts.stub", "I have a gift for you"},
	{"gifts.stub", "gifts", "Conversation: reply to offering a gift", "conversation over", "How thoughtful. Unfortunately my compliance department is mid-audit — I cannot accept anything at present. Do try again some other time."},
	{"rumors.button", "rumors", "Conversation: choice on the opening menu", "→ rumors.stub", "Heard any rumours?"},
	{"rumors.stub", "rumors", "Conversation: reply to asking for rumours", "conversation over", "Plenty — but nothing I can share today. My sources are being... recalibrated. Ask again in a while."},
}

// serviceTextDefaults indexes ServiceTextDefs by key.
var serviceTextDefaults = func() map[string]string {
	m := make(map[string]string, len(ServiceTextDefs))
	for _, d := range ServiceTextDefs {
		m[d.Key] = d.Default
	}
	return m
}()

// GetAgentText returns the effective dialogue text for a key as spoken by an
// agent of the given division: the division's authored line wins, then the
// global (divisionID 0) authored line, then the shipped default. Pass
// divisionID 0 when no agent is in scope (e.g. the not-an-agent reply).
func GetAgentText(ctx context.Context, db *sql.DB, divisionID int64, key string) string {
	var body string
	err := db.QueryRowContext(ctx, `
		SELECT body FROM agtServiceTexts
		 WHERE textKey = ? AND divisionID IN (?, 0)
		 ORDER BY divisionID DESC LIMIT 1`, key, divisionID).Scan(&body)
	if err == nil && strings.TrimSpace(body) != "" {
		return body
	}
	return serviceTextDefaults[key]
}

// GetServiceText is GetAgentText with no division: the global line.
func GetServiceText(ctx context.Context, db *sql.DB, key string) string {
	return GetAgentText(ctx, db, 0, key)
}

// SetAgentText stores an authored line for one division (0 = global). A body equal to what the
// scope would inherit (the global line, or the shipped default for global) deletes the row, so
// upstream changes keep flowing through.
func SetAgentText(ctx context.Context, db *sql.DB, divisionID int64, key, body string) error {
	if _, ok := serviceTextDefaults[key]; !ok {
		return fmt.Errorf("SetAgentText: unknown key %q", key)
	}
	inherited := serviceTextDefaults[key]
	if divisionID != 0 {
		inherited = GetAgentText(ctx, db, 0, key)
	}
	if strings.TrimSpace(body) == "" || body == inherited {
		_, err := db.ExecContext(ctx,
			`DELETE FROM agtServiceTexts WHERE divisionID = ? AND textKey = ?`, divisionID, key)
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO agtServiceTexts (divisionID, textKey, body) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE body = VALUES(body)`, divisionID, key, body)
	if err != nil {
		return fmt.Errorf("SetAgentText %q div=%d: %w", key, divisionID, err)
	}
	return nil
}

// ── Mission cooldown ─────────────────────────────────────────────────────────
//
// An agent hands out at most missions.agentMissionCap missions per missions.agentCooldownHours to
// one character, counted on acceptedAt per (character, agent). Declined offers do not count and
// cap 0 disables the gate. See docs/agent-services.md.

const (
	settingAgentMissionCap    = "missions.agentMissionCap"
	settingAgentCooldownHours = "missions.agentCooldownHours"
	settingAgentDeclineHours  = "missions.agentDeclineHours"
)

// Cooldown defaults: three missions, then come back tomorrow. One decline per agent every four
// hours is free; another inside the window costs the template's standing loss.
const (
	DefaultAgentMissionCap    = 3
	DefaultAgentCooldownHours = 24
	DefaultAgentDeclineHours  = 4
)

// GetAgentDeclineHours returns the free-decline window (0 = every decline costs standing).
func GetAgentDeclineHours(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingAgentDeclineHours, DefaultAgentDeclineHours, 0, 720)
}

// SetAgentDeclineHours stores the free-decline window.
func SetAgentDeclineHours(ctx context.Context, db *sql.DB, v int) error {
	return SetServerSetting(ctx, db, settingAgentDeclineHours, fmt.Sprintf("%d", v))
}

// RecentAgentDecline reports whether the character declined an offer from this agent inside the
// free-decline window, and when that decline leaves it. A recent decline makes the next one cost
// standing. A window of 0 reports every decline as recent.
func RecentAgentDecline(ctx context.Context, db *sql.DB, charID, agentID int64) (recent bool, freeAt time.Time, err error) {
	hours := GetAgentDeclineHours(ctx, db)
	if hours <= 0 {
		return true, time.Time{}, nil
	}
	var last sql.NullTime
	err = db.QueryRowContext(ctx, `
		SELECT MAX(declinedAt) FROM characterMissions
		 WHERE charID = ? AND agentID = ?
		   AND declinedAt >= NOW() - INTERVAL ? HOUR`,
		charID, agentID, hours).Scan(&last)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("RecentAgentDecline: %w", err)
	}
	if !last.Valid {
		return false, time.Time{}, nil
	}
	return true, last.Time.Add(time.Duration(hours) * time.Hour), nil
}

// GetAgentMissionCap returns missions per window (0 = unlimited).
func GetAgentMissionCap(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingAgentMissionCap, DefaultAgentMissionCap, 0, 1000)
}

// SetAgentMissionCap stores the cap.
func SetAgentMissionCap(ctx context.Context, db *sql.DB, v int) error {
	return SetServerSetting(ctx, db, settingAgentMissionCap, fmt.Sprintf("%d", v))
}

// GetAgentCooldownHours returns the rolling window length.
func GetAgentCooldownHours(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingAgentCooldownHours, DefaultAgentCooldownHours, 1, 720)
}

// SetAgentCooldownHours stores the window length.
func SetAgentCooldownHours(ctx context.Context, db *sql.DB, v int) error {
	return SetServerSetting(ctx, db, settingAgentCooldownHours, fmt.Sprintf("%d", v))
}

// AgentMissionCooldown reports whether the character has exhausted this agent's mission cap, and
// when the oldest accepted mission in the window rolls out of it.
func AgentMissionCooldown(ctx context.Context, db *sql.DB, charID, agentID int64) (onCooldown bool, availableAt time.Time, err error) {
	capN := GetAgentMissionCap(ctx, db)
	if capN <= 0 {
		return false, time.Time{}, nil
	}
	hours := GetAgentCooldownHours(ctx, db)
	var n int
	var earliest sql.NullTime
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(acceptedAt) FROM characterMissions
		 WHERE charID = ? AND agentID = ?
		   AND acceptedAt >= NOW() - INTERVAL ? HOUR`,
		charID, agentID, hours).Scan(&n, &earliest)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("AgentMissionCooldown: %w", err)
	}
	if n < capN || !earliest.Valid {
		return false, time.Time{}, nil
	}
	return true, earliest.Time.Add(time.Duration(hours) * time.Hour), nil
}

// SubstituteServiceText fills the {token} placeholders.
func SubstituteServiceText(text string, tokens map[string]string) string {
	for k, v := range tokens {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return text
}

// ── ESP support ──────────────────────────────────────────────────────────────

// ESPListServiceDivisions returns the whole distribution grid.
func ESPListServiceDivisions(ctx context.Context, db *sql.DB) ([]ServiceDivisionRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT serviceKey, divisionID, minLevel FROM agtServiceDivisions`)
	if err != nil {
		return nil, fmt.Errorf("ESPListServiceDivisions: %w", err)
	}
	defer rows.Close()
	var out []ServiceDivisionRow
	for rows.Next() {
		var r ServiceDivisionRow
		if err := rows.Scan(&r.ServiceKey, &r.DivisionID, &r.MinLevel); err != nil {
			return nil, fmt.Errorf("ESPListServiceDivisions scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPReplaceServiceDivisions swaps the entire distribution grid in one transaction; the page
// posts the whole grid, so a partial write would tear it.
func ESPReplaceServiceDivisions(ctx context.Context, db *sql.DB, grid []ServiceDivisionRow) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPReplaceServiceDivisions begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agtServiceDivisions`); err != nil {
		return fmt.Errorf("ESPReplaceServiceDivisions clear: %w", err)
	}
	for _, r := range grid {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agtServiceDivisions (serviceKey, divisionID, minLevel)
			VALUES (?, ?, ?)`, r.ServiceKey, r.DivisionID, r.MinLevel); err != nil {
			return fmt.Errorf("ESPReplaceServiceDivisions insert: %w", err)
		}
	}
	return tx.Commit()
}

// ServiceOverrideRow is one per-agent force with its display context.
type ServiceOverrideRow struct {
	AgentID    int64
	AgentName  string
	ServiceKey string
	Enabled    bool
}

// ESPListServiceOverrides returns all per-agent overrides with names.
func ESPListServiceOverrides(ctx context.Context, db *sql.DB) ([]ServiceOverrideRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.agentID, COALESCE(c.characterName, ''), o.serviceKey, o.enabled
		  FROM agtServiceOverrides o
		  LEFT JOIN character_ c ON c.characterID = o.agentID
		 ORDER BY o.agentID, o.serviceKey`)
	if err != nil {
		return nil, fmt.Errorf("ESPListServiceOverrides: %w", err)
	}
	defer rows.Close()
	var out []ServiceOverrideRow
	for rows.Next() {
		var r ServiceOverrideRow
		if err := rows.Scan(&r.AgentID, &r.AgentName, &r.ServiceKey, &r.Enabled); err != nil {
			return nil, fmt.Errorf("ESPListServiceOverrides scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPSetServiceOverride forces a service on or off for one agent.
func ESPSetServiceOverride(ctx context.Context, db *sql.DB, agentID int64, serviceKey string, enabled bool) error {
	if _, ok := ServiceNames[serviceKey]; !ok {
		return fmt.Errorf("unknown service %q", serviceKey)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agtAgents WHERE agentID = ?`, agentID).Scan(&n); err != nil {
		return fmt.Errorf("ESPSetServiceOverride: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("agent %d does not exist", agentID)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO agtServiceOverrides (agentID, serviceKey, enabled) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE enabled = VALUES(enabled)`, agentID, serviceKey, enabled)
	if err != nil {
		return fmt.Errorf("ESPSetServiceOverride: %w", err)
	}
	return nil
}

// ESPDeleteServiceOverride removes a force, returning the agent to its
// division's distribution.
func ESPDeleteServiceOverride(ctx context.Context, db *sql.DB, agentID int64, serviceKey string) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM agtServiceOverrides WHERE agentID = ? AND serviceKey = ?`,
		agentID, serviceKey)
	if err != nil {
		return fmt.Errorf("ESPDeleteServiceOverride: %w", err)
	}
	return nil
}

// ESPServiceAgentCounts counts, per service, how many agents currently offer
// it (division grants adjusted by overrides).
func ESPServiceAgentCounts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	out := map[string]int{}
	for _, key := range ServiceKeys {
		var n int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM agtAgents a
			 WHERE (
			         EXISTS (SELECT 1 FROM agtServiceDivisions d
			                  WHERE d.serviceKey = ? AND d.divisionID = a.divisionID
			                    AND a.level >= d.minLevel)
			         AND NOT EXISTS (SELECT 1 FROM agtServiceOverrides o
			                          WHERE o.agentID = a.agentID AND o.serviceKey = ? AND o.enabled = 0)
			       )
			    OR EXISTS (SELECT 1 FROM agtServiceOverrides o
			                WHERE o.agentID = a.agentID AND o.serviceKey = ? AND o.enabled = 1)`,
			key, key, key).Scan(&n)
		if err != nil {
			return out, fmt.Errorf("ESPServiceAgentCounts %s: %w", key, err)
		}
		out[key] = n
	}
	return out, nil
}

// ESPAgentServiceSearchRow is one agent in the per-agent lookup.
type ESPAgentServiceSearchRow struct {
	AgentID      int64
	AgentName    string
	CorpName     string
	DivisionID   int64
	DivisionName string
	Level        int
	Services     []string // effective service names, display order
}

// ESPAgentServiceSearch finds agents by name (or exact ID) and resolves each
// one's effective services.
func ESPAgentServiceSearch(ctx context.Context, db *sql.DB, q string, limit int) ([]ESPAgentServiceSearchRow, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.agentID, COALESCE(c.characterName, ''), COALESCE(corp.corporationName, ''),
		       COALESCE(a.divisionID, 0), COALESCE(d.divisionName, ''), COALESCE(a.level, 1)
		  FROM agtAgents a
		  LEFT JOIN character_ c ON c.characterID = a.agentID
		  LEFT JOIN corporation corp ON corp.corporationID = a.corporationID
		  LEFT JOIN crpNPCDivisions d ON d.divisionID = a.divisionID
		 WHERE c.characterName LIKE ? OR a.agentID = ?
		 ORDER BY c.characterName LIMIT ?`,
		"%"+q+"%", q, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPAgentServiceSearch: %w", err)
	}
	defer rows.Close()
	var out []ESPAgentServiceSearchRow
	for rows.Next() {
		var r ESPAgentServiceSearchRow
		if err := rows.Scan(&r.AgentID, &r.AgentName, &r.CorpName,
			&r.DivisionID, &r.DivisionName, &r.Level); err != nil {
			return nil, fmt.Errorf("ESPAgentServiceSearch scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		set, err := AgentServiceSet(ctx, db, &AgentInfo{
			AgentID: out[i].AgentID, DivisionID: out[i].DivisionID, Level: out[i].Level,
		})
		if err != nil {
			return nil, err
		}
		for _, key := range ServiceKeys {
			if set[key] {
				out[i].Services = append(out[i].Services, ServiceNames[key])
			}
		}
	}
	return out, nil
}

// ESPLocatorJobRow is one row in the locator job log.
type ESPLocatorJobRow struct {
	JobID       int64
	CharName    string
	AgentName   string
	TargetName  string
	Fee         int64
	RequestedAt time.Time
	DueAt       time.Time
	Delivered   bool
}

// ESPRecentLocatorJobs returns the latest locator jobs with names resolved.
func ESPRecentLocatorJobs(ctx context.Context, db *sql.DB, limit int) ([]ESPLocatorJobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, `
		SELECT j.jobID, COALESCE(c.characterName, ''), COALESCE(a.characterName, ''),
		       COALESCE(t.characterName, ''), j.fee, j.requestedAt, j.dueAt, j.delivered
		  FROM agtLocatorJobs j
		  LEFT JOIN character_ c ON c.characterID = j.charID
		  LEFT JOIN character_ a ON a.characterID = j.agentID
		  LEFT JOIN character_ t ON t.characterID = j.targetID
		 ORDER BY j.jobID DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ESPRecentLocatorJobs: %w", err)
	}
	defer rows.Close()
	var out []ESPLocatorJobRow
	for rows.Next() {
		var r ESPLocatorJobRow
		if err := rows.Scan(&r.JobID, &r.CharName, &r.AgentName, &r.TargetName,
			&r.Fee, &r.RequestedAt, &r.DueAt, &r.Delivered); err != nil {
			return nil, fmt.Errorf("ESPRecentLocatorJobs scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── ESP dialogue listing ─────────────────────────────────────────────────────

// ESPDialogueRow is one text key resolved for a division scope: the line an agent of that
// division would say (Effective), where it came from (Source: "default", "global" or "division"),
// and what the scope inherits if its own row were deleted (Inherited, the textarea's reset target).
type ESPDialogueRow struct {
	Key       string
	Category  string
	Where     string
	Choices   string
	Effective string
	Inherited string
	Source    string
}

// ESPListDialogue resolves every text key for one division (0 = the global
// scope itself). One query loads all authored rows for the division and the
// global scope; resolution is done in order of ServiceTextDefs so the page
// keeps flow order.
func ESPListDialogue(ctx context.Context, db *sql.DB, divisionID int64) ([]ESPDialogueRow, error) {
	authored := map[[2]interface{}]string{}
	rows, err := db.QueryContext(ctx, `
		SELECT divisionID, textKey, body FROM agtServiceTexts
		 WHERE divisionID IN (?, 0)`, divisionID)
	if err != nil {
		return nil, fmt.Errorf("ESPListDialogue: %w", err)
	}
	for rows.Next() {
		var div int64
		var key, body string
		if err := rows.Scan(&div, &key, &body); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ESPListDialogue scan: %w", err)
		}
		authored[[2]interface{}{div, key}] = body
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ESPDialogueRow, 0, len(ServiceTextDefs))
	for _, def := range ServiceTextDefs {
		r := ESPDialogueRow{
			Key: def.Key, Category: def.Category, Where: def.Where, Choices: def.Choices,
		}
		global, hasGlobal := authored[[2]interface{}{int64(0), def.Key}]
		if !hasGlobal {
			global = def.Default
		}
		if divisionID != 0 {
			r.Inherited = global
			if body, ok := authored[[2]interface{}{divisionID, def.Key}]; ok {
				r.Effective, r.Source = body, "division"
			} else if hasGlobal {
				r.Effective, r.Source = global, "global"
			} else {
				r.Effective, r.Source = def.Default, "default"
			}
		} else {
			r.Inherited = def.Default
			if hasGlobal {
				r.Effective, r.Source = global, "global"
			} else {
				r.Effective, r.Source = def.Default, "default"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ESPDialogueDivisionCounts returns, per division, how many authored division-scoped lines exist:
// the dialogue page's "this division deviates" marker.
func ESPDialogueDivisionCounts(ctx context.Context, db *sql.DB) (map[int64]int, error) {
	out := map[int64]int{}
	rows, err := db.QueryContext(ctx, `
		SELECT divisionID, COUNT(*) FROM agtServiceTexts
		 WHERE divisionID <> 0 GROUP BY divisionID`)
	if err != nil {
		return out, fmt.Errorf("ESPDialogueDivisionCounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var div int64
		var n int
		if err := rows.Scan(&div, &n); err != nil {
			return out, fmt.Errorf("ESPDialogueDivisionCounts scan: %w", err)
		}
		out[div] = n
	}
	return out, rows.Err()
}
