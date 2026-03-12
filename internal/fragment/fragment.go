package fragment

import "encoding/binary"

// IsFragmentedIPv4 reports whether b starts with a fragmented IPv4 packet.
func IsFragmentedIPv4(b []byte) bool {
	if len(b) < 20 {
		return false
	}
	if b[0]>>4 != 4 {
		return false
	}
	ihl := int(b[0]&0x0F) * 4
	if ihl < 20 || len(b) < ihl {
		return false
	}
	totalLen := int(binary.BigEndian.Uint16(b[2:4]))
	if totalLen < ihl || totalLen > len(b) {
		return false
	}
	frag := binary.BigEndian.Uint16(b[6:8])
	moreFragments := (frag & 0x2000) != 0
	offset := frag & 0x1FFF
	return moreFragments || offset != 0
}
