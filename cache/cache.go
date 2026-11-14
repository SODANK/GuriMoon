// Package cache loads EVEmu .cache files and builds the PyRep structures for
// AuthenticationRsp.cachables and objectCaching.GetCachableObject.
//
// File layout, little-endian: uint64 FILETIME timestamp, uint32 version (CRC32 of the
// data), uint32 length, uint32 magic 0xFF886622, then length bytes of marshal stream,
// possibly deflate-compressed. The file name is <objectID>.cache.
package cache

import (
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gurimoon/marshal"
)

const (
	fileMagic    uint32 = 0xFF886622
	nodeID       uint64 = 333444 // HackCacheNodeID in EVEmu
	ssHeaderByte byte   = 0x7E   // first byte of an uncompressed stream
)

// Entry is a single loaded cache object.
type Entry struct {
	ObjectID  string // e.g. "config.BulkData.constants"
	Timestamp uint64 // Win32 FILETIME when generated
	Version   uint32 // CRC32 of Data
	Data      []byte // possibly deflate-compressed marshal bytes
}

// loadDir reads every *.cache file in dir into a map keyed by objectID.
func loadDir(dir string) (map[string]*Entry, error) {
	entries := make(map[string]*Entry)

	files, err := filepath.Glob(filepath.Join(dir, "*.cache"))
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		e, err := loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cache: load %q: %w", path, err)
		}
		entries[e.ObjectID] = e
	}
	return entries, nil
}

// cacheHeader is the 20-byte file header.
type cacheHeader struct {
	Timestamp uint64
	Version   uint32
	Length    uint32
	Magic     uint32
}

func loadFile(path string) (*Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hdr cacheHeader
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if hdr.Magic != fileMagic {
		return nil, fmt.Errorf("bad magic 0x%08X (want 0x%08X)", hdr.Magic, fileMagic)
	}

	data := make([]byte, hdr.Length)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, fmt.Errorf("read data (%d bytes): %w", hdr.Length, err)
	}

	// objectID is the file name without ".cache".
	base := filepath.Base(path)
	objectID := strings.TrimSuffix(base, ".cache")

	return &Entry{
		ObjectID:  objectID,
		Timestamp: hdr.Timestamp,
		Version:   hdr.Version,
		Data:      data,
	}, nil
}

// Hint builds the util.CachedObject value placed in AuthenticationRsp.cachables. The
// client compares its (timestamp, version) with its local copy and calls
// objectCaching.GetCachableObject only on a mismatch.
//
// Shape: util.CachedObject(objectID, nodeID, (timestamp, version)).
func Hint(e *Entry) marshal.PyRep {
	return marshal.PyObject{
		Type: "util.CachedObject",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: e.ObjectID},
			marshal.PyInt{Value: nodeID},
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: e.Timestamp},
				marshal.PyInt{Value: uint64(e.Version)},
			}},
		}},
	}
}

// BuildHints returns the AuthenticationRsp.cachables dict: objectID to Hint.
func BuildHints(entries map[string]*Entry) marshal.PyDict {
	d := marshal.PyDict{}
	for _, e := range entries {
		d.Items = append(d.Items, marshal.DictEntry{
			Key:   marshal.PyString{Value: e.ObjectID},
			Value: Hint(e),
		})
	}
	return d
}

// CachedMethodCallResult builds the objectCaching.CachedMethodCallResult returned by
// cached service methods (dogmaIM.GetAttributeTypes, standing2.GetNPCNPCStandings).
//
// Shape: ({"versionCheck": "run"}, Hint(e), None).
func CachedMethodCallResult(e *Entry) marshal.PyRep {
	return marshal.PyObject{
		Type: "objectCaching.CachedMethodCallResult",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "versionCheck"}, Value: marshal.PyString{Value: "run"}},
			}},
			Hint(e),
			marshal.PyNone{},
		}},
	}
}

// EncodeResponse builds the objectCaching.CachedObject returned by GetCachableObject.
//
// Shape: ((timestamp, version), None, nodeID, shared=1, PyBuffer(data), compressed,
// objectID). compressed is 0 when data starts with the 0x7E stream header, else 1.
func EncodeResponse(e *Entry) marshal.PyRep {
	compressed := uint64(1)
	if len(e.Data) > 0 && e.Data[0] == ssHeaderByte {
		compressed = 0
	}
	return marshal.PyObject{
		Type: "objectCaching.CachedObject",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: e.Timestamp},
				marshal.PyInt{Value: uint64(e.Version)},
			}},
			marshal.PyNone{},
			marshal.PyInt{Value: nodeID},
			marshal.PyInt{Value: 1}, // shared
			marshal.PyBuffer{Data: e.Data},
			marshal.PyInt{Value: compressed},
			marshal.PyString{Value: e.ObjectID},
		}},
	}
}

// MethodCallValue wraps result in a CachedMethodCallResult that carries the value
// inline, the form per-caller volatile data needs. objectCaching records a version for
// a method only when the server answers with this object; without it
// GetCachedMethodCallVersion stays 0 and marketsvc.RefreshOrderCache keeps its first
// answer for the whole session.
//
// Shape (__setstate__): (details, PyBuffer(marshalled), [timestamp, checksum]).
// versionCheck must be "always" for volatile data: "run" and the named ages let the
// client skip the call. sessionInfo names a session field added to the client's cache
// key ("regionid" for region-scoped data, "" for none).
func MethodCallValue(result marshal.PyRep, versionCheck, sessionInfo string) (marshal.PyRep, error) {
	data, err := marshal.Marshal(result)
	if err != nil {
		return nil, err
	}
	details := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "versionCheck"}, Value: marshal.PyString{Value: versionCheck}},
	}}
	if sessionInfo != "" {
		details.Items = append(details.Items, marshal.DictEntry{
			Key:   marshal.PyString{Value: "sessionInfo"},
			Value: marshal.PyString{Value: sessionInfo},
		})
	}
	// The version is a content checksum so equal bytes give equal versions. Masked to 31
	// bits: the client only compares it with itself.
	sum := uint64(adler32.Checksum(data)) & 0x7FFFFFFF
	return marshal.PyObject{
		Type: "objectCaching.CachedMethodCallResult",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			details,
			marshal.PyBuffer{Data: data},
			marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: win32TimeNow()},
				marshal.PyInt{Value: sum},
			}},
		}},
	}, nil
}
