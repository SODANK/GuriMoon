package lsc

// Module access to chat: inert until a module installs ModuleChat. Chat is not
// persisted, so an in-process hook is the only way to hear a line.
//
// Two client constraints apply. A line beginning with "/" is routed by lscchannel.py
// to the slash service and never reaches SendMessage. A dragged name is serialised
// as <url=showinfo:typeID//itemID>Name</url> only when the player picks a
// non-default option in the drop dialog; the default "Text only" yields a bare name.

// ChatMessage is one line, as seen by the server, with the identifiers a module
// is likely to key on already resolved.
type ChatMessage struct {
	// ChannelID is the internal channel ID: the number the Registry keys members by
	// and JoinBot/SpeakBot take.
	ChannelID int64

	// GangID is non-zero when this is fleet chat, decoded from the reserved band of
	// the internal ID (gangChannelBase is unexported).
	GangID int64

	// SystemID is non-zero when this is Local, taken from the solar-system ID band.
	// Corp and private channels are not decoded: their IDs share the default band
	// with static channels.
	SystemID int64

	SenderCharID int64
	SenderCorpID int64
	SenderName   string

	// Text is the raw line, markup and HTML entities intact.
	Text string
}

// ModuleChat is set by a module to hear every plain-text line the server
// broadcasts. Called on the sender's connection goroutine after the broadcast loop;
// it must return promptly. Nil means no module is listening.
var ModuleChat func(ChatMessage)

// GangChannelID is the internal channel ID of a fleet's chat, for a module that
// wants to JoinBot or SpeakBot into one.
func GangChannelID(gangID int64) int64 {
	if gangID <= 0 {
		return 0
	}
	return gangChannelBase + gangID
}

// newChatMessage builds the struct handed to ModuleChat.
func newChatMessage(chID, senderCharID, senderCorpID int64, name, text string) ChatMessage {
	m := ChatMessage{
		ChannelID:    chID,
		SenderCharID: senderCharID,
		SenderCorpID: senderCorpID,
		SenderName:   name,
		Text:         text,
	}
	if isGangChannel(chID) {
		m.GangID = chID - gangChannelBase
	} else if chID >= 30_000_000 && chID < 40_000_000 {
		m.SystemID = chID
	}
	return m
}
