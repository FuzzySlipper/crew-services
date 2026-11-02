package board

import (
	"crypto/rand"
	"strings"
	"time"
)

// crockford is Crockford's base32 alphabet: time-sortable and unambiguous.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns an ID unique across sites: the site's name, then 48 bits of
// milliseconds and 80 random bits in Crockford base32, so IDs from one site
// sort by creation time and IDs from different sites never collide.
func NewID(site string, now time.Time) string {
	var raw [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		raw[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		panic("board: no randomness: " + err.Error())
	}
	var b strings.Builder
	b.WriteString(site)
	b.WriteByte('-')
	// 128 bits as 26 base32 digits, most significant first.
	var acc uint64
	bits := 0
	out := make([]byte, 0, 26)
	// Two leading zero bits pad 128 to 130.
	bits = 2
	for _, by := range raw {
		acc = acc<<8 | uint64(by)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, crockford[(acc>>uint(bits))&31])
		}
	}
	b.Write(out)
	return b.String()
}
