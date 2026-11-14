package alert

import (
	"bytes"
	"compress/zlib"
	"io"
	"strings"
	"unicode/utf8"

	"gurimoon/marshal"
	"gurimoon/rpccov"
	"gurimoon/server"
)

// captureTrace files a client-side Python traceback into the RPC coverage ledger
// (ESP /coverage), attributed to the last calls on this connection. Args
// (alert.py __SendStackTraceAlert): (stackID=(adler32Hash, culpritText), stackTrace,
// mode). Every field is best-effort and nothing here may fail the call.
func captureTrace(addr string, cs *server.CallStream) {
	if cs == nil {
		return
	}
	var (
		hash    uint64
		culprit string
	)
	if len(cs.ArgTuple.Items) > 0 {
		if stackID, ok := cs.ArgTuple.Items[0].(marshal.PyTuple); ok {
			for _, item := range stackID.Items {
				switch v := item.(type) {
				case marshal.PyInt:
					if hash == 0 {
						hash = v.Value
					}
				case marshal.PyString:
					if culprit == "" {
						culprit = v.Value
					}
				}
			}
		} else {
			culprit = firstLine(flattenText(cs.ArgTuple.Items[0]))
		}
	}

	// The traceback body may arrive as a string or as a buffer of raw bytes,
	// and which one varies with how the client throttled the report, so pull
	// text out of every remaining argument rather than indexing a fixed slot.
	var parts []string
	for i, item := range cs.ArgTuple.Items {
		if i == 0 {
			continue
		}
		if s := flattenText(item); s != "" {
			parts = append(parts, s)
		}
	}
	if cs.ArgDict != nil {
		for _, entry := range cs.ArgDict.Items {
			if s := flattenText(entry.Value); s != "" {
				parts = append(parts, flattenText(entry.Key)+": "+s)
			}
		}
	}

	rpccov.RecordTrace(addr, hash, culprit, strings.Join(parts, "\n"))
}

// traceText pulls the same culprit line and decoded traceback body captureTrace
// files into rpccov, so the handlers can put readable text in server.log
// instead of the raw PyBuffer byte array the client actually sent.
func traceText(cs *server.CallStream) (culprit, body string) {
	if cs == nil {
		return "", ""
	}
	var parts []string
	for i, item := range cs.ArgTuple.Items {
		if i == 0 {
			if stackID, ok := item.(marshal.PyTuple); ok {
				for _, sub := range stackID.Items {
					if s, ok := sub.(marshal.PyString); ok && culprit == "" {
						culprit = firstLine(s.Value)
					}
				}
			} else {
				culprit = firstLine(flattenText(item))
			}
			continue
		}
		if s := flattenText(item); strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return culprit, strings.Join(parts, "\n")
}

// logTrace writes a decoded client traceback to the log, one line per line, so
// a grep for the failing module actually hits.
func logTrace(addr string, cs *server.CallStream, logf func(format string, v ...interface{})) {
	culprit, body := traceText(cs)
	if culprit != "" {
		logf("[%s]   culprit: %s", addr, culprit)
	}
	for _, line := range strings.Split(body, "\n") {
		if t := strings.TrimRight(line, " \t\r"); strings.TrimSpace(t) != "" {
			logf("[%s]   | %s", addr, t)
		}
	}
}

// flattenText walks a PyRep and concatenates every human-readable string it
// finds.  Buffers are decoded as UTF-8, transparently inflating the zlib-framed
// ones the client sometimes sends for long tracebacks.
func flattenText(rep marshal.PyRep) string {
	switch v := rep.(type) {
	case marshal.PyString:
		return v.Value
	case marshal.PyBuffer:
		return decodeBuffer(v.Data)
	case marshal.PyTuple:
		return joinItems(v.Items)
	case marshal.PyList:
		return joinItems(v.Items)
	case marshal.PyDict:
		var parts []string
		for _, e := range v.Items {
			if s := flattenText(e.Value); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case marshal.PyObject:
		return flattenText(v.Args)
	case marshal.PySubStruct:
		return flattenText(v.Value)
	case *marshal.PySubStream:
		if v.Decoded != nil {
			return flattenText(v.Decoded)
		}
	}
	return ""
}

func joinItems(items []marshal.PyRep) string {
	var parts []string
	for _, it := range items {
		if s := flattenText(it); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// decodeBuffer renders a raw byte buffer as text, inflating it first when it
// carries a zlib header.  Binary payloads that are not text are dropped rather
// than pasted into the report as mojibake.
func decodeBuffer(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if len(data) > 2 && data[0] == 0x78 {
		if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
			if out, err := io.ReadAll(io.LimitReader(zr, 1<<20)); err == nil && utf8.Valid(out) {
				zr.Close()
				return string(out)
			}
			zr.Close()
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	// Latin-1 fallback: client tracebacks occasionally carry non-UTF-8 bytes
	// from item or character names.
	var sb strings.Builder
	for _, b := range data {
		sb.WriteRune(rune(b))
	}
	return sb.String()
}

// firstLine returns the first non-empty line of s, used as a short label.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
