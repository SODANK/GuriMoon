package db

import (
	"errors"

	"gurimoon/marshal"
)

// ClientMsgError is a refusal with a row in the client's message catalogue
// (config.BulkData.messages, sql/evemessages.sql). A wire handler raises Key with Dict so the
// pilot sees the client's own box; every other caller (ESP panel, module bridge, log) reads Text
// through Error().
type ClientMsgError struct {
	Key  string
	Dict map[string]marshal.PyRep
	Text string
}

func (e *ClientMsgError) Error() string { return e.Text }

// MsgErr builds a ClientMsgError. The key is the first string argument and the
// dict the second, so server/clientmessages_test.go checks every literal call
// against the catalogue the same way it checks clientMsgErr.
func MsgErr(key string, dict map[string]marshal.PyRep, text string) error {
	return &ClientMsgError{Key: key, Dict: dict, Text: text}
}

// AsClientMsg unwraps a ClientMsgError anywhere in err's chain.
func AsClientMsg(err error) (*ClientMsgError, bool) {
	var m *ClientMsgError
	if errors.As(err, &m) {
		return m, true
	}
	return nil, false
}
