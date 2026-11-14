package esp

// PetitionEventHook is set by the petitioner service at boot and called by the
// petition page. event is "message", "assign" or "close"; charID is the pilot
// to tell; text and messageID carry a message.
var PetitionEventHook func(event string, petitionID, charID int64, text string, messageID int64)

// PetitionAdminSenderID is the owner the client names on a reply written
// from the panel, which has no character of its own: CONCORD.
const PetitionAdminSenderID = 1000125
