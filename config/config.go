// Package config loads and holds the server configuration from config.json.
//
// config.json is read once at boot. Live dials are server_settings rows edited in ESP, and the
// "settings" block supplies a default for any such key. Precedence: table row, then this block,
// then the Go default. db.GetServerSetting applies it; hot paths read db.CachedSetting*.
//
// The file may carry // line and /* block */ comments (stripJSONComments). Other readers of the
// file, such as a JSON linter or editor, may flag them.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// AuthenticationConfig controls login behaviour.
type AuthenticationConfig struct {
	AutoAccount bool  `json:"autoaccount"` // auto-create accounts on first login
	Role        int64 `json:"role"`        // default role for new accounts (default 3 = ROLE_LOGIN|ROLE_PLAYER)
	SlashRoles  int64 `json:"slashRoles"`  // role bits allowed to run slash commands, ANY bit passes (default GML|GMH|ADMIN|LEGIONEER)
}

// CharacterConfig controls new-character defaults.
type CharacterConfig struct {
	StartBalance float64 `json:"startBalance"` // ISK given to newly created characters
}

// AgentsConfig controls agent side-services (the non-mission things an agent can be paid to
// do). Fees live in server_settings (ESP Agents, Services page); only the timing knob lives here.
type AgentsConfig struct {
	// LocatorDelayMinutes is how long a paid locator agent takes before the
	// "I found your pilot" EVE-mail arrives. Defaults to 3.
	LocatorDelayMinutes int `json:"locatorDelayMinutes"`
}

// JumpCloneConfig controls jump clone installation. The fee and the jump
// cooldown live in server_settings (jumpclone.installCost,
// jumpclone.cooldownHours); the standing requirement lives here.
type JumpCloneConfig struct {
	// MinStanding is the standing toward a station's owner corporation a pilot needs to install a
	// jump clone there (8.0 when omitted, capped at 10). The owner's own members are exempt; 0 or
	// below turns the check off.
	MinStanding *float64 `json:"minStanding"`
}

// RatesConfig holds global gameplay rate multipliers copied into the rates package at startup.
// Bigger means more or faster. Fields absent from config.json default to 1.0, so an explicit 0
// is honoured (npcBounty 0 disables bounties).
type RatesConfig struct {
	SkillTraining   float64 `json:"skillTraining"`   // SP/min multiplier (2 = train twice as fast)
	NPCBounty       float64 `json:"npcBounty"`       // ISK bounty multiplier for NPC kills
	MiningYield     float64 `json:"miningYield"`     // m3 per mining cycle multiplier
	LootDrop        float64 `json:"lootDrop"`        // loot drop chance multiplier
	PlayerDamage    float64 `json:"playerDamage"`    // player turret/missile damage multiplier
	NPCDamage       float64 `json:"npcDamage"`       // NPC turret/missile damage multiplier
	IndustrySpeed   float64 `json:"industrySpeed"`   // divides all industry job times (2 = twice as fast)
	ProductionSpeed float64 `json:"productionSpeed"` // divides manufacturing job time, stacks with industrySpeed
	ResearchSpeed   float64 `json:"researchSpeed"`   // divides ME/PE research and copy job time, stacks with industrySpeed
	ResearchPoints  float64 `json:"researchPoints"`  // R&D agent research point accrual multiplier (0 disables)
	MissionReward   float64 `json:"missionReward"`   // mission ISK reward and time bonus multiplier
	LoyaltyPoints   float64 `json:"loyaltyPoints"`   // mission LP reward multiplier (0 disables LP)
	StandingGain    float64 `json:"standingGain"`    // multiplier on mission standing changes, gain and loss
	RatStandingLoss float64 `json:"ratStandingLoss"` // multiplier on the faction standing lost per NPC kill (0 disables)
}

// DefaultRates returns a RatesConfig with every multiplier at 1.0.
func DefaultRates() RatesConfig {
	return RatesConfig{
		SkillTraining:   1.0,
		NPCBounty:       1.0,
		MiningYield:     1.0,
		LootDrop:        1.0,
		PlayerDamage:    1.0,
		NPCDamage:       1.0,
		IndustrySpeed:   1.0,
		ProductionSpeed: 1.0,
		ResearchSpeed:   1.0,
		ResearchPoints:  1.0,
		MissionReward:   1.0,
		LoyaltyPoints:   1.0,
		StandingGain:    1.0,
		RatStandingLoss: 1.0,
	}
}

// ESPConfig controls the built-in ESP web admin panel.
type ESPConfig struct {
	Enabled    bool     `json:"enabled"`    // start the HTTP admin panel
	Listen     string   `json:"listen"`     // HTTP listen address (default "127.0.0.1:26080")
	MinRole    int64    `json:"minRole"`    // numeric floor on the account role, checked in addition to staffRoles (default 2)
	StaffRoles int64    `json:"staffRoles"` // role bits allowed into ESP, ANY bit passes (default GML|GMH|ADMIN = 56)
	ClientPath string   `json:"clientPath"` // Red Moon Rising client folder (the one with eve.exe): icons, portraits, textures, model source, intro subtitles
	NewsListen string   `json:"newsListen"` // client news feed listener (":80" typical, "" disables)
	ModelsDirs []string `json:"modelsDirs"` // folders of converted .glb models for the admin panel 3D viewer
	// PublicURL is the panel's address as a reader reaches it ("http://myhost:26080"); player
	// corporations without a homepage get their /corporation/<id> page under it. Empty derives
	// http://127.0.0.1<port> from Listen.
	PublicURL string `json:"publicURL"`
	// SkillMarkup multiplies the market average when the skill window sells a
	// skillbook (db.SkillPriceMarkup). 0 or below 1 keeps the 1.10 default.
	SkillMarkup float64 `json:"skillMarkup"`
	// Pprof mounts Go's /debug/pprof profiler on the panel (behind its auth).
	// Omitted = on; false removes the routes entirely.
	Pprof *bool `json:"pprof"`
	// SessionDays is how long a panel login cookie lives (default 30).
	SessionDays int `json:"sessionDays"`
	// AllowedHosts is the extra Host names both HTTP servers answer to (a LAN name or IP a
	// reader types). Loopback, each listener's own host and the client's two CCP hostnames
	// are always allowed; anything else is refused with 421 (esp.hostGuard).
	AllowedHosts []string `json:"allowedHosts"`
}

// PublicBase is the panel's address as a reader reaches it, or "" with the
// panel off: PublicURL if set, else http://127.0.0.1<port> from Listen (a
// wildcard bind is not an address anyone can open).
func (e ESPConfig) PublicBase() string {
	if !e.Enabled {
		return ""
	}
	if u := strings.TrimRight(strings.TrimSpace(e.PublicURL), "/"); u != "" {
		return u
	}
	host, port, err := net.SplitHostPort(e.Listen)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// NPCMarketConfig controls the NPC market supply/demand simulation
// (db.TickNPCMarketDynamics): periodic repricing of trade-good Seeder orders
// from player trade flow, plus restocking of all Seeder orders.
type NPCMarketConfig struct {
	Dynamics    bool `json:"dynamics"`    // run the periodic repricing sim (default true)
	TickMinutes int  `json:"tickMinutes"` // sim step interval in minutes (default 60)
}

// DefaultNPCMarket returns the NPC market sim defaults: enabled, hourly.
func DefaultNPCMarket() NPCMarketConfig {
	return NPCMarketConfig{Dynamics: true, TickMinutes: 60}
}

// Config is the top-level structure mirroring config.json.
type Config struct {
	Server         ServerConfig         `json:"server"`
	Database       DatabaseConfig       `json:"database"`
	Authentication AuthenticationConfig `json:"authentication"`
	Character      CharacterConfig      `json:"character"`
	Rates          RatesConfig          `json:"rates"`
	Agents         AgentsConfig         `json:"agents"`
	JumpClone      JumpCloneConfig      `json:"jumpClone"`
	NPCMarket      NPCMarketConfig      `json:"npcMarket"`
	ESP            ESPConfig            `json:"esp"`
	// Settings supplies defaults for server_settings keys, e.g. {"docking.rangeM": 2500}. Values
	// are numbers, strings, booleans (stored 1/0) or number arrays (stored comma-separated).
	Settings map[string]json.RawMessage `json:"settings"`
	// RPCCoveragePath is where the RPC coverage ledger is persisted across restarts; it backs ESP
	// /coverage and cmd/rpc-coverage. "-" disables persistence (the in-memory ledger still works).
	RPCCoveragePath string `json:"rpcCoveragePath"`
	// Modules holds per-module config blocks keyed by module name; each
	// enabled module (modules/modules.go) parses its own block. See
	// modules/README.md.
	Modules map[string]json.RawMessage `json:"modules"`
}

// ServerConfig holds network listener settings.
type ServerConfig struct {
	// Listen is the TCP address the server binds to (default "127.0.0.1:26000"). The client
	// always connects to port 26000; widen the host, never the port.
	Listen string `json:"listen"`

	// LogLevel is "error", "info" (default), "debug" or "trace". It gates only success-path trace;
	// failures and anything the ESP Bug Tracker classifies are always emitted. See package logx.
	LogLevel string `json:"logLevel"`

	// EnforceStampBudget turns destiny's stamp ceiling from a counter into a refusal
	// (destiny.StampBudgetEnforced). A refused send is a dropped destiny update, so keep it off
	// until `/stamps` reports no sends over the ceiling and the never-drop paths use SendStampedUrgent.
	EnforceStampBudget bool `json:"enforceStampBudget"`

	// StampResyncLead is how far a client's destiny stamp stream may run ahead of the wall clock
	// before the ballpark reconciler forces a full SetState to re-anchor it. 0 disables; omitted
	// uses the default (120). A firing on a healthy server means a sender mints stamps per event.
	StampResyncLead *int `json:"stampResyncLead"`

	// DestinyTraceAccount logs every DoDestinyUpdate sent to this account ID with its
	// stamps, action names and the wall-clock stamp (destiny.TraceAccount). 0 = off.
	DestinyTraceAccount int64 `json:"destinyTraceAccount"`

	// CombatDebug re-enables the per-hit combat trace (weapon hits, missile
	// aim/launch/impact, NPC fire, HP pushes). Off by default: a fleet fight
	// writes one line per hit per log, and the write is on the combat path.
	CombatDebug bool `json:"combatDebug"`

	// MissileHardpointOffsets moves each launcher's missile ball to a per-slot offset after the
	// client's LaunchMissile snap, so a volley's flights start from distinct points. Off by
	// default; unverified on the 2.10.3913 client.
	MissileHardpointOffsets bool `json:"missileHardpointOffsets"`

	// LogFile is where the server log is written (default "server.log");
	// CrashLogFile receives fatal runtime reports (default "server.crash.log").
	// Both are truncated at start unless LogAppend is set.
	LogFile      string `json:"logFile"`
	CrashLogFile string `json:"crashLogFile"`
	LogAppend    bool   `json:"logAppend"`

	// WriteTimeoutSec bounds one socket write to a client before the
	// connection is dropped (default 15). HandshakeTimeoutSec is how long a
	// new connection has to finish version exchange and login (default 30).
	WriteTimeoutSec     int `json:"writeTimeoutSec"`
	HandshakeTimeoutSec int `json:"handshakeTimeoutSec"`
	// UnauthIdleKickSec drops a connection that has not logged in after this
	// many seconds (default 120).
	UnauthIdleKickSec int `json:"unauthIdleKickSec"`

	// ShutdownWaitSec is how long a clean stop waits for in-flight tick work
	// before the final flush (default 20); ShutdownFlushSec bounds that flush
	// (default 60).
	ShutdownWaitSec  int `json:"shutdownWaitSec"`
	ShutdownFlushSec int `json:"shutdownFlushSec"`

	// MemoryLimitMB is a soft limit on the Go heap in megabytes (debug.SetMemoryLimit).
	// Near it the garbage collector runs harder and returns memory to the OS, so short
	// spikes do not leave the process holding the peak forever. 0 = no limit (the Go
	// default). A GOMEMLIMIT environment variable takes precedence.
	MemoryLimitMB int `json:"memoryLimitMB"`
}

// DatabaseConfig holds MariaDB connection parameters.
type DatabaseConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DB       string `json:"db"`

	// Pool and timeouts. Omitted values keep the defaults: 96 open, idle =
	// open, 30 min lifetime (keep it under MySQL wait_timeout), 10 min idle,
	// 10 s dial, 60 s per-query read/write.
	MaxOpenConns           int `json:"maxOpenConns"`
	MaxIdleConns           int `json:"maxIdleConns"`
	ConnMaxLifetimeMinutes int `json:"connMaxLifetimeMinutes"`
	ConnMaxIdleMinutes     int `json:"connMaxIdleMinutes"`
	DialTimeoutSec         int `json:"dialTimeoutSec"`
	ReadTimeoutSec         int `json:"readTimeoutSec"`
	WriteTimeoutSec        int `json:"writeTimeoutSec"`
}

// DSN returns the go-sql-driver/mysql connection string for database/sql. It sets
// interpolateParams so a parametrised query is one round trip instead of prepare, execute and
// close; every call site passes its arguments as placeholders, which keeps that safe. The
// read, write and dial timeouts bound a stalled query, and the collation is fixed.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s"+
		"?parseTime=true&interpolateParams=true&collation=utf8mb3_general_ci"+
		"&timeout=%ds&readTimeout=%ds&writeTimeout=%ds",
		d.User, d.Password, d.Host, d.Port, d.DB,
		orDefault(d.DialTimeoutSec, 10), orDefault(d.ReadTimeoutSec, 60), orDefault(d.WriteTimeoutSec, 60))
}

// orDefault answers v, or def when v is zero or negative.
func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// SettingDefaults converts the "settings" block to the string form server_settings stores:
// numbers and strings as written, booleans as 1/0, number arrays comma-separated. The result
// goes to db.SetConfigSettingDefaults.
func (c *Config) SettingDefaults() (map[string]string, error) {
	out := make(map[string]string, len(c.Settings))
	for k, raw := range c.Settings {
		if len(k) > 64 {
			return nil, fmt.Errorf("settings.%s: key longer than 64 characters", k)
		}
		v, err := settingString(raw)
		if err != nil {
			return nil, fmt.Errorf("settings.%s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

func settingString(raw json.RawMessage) (string, error) {
	var x any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&x); err != nil {
		return "", err
	}
	var s string
	switch v := x.(type) {
	case json.Number:
		s = v.String()
	case string:
		s = v
	case bool:
		s = "0"
		if v {
			s = "1"
		}
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			n, ok := e.(json.Number)
			if !ok {
				return "", fmt.Errorf("arrays may only hold numbers")
			}
			parts[i] = n.String()
		}
		s = strings.Join(parts, ",")
	default:
		return "", fmt.Errorf("must be a number, string, boolean or number array")
	}
	if len(s) > 255 {
		return "", fmt.Errorf("value longer than 255 characters")
	}
	return s, nil
}

// Load reads and parses the JSON file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %q: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	// Pre-fill rate defaults so fields absent from the JSON stay at 1.0
	// while an explicit 0 in the file is preserved.
	cfg.Rates = DefaultRates()
	cfg.NPCMarket = DefaultNPCMarket()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(stripJSONComments(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// stripJSONComments blanks out // line comments and /* */ block comments so config.json can
// carry notes. Text inside a string is left alone, so a value like "http://host" survives.
// Each comment byte becomes a space and newlines stay, so parse errors keep their positions.
func stripJSONComments(in []byte) []byte {
	out := make([]byte, len(in))
	copy(out, in)
	inStr := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inStr {
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				if out[i] != '\r' {
					out[i] = ' '
				}
			}
			i--
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i += 2
			for ; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' && out[i] != '\r' {
					out[i] = ' '
				}
			}
		}
	}
	return out
}

func (c *Config) validate() error {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:26000"
	}
	if c.Database.Host == "" {
		return fmt.Errorf("database.host is required")
	}
	if c.Database.Port == 0 {
		c.Database.Port = 3306
	}
	if c.Database.User == "" {
		return fmt.Errorf("database.user is required")
	}
	if c.Database.DB == "" {
		return fmt.Errorf("database.db is required")
	}
	if c.Authentication.Role == 0 {
		// Default 3 = ROLE_LOGIN|ROLE_PLAYER. A larger value such as 63 carries GM bits and would
		// make every auto-created account a GM.
		c.Authentication.Role = 3
	}
	if c.Character.StartBalance == 0 {
		c.Character.StartBalance = 100000.0
	}
	// Speed-type rates divide or scale times; zero or negative would stall training or divide by
	// zero, so clamp back to stock.
	if c.Rates.SkillTraining <= 0 {
		c.Rates.SkillTraining = 1.0
	}
	if c.Rates.IndustrySpeed <= 0 {
		c.Rates.IndustrySpeed = 1.0
	}
	if c.Rates.ProductionSpeed <= 0 {
		c.Rates.ProductionSpeed = 1.0
	}
	if c.Rates.ResearchSpeed <= 0 {
		c.Rates.ResearchSpeed = 1.0
	}
	// Multiplier-type rates may legitimately be 0 (disable), but never negative.
	if c.Rates.NPCBounty < 0 {
		c.Rates.NPCBounty = 0
	}
	if c.Rates.MiningYield < 0 {
		c.Rates.MiningYield = 0
	}
	if c.Rates.LootDrop < 0 {
		c.Rates.LootDrop = 0
	}
	if c.Rates.PlayerDamage < 0 {
		c.Rates.PlayerDamage = 0
	}
	if c.Rates.NPCDamage < 0 {
		c.Rates.NPCDamage = 0
	}
	if c.Rates.ResearchPoints < 0 {
		c.Rates.ResearchPoints = 0
	}
	if c.Rates.MissionReward < 0 {
		c.Rates.MissionReward = 0
	}
	if c.Rates.LoyaltyPoints < 0 {
		c.Rates.LoyaltyPoints = 0
	}
	if c.Rates.StandingGain < 0 {
		c.Rates.StandingGain = 0
	}
	if c.Rates.RatStandingLoss < 0 {
		c.Rates.RatStandingLoss = 0
	}
	if c.Server.LogFile == "" {
		c.Server.LogFile = "server.log"
	}
	if c.Server.CrashLogFile == "" {
		c.Server.CrashLogFile = "server.crash.log"
	}
	c.Server.WriteTimeoutSec = orDefault(c.Server.WriteTimeoutSec, 15)
	c.Server.HandshakeTimeoutSec = orDefault(c.Server.HandshakeTimeoutSec, 30)
	c.Server.UnauthIdleKickSec = orDefault(c.Server.UnauthIdleKickSec, 120)
	c.Server.ShutdownWaitSec = orDefault(c.Server.ShutdownWaitSec, 20)
	c.Server.ShutdownFlushSec = orDefault(c.Server.ShutdownFlushSec, 60)
	c.Database.MaxOpenConns = orDefault(c.Database.MaxOpenConns, 96)
	c.Database.MaxIdleConns = orDefault(c.Database.MaxIdleConns, c.Database.MaxOpenConns)
	if c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		c.Database.MaxIdleConns = c.Database.MaxOpenConns
	}
	c.Database.ConnMaxLifetimeMinutes = orDefault(c.Database.ConnMaxLifetimeMinutes, 30)
	c.Database.ConnMaxIdleMinutes = orDefault(c.Database.ConnMaxIdleMinutes, 10)
	c.ESP.SessionDays = orDefault(c.ESP.SessionDays, 30)
	if c.ESP.Pprof == nil {
		on := true
		c.ESP.Pprof = &on
	}
	if _, err := c.SettingDefaults(); err != nil {
		return err
	}
	if c.NPCMarket.TickMinutes <= 0 {
		c.NPCMarket.TickMinutes = 60
	}
	if c.ESP.Listen == "" {
		c.ESP.Listen = "127.0.0.1:26080"
	}
	if c.ESP.MinRole == 0 {
		c.ESP.MinRole = 2
	}
	if c.RPCCoveragePath == "" {
		c.RPCCoveragePath = "data/rpc-coverage.json"
	}
	if c.Agents.LocatorDelayMinutes <= 0 {
		c.Agents.LocatorDelayMinutes = 3
	}
	if c.JumpClone.MinStanding == nil {
		v := 8.0
		c.JumpClone.MinStanding = &v
	} else if *c.JumpClone.MinStanding > 10 {
		*c.JumpClone.MinStanding = 10
	}
	return nil
}
