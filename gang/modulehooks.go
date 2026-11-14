package gang

// ModuleTagged is set by a module to hear every shared overview tag change (the client's
// Tag menu, bound.GangTagTarget). setterCharID is who tagged, itemID the tagged ball, and
// tag an int64 (0-9), a string (a letter) or nil (cleared). Nil: nobody hears.
//
// It is called on the tagger's connection goroutine after the members were sent the new
// state, so it must not block.
var ModuleTagged func(gangID, setterCharID, itemID int64, tag any)
