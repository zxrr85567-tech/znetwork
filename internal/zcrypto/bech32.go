package zcrypto

// Bech32 (BIP-173) encoding for Z Network addresses.
//
// Z Network uses the original Bech32 checksum constant (1), not Bech32m, because
// it only encodes fixed 20-byte addresses and never SegWit witness programs.
// The implementation is validated against the BIP-173 test vectors in
// bech32_test.go; do not "improve" it without re-running those vectors.
//
// Canonical form rules enforced by Decode:
//   - all-lowercase or all-uppercase, never mixed
//   - total length <= 90 characters
//   - HRP in [1, 83]
//   - separator '1' present, data part >= 6 characters
//   - checksum must validate
//   - decoded payload must be exactly 20 bytes

import (
	"fmt"
	"strings"
)

const (
	bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	// AddressHRP is the human-readable part for account and contract addresses.
	AddressHRP = "z"
	// MaxBech32Len is the BIP-173 limit on total string length.
	MaxBech32Len = 90
)

var bech32Gen = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func bech32Polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 == 1 {
				chk ^= bech32Gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// convertBits regroups bits from a group size of fromBits to toBits.
func convertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint
	maxv := uint32(1)<<toBits - 1
	out := make([]byte, 0, len(data)*int(fromBits)/int(toBits)+1)
	for _, value := range data {
		if uint32(value)>>fromBits != 0 {
			return nil, fmt.Errorf("zcrypto: bech32 invalid data value %d", value)
		}
		acc = acc<<fromBits | uint32(value)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			out = append(out, byte(acc>>bits)&byte(maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(toBits-bits))&byte(maxv))
		}
	} else if bits >= fromBits || byte(acc<<(toBits-bits))&byte(maxv) != 0 {
		return nil, fmt.Errorf("zcrypto: bech32 invalid padding")
	}
	return out, nil
}

// Bech32 encodes 20 address bytes with HRP "z".
func Bech32(addr []byte) string {
	s, err := encodeBech32(AddressHRP, addr)
	if err != nil {
		// Unreachable for a 20-byte input; kept for signature symmetry.
		return ""
	}
	return s
}

func encodeBech32(hrp string, payload []byte) (string, error) {
	if hrp == "" || len(hrp) > 83 {
		return "", fmt.Errorf("zcrypto: invalid bech32 HRP length")
	}
	data, err := convertBits(payload, 8, 5, true)
	if err != nil {
		return "", err
	}
	if len(hrp)+1+len(data)+6 > MaxBech32Len {
		return "", fmt.Errorf("zcrypto: bech32 string too long")
	}
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	pm := bech32Polymod(values) ^ 1
	checksum := make([]byte, 6)
	for i := 0; i < 6; i++ {
		checksum[i] = byte(pm>>uint(5*(5-i))) & 31
	}
	var sb strings.Builder
	sb.Grow(len(hrp) + 1 + len(data) + 6)
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range append(data, checksum...) {
		sb.WriteByte(bech32Charset[v])
	}
	return sb.String(), nil
}

// DecodeBech32 validates a Bech32 string and returns its HRP and payload bytes.
func DecodeBech32(s string) (string, []byte, error) {
	if len(s) < 8 || len(s) > MaxBech32Len {
		return "", nil, fmt.Errorf("%w: length", ErrBadAddress)
	}
	hasLower := strings.ToLower(s) == s
	hasUpper := strings.ToUpper(s) == s
	if !hasLower && !hasUpper {
		return "", nil, fmt.Errorf("%w: mixed case", ErrBadAddress)
	}
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, fmt.Errorf("%w: separator position", ErrBadAddress)
	}
	hrp := s[:pos]
	dataPart := s[pos+1:]
	data := make([]byte, 0, len(dataPart))
	for i := 0; i < len(dataPart); i++ {
		idx := strings.IndexByte(bech32Charset, dataPart[i])
		if idx < 0 {
			return "", nil, fmt.Errorf("%w: invalid character %q", ErrBadAddress, dataPart[i])
		}
		data = append(data, byte(idx))
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != 1 {
		return "", nil, fmt.Errorf("%w: checksum", ErrBadAddress)
	}
	payload, err := convertBits(data[:len(data)-6], 5, 8, false)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrBadAddress, err)
	}
	return hrp, payload, nil
}

// Bech32 returns the canonical Bech32 representation of the address.
func (a Address) Bech32() string { return Bech32(a[:]) }

// ParseAddressText accepts canonical Bech32 ("z1...") or 40-char hex. Any other
// form (wrong HRP, bad checksum, wrong length) is rejected.
func ParseAddressText(s string) (Address, error) {
	var a Address
	if s == "" {
		return a, ErrBadAddress
	}
	if len(s) == 40 && !strings.ContainsRune(s, '1') {
		return ParseAddressHex(s)
	}
	hrp, payload, err := DecodeBech32(s)
	if err != nil {
		return a, err
	}
	if hrp != AddressHRP {
		return a, fmt.Errorf("%w: unexpected HRP %q", ErrBadAddress, hrp)
	}
	if len(payload) != 20 {
		return a, fmt.Errorf("%w: payload must be 20 bytes", ErrBadAddress)
	}
	copy(a[:], payload)
	return a, nil
}
