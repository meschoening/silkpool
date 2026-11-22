// Package roleaccount reads and makes the Mac's role accounts through
// dscl, without cgo. A slot account on macOS is a role account with a
// primary group of its own (ADR 0007), and this package holds the one
// way Silkpool makes one, finds it again and keeps it as it made it:
// each record's first write is a GeneratedUID of Silkpool's own, read
// straight back, and nothing makes a record an identity until Silkpool
// has shown that record is the one it made.
//
// The package runs anywhere a Directory can. On the Mac, Local reaches
// the real directory through /usr/bin/dscl; tests give it one of their
// own.
package roleaccount

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// A Record is a directory record as dscl prints it: each attribute's
// values, by the attribute's standard name. A native attribute keeps its
// dsAttrTypeNative: prefix.
type Record map[string][]string

// One returns the attribute's value, and false unless it has exactly
// one.
func (r Record) One(attr string) (string, bool) {
	if v := r[attr]; len(v) == 1 {
		return v[0], true
	}
	return "", false
}

// Is reports whether the attribute holds exactly the one value v.
func (r Record) Is(attr, v string) bool { return slices.Equal(r[attr], []string{v}) }

// parseRecords reads what dscl -plist prints: one record, as a dict of
// attribute names each with an array of values, or a list of them, as
// -readall prints. Anything else it doesn't know fails, so that a
// reading cut short or changed can't pass for fewer attributes.
func parseRecords(b []byte) ([]Record, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	var (
		records []Record
		path    []string
		key     string
		text    strings.Builder
		inPlist bool
	)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			path = append(path, tok.Name.Local)
			text.Reset()
			switch at := strings.Join(path, "/"); at {
			case "plist", "plist/array":
				inPlist = true
			case "plist/dict", "plist/array/dict":
				records = append(records, Record{})
			case "plist/dict/key", "plist/array/dict/key":
			case "plist/dict/array", "plist/array/dict/array":
				if key == "" {
					return nil, errors.New("an attribute's values without its name")
				}
				if _, ok := records[len(records)-1][key]; ok {
					return nil, fmt.Errorf("the attribute %s twice", key)
				}
				records[len(records)-1][key] = []string{}
			case "plist/dict/array/string", "plist/array/dict/array/string",
				"plist/dict/array/data", "plist/array/dict/array/data":
			default:
				return nil, fmt.Errorf("an unexpected <%s> in %s", tok.Name.Local, strings.Join(path[:len(path)-1], "/"))
			}
		case xml.EndElement:
			r := func() Record { return records[len(records)-1] }
			switch strings.Join(path, "/") {
			case "plist/dict/key", "plist/array/dict/key":
				key = strings.TrimPrefix(text.String(), "dsAttrTypeStandard:")
			case "plist/dict/array", "plist/array/dict/array":
				key = ""
			case "plist/dict/array/string", "plist/array/dict/array/string":
				r()[key] = append(r()[key], text.String())
			case "plist/dict/array/data", "plist/array/dict/array/data":
				// A binary value, such as a password's hash, as base64. It
				// counts as a value; what it holds doesn't matter here.
				raw := strings.Join(strings.Fields(text.String()), "")
				if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
					return nil, fmt.Errorf("the attribute %s holds data that isn't base64", key)
				}
				r()[key] = append(r()[key], "data:"+raw)
			case "plist/dict", "plist/array/dict":
				if key != "" {
					return nil, fmt.Errorf("the attribute %s without its values", key)
				}
				if len(r()) == 0 {
					return nil, errors.New("a record without attributes")
				}
			}
			path = path[:len(path)-1]
		case xml.CharData:
			text.Write(tok)
		}
	}
	if !inPlist && len(records) == 0 {
		return nil, errors.New("no plist")
	}
	return records, nil
}

// parseRecord reads what dscl -plist -read prints: exactly one record.
func parseRecord(b []byte) (Record, error) {
	rs, err := parseRecords(b)
	if err != nil {
		return nil, err
	}
	if len(rs) != 1 {
		return nil, fmt.Errorf("%d records, not one", len(rs))
	}
	return rs[0], nil
}
