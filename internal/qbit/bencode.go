package qbit

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// extractInfoHash returns the BitTorrent v1 info-hash (uppercase hex, 40 chars)
// for a .torrent file. It walks the top-level bencoded dict, finds the "info"
// key, then SHA1s the raw bytes of its value (NOT a re-encoded copy — the
// spec is explicit that the hash must be over the original byte range to
// stay round-trip stable). torrentName is best-effort: pulled from the info
// dict's "name" field, empty if absent.
func extractInfoHash(data []byte) (hash string, torrentName string, err error) {
	d := &bdecoder{buf: data}
	infoStart, infoEnd, name, err := d.findInfoSpan()
	if err != nil {
		return "", "", err
	}
	sum := sha1.Sum(data[infoStart:infoEnd])
	return hex.EncodeToString(sum[:]), name, nil
}

// bdecoder is a minimal walker for the subset of bencode we need: top-level
// dict with an "info" key. We never need to materialize the whole tree, just
// to locate the byte span of the info dict and read its "name" string.
type bdecoder struct {
	buf []byte
	pos int
}

func (d *bdecoder) peek() byte {
	if d.pos >= len(d.buf) {
		return 0
	}
	return d.buf[d.pos]
}

// findInfoSpan locates the byte span of the value of the top-level "info" key.
// Returns start (inclusive) and end (exclusive) offsets into d.buf.
func (d *bdecoder) findInfoSpan() (start, end int, name string, err error) {
	if d.peek() != 'd' {
		return 0, 0, "", errors.New("torrent: top-level not a dict")
	}
	d.pos++ // skip 'd'
	for d.peek() != 'e' && d.pos < len(d.buf) {
		key, err := d.readString()
		if err != nil {
			return 0, 0, "", fmt.Errorf("torrent: read key: %w", err)
		}
		valStart := d.pos
		if err := d.skipValue(); err != nil {
			return 0, 0, "", fmt.Errorf("torrent: skip value for key %q: %w", key, err)
		}
		valEnd := d.pos
		if key == "info" {
			// Walk the info dict to find its "name" field — best-effort, ignore
			// errors so we still return the hash when name parsing fails.
			name, _ = readDictString(d.buf[valStart:valEnd], "name")
			return valStart, valEnd, name, nil
		}
	}
	return 0, 0, "", errors.New("torrent: no info dict")
}

func (d *bdecoder) readString() (string, error) {
	// Format: <len>:<bytes>
	colon := -1
	for i := d.pos; i < len(d.buf); i++ {
		if d.buf[i] == ':' {
			colon = i
			break
		}
		if d.buf[i] < '0' || d.buf[i] > '9' {
			return "", fmt.Errorf("non-digit in string length at %d", i)
		}
	}
	if colon == -1 {
		return "", errors.New("unterminated string length")
	}
	n, err := strconv.Atoi(string(d.buf[d.pos:colon]))
	if err != nil {
		return "", err
	}
	if n < 0 || colon+1+n > len(d.buf) {
		return "", errors.New("string length out of range")
	}
	s := string(d.buf[colon+1 : colon+1+n])
	d.pos = colon + 1 + n
	return s, nil
}

func (d *bdecoder) skipValue() error {
	if d.pos >= len(d.buf) {
		return errors.New("unexpected EOF")
	}
	c := d.buf[d.pos]
	switch {
	case c == 'i':
		// i<int>e
		d.pos++
		for d.pos < len(d.buf) && d.buf[d.pos] != 'e' {
			d.pos++
		}
		if d.pos >= len(d.buf) {
			return errors.New("unterminated int")
		}
		d.pos++ // skip 'e'
		return nil
	case c == 'l':
		d.pos++
		for d.peek() != 'e' && d.pos < len(d.buf) {
			if err := d.skipValue(); err != nil {
				return err
			}
		}
		if d.pos >= len(d.buf) {
			return errors.New("unterminated list")
		}
		d.pos++ // skip 'e'
		return nil
	case c == 'd':
		d.pos++
		for d.peek() != 'e' && d.pos < len(d.buf) {
			if _, err := d.readString(); err != nil {
				return err
			}
			if err := d.skipValue(); err != nil {
				return err
			}
		}
		if d.pos >= len(d.buf) {
			return errors.New("unterminated dict")
		}
		d.pos++ // skip 'e'
		return nil
	case c >= '0' && c <= '9':
		_, err := d.readString()
		return err
	}
	return fmt.Errorf("unknown bencode token %q at %d", c, d.pos)
}

// readDictString takes the byte span of a dict (starting with 'd', ending past
// matching 'e') and returns the string value of key. Empty + nil on miss.
func readDictString(buf []byte, key string) (string, error) {
	d := &bdecoder{buf: buf}
	if d.peek() != 'd' {
		return "", errors.New("not a dict")
	}
	d.pos++
	for d.peek() != 'e' && d.pos < len(d.buf) {
		k, err := d.readString()
		if err != nil {
			return "", err
		}
		valStart := d.pos
		if err := d.skipValue(); err != nil {
			return "", err
		}
		if k == key {
			// Value should be a string for the keys we care about.
			vd := &bdecoder{buf: buf[valStart:d.pos]}
			return vd.readString()
		}
	}
	return "", nil
}
