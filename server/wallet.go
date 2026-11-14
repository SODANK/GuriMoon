package server

import (
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
)

// The neocom's ISK tooltip is not a fresh read: wallet.py's self.wealth is set at
// login in Run() and afterwards only from OnAccountChange. Every path that moves ISK
// must end in one of these pushes. ownerID is the wallet owner as wallet.py tests it
// (eve.session.charid for the personal wallet, corpid for the corp one), which is not
// always the character being notified.

// CashBalanceArgs builds the OnAccountChange payload: (accountKey, ownerID,
// balance).
func CashBalanceArgs(ownerID int64, balance float64) marshal.PyTuple {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(ownerID)},
		marshal.PyReal{Value: balance},
	}}
}

// PushCashBalance repaints the wallet of the connection's own client.
func PushCashBalance(c *evenet.Conn, accountID, ownerID int64, balance float64) {
	if c == nil {
		return
	}
	_ = SendNotification(c, accountID, "OnAccountChange", "clientID", CashBalanceArgs(ownerID, balance))
}

// NotifyCashBalance repaints another character's wallet, if they are online. charID is
// the recipient; ownerID is whose wallet moved (the corp, for a corp wallet).
func NotifyCashBalance(sessReg *esp.SessionRegistry, charID, ownerID int64, balance float64) {
	if sessReg == nil {
		return
	}
	NotifyCharacter(sessReg, charID, "OnAccountChange", "clientID", CashBalanceArgs(ownerID, balance))
}
