package packp

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/format/pktline"
)

// A AdvRefsDecoder reads and decodes AdvRef values from an input stream.
type AdvRefsDecoder struct {
	s     *pktline.Scanner // a pkt-line scanner from the input stream
	line  []byte           // current pkt-line contents, use parser.nextLine() to make it advance
	nLine int              // current pkt-line number for debugging, begins at 1
	hash  plumbing.Hash    // last hash read
	err   error            // sticky error, use the parser.error() method to fill this out
	data  *AdvRefs         // parsed data is stored here
}

// ErrEmpty is returned by Decode when there was no advertised-message at all
var ErrEmpty = errors.New("empty advertised-ref message")

// NewAdvRefsDecoder returns a new decoder that reads from r.
//
// Will not read more data from r than necessary.
func NewAdvRefsDecoder(r io.Reader) *AdvRefsDecoder {
	return &AdvRefsDecoder{
		s: pktline.NewScanner(r),
	}
}

// Decode reads the next advertised-refs message form its input and
// stores it in the value pointed to by v.
func (d *AdvRefsDecoder) Decode(v *AdvRefs) error {
	d.data = v

	for state := decodePrefix; state != nil; {
		state = state(d)
	}

	return d.err
}

type decoderStateFn func(*AdvRefsDecoder) decoderStateFn

// fills out the parser stiky error
func (d *AdvRefsDecoder) error(format string, a ...interface{}) {
	d.err = fmt.Errorf("pkt-line %d: %s", d.nLine,
		fmt.Sprintf(format, a...))
}

// Reads a new pkt-line from the scanner, makes its payload available as
// p.line and increments p.nLine.  A successful invocation returns true,
// otherwise, false is returned and the sticky error is filled out
// accordingly.  Trims eols at the end of the payloads.
func (d *AdvRefsDecoder) nextLine() bool {
	d.nLine++

	if !d.s.Scan() {
		if d.err = d.s.Err(); d.err != nil {
			return false
		}

		if d.nLine == 1 {
			d.err = ErrEmpty
			return false
		}

		d.error("EOF")
		return false
	}

	d.line = d.s.Bytes()
	d.line = bytes.TrimSuffix(d.line, eol)

	return true
}

// The HTTP smart prefix is often followed by a flush-pkt.
func decodePrefix(d *AdvRefsDecoder) decoderStateFn {
	if ok := d.nextLine(); !ok {
		return nil
	}

	// If the repository is empty, we receive a flush here (SSH).
	if isFlush(d.line) {
		d.err = ErrEmpty
		return nil
	}

	if isPrefix(d.line) {
		tmp := make([]byte, len(d.line))
		copy(tmp, d.line)
		d.data.Prefix = append(d.data.Prefix, tmp)
		if ok := d.nextLine(); !ok {
			return nil
		}
	}

	if isFlush(d.line) {
		d.data.Prefix = append(d.data.Prefix, pktline.Flush)
		if ok := d.nextLine(); !ok {
			return nil
		}
	}

	return decodeFirstHash
}

func isPrefix(payload []byte) bool {
	return payload[0] == '#'
}

func isFlush(payload []byte) bool {
	return len(payload) == 0
}

// If the first hash is zero, then a no-refs is comming. Otherwise, a
// list-of-refs is comming, and the hash will be followed by the first
// advertised ref.
func decodeFirstHash(p *AdvRefsDecoder) decoderStateFn {
	// If the repository is empty, we receive a flush here (HTTP).
	if isFlush(p.line) {
		p.err = ErrEmpty
		return nil
	}

	if len(p.line) < hashSize {
		p.error("cannot read hash, pkt-line too short")
		return nil
	}

	if _, err := hex.Decode(p.hash[:], p.line[:hashSize]); err != nil {
		p.error("invalid hash text: %s", err)
		return nil
	}

	p.line = p.line[hashSize:]

	if p.hash.IsZero() {
		return decodeSkipNoRefs
	}

	return decodeFirstRef
}

// Skips SP "capabilities^{}" NUL
func decodeSkipNoRefs(p *AdvRefsDecoder) decoderStateFn {
	if len(p.line) < len(noHeadMark) {
		p.error("too short zero-id ref")
		return nil
	}

	if !bytes.HasPrefix(p.line, noHeadMark) {
		p.error("malformed zero-id ref")
		return nil
	}

	p.line = p.line[len(noHeadMark):]

	return decodeCaps
}

// decode the refname, expectes SP refname NULL
func decodeFirstRef(l *AdvRefsDecoder) decoderStateFn {
	if len(l.line) < 3 {
		l.error("line too short after hash")
		return nil
	}

	if !bytes.HasPrefix(l.line, sp) {
		l.error("no space after hash")
		return nil
	}
	l.line = l.line[1:]

	chunks := bytes.SplitN(l.line, null, 2)
	if len(chunks) < 2 {
		l.error("NULL not found")
		return nil
	}
	ref := chunks[0]
	l.line = chunks[1]

	if bytes.Equal(ref, []byte(head)) {
		l.data.Head = &l.hash
	} else {
		l.data.References[string(ref)] = l.hash
	}

	return decodeCaps
}

func decodeCaps(p *AdvRefsDecoder) decoderStateFn {
	if len(p.line) == 0 {
		return decodeOtherRefs
	}

	for _, c := range bytes.Split(p.line, sp) {
		name, values := readCapability(c)
		p.data.Capabilities.Add(name, values...)
	}

	return decodeOtherRefs
}

// The refs are either tips (obj-id SP refname) or a peeled (obj-id SP refname^{}).
// If there are no refs, then there might be a shallow or flush-ptk.
func decodeOtherRefs(p *AdvRefsDecoder) decoderStateFn {
	if ok := p.nextLine(); !ok {
		return nil
	}

	if bytes.HasPrefix(p.line, shallow) {
		return decodeShallow
	}

	if len(p.line) == 0 {
		return nil
	}

	saveTo := p.data.References
	if bytes.HasSuffix(p.line, peeled) {
		p.line = bytes.TrimSuffix(p.line, peeled)
		saveTo = p.data.Peeled
	}

	ref, hash, err := readRef(p.line)
	if err != nil {
		p.error("%s", err)
		return nil
	}
	saveTo[ref] = hash

	return decodeOtherRefs
}

// Reads a ref-name
func readRef(data []byte) (string, plumbing.Hash, error) {
	chunks := bytes.Split(data, sp)
	switch {
	case len(chunks) == 1:
		return "", plumbing.ZeroHash, fmt.Errorf("malformed ref data: no space was found")
	case len(chunks) > 2:
		return "", plumbing.ZeroHash, fmt.Errorf("malformed ref data: more than one space found")
	default:
		return string(chunks[1]), plumbing.NewHash(string(chunks[0])), nil
	}
}

// Keeps reading shallows until a flush-pkt is found
func decodeShallow(p *AdvRefsDecoder) decoderStateFn {
	if !bytes.HasPrefix(p.line, shallow) {
		p.error("malformed shallow prefix, found %q... instead", p.line[:len(shallow)])
		return nil
	}
	p.line = bytes.TrimPrefix(p.line, shallow)

	if len(p.line) != hashSize {
		p.error(fmt.Sprintf(
			"malformed shallow hash: wrong length, expected 40 bytes, read %d bytes",
			len(p.line)))
		return nil
	}

	text := p.line[:hashSize]
	var h plumbing.Hash
	if _, err := hex.Decode(h[:], text); err != nil {
		p.error("invalid hash text: %s", err)
		return nil
	}

	p.data.Shallows = append(p.data.Shallows, h)

	if ok := p.nextLine(); !ok {
		return nil
	}

	if len(p.line) == 0 {
		return nil // succesfull parse of the advertised-refs message
	}

	return decodeShallow
}