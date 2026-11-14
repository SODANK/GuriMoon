package server

import "gurimoon/marshal"

// ParseRowsetArg decodes a client-marshalled util.Rowset instance into its header
// names and line values. The instance state is the object's __dict__ (a PyDict with
// "header" and "lines"); a rowset the client edited in place and sent back
// (posMgr.SetStarbasePermissions, corp title/role writes) has this shape.
func ParseRowsetArg(rep marshal.PyRep) (header []string, lines [][]marshal.PyRep, ok bool) {
	obj, isObj := rep.(marshal.PyObject)
	if !isObj {
		return nil, nil, false
	}
	dict, isDict := obj.Args.(marshal.PyDict)
	if !isDict {
		return nil, nil, false
	}
	for _, e := range dict.Items {
		key, isStr := e.Key.(marshal.PyString)
		if !isStr {
			continue
		}
		switch key.Value {
		case "header":
			if l, isList := e.Value.(marshal.PyList); isList {
				for _, h := range l.Items {
					if s, isS := h.(marshal.PyString); isS {
						header = append(header, s.Value)
					}
				}
			}
		case "lines":
			if l, isList := e.Value.(marshal.PyList); isList {
				for _, ln := range l.Items {
					switch row := ln.(type) {
					case marshal.PyList:
						lines = append(lines, row.Items)
					case marshal.PyTuple:
						lines = append(lines, row.Items)
					}
				}
			}
		}
	}
	return header, lines, len(header) > 0
}
