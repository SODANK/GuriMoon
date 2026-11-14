package esp

// LoadedSystemCount, set by the game server, reports how many solar systems
// have players in them; a system with players keeps its field until it empties.
// nil when no game server is attached.
var LoadedSystemCount func() int
