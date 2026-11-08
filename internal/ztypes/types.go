// Package ztypes defines the Z Network protocol data model: transactions,
// blocks, receipts, state records, genesis and parameters.
//
// This package is the single source of truth for every structure that is hashed,
// signed, serialized or transmitted. Nothing outside this package may define its
// own wire format for protocol data.
package ztypes

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"znetwork/internal/zcrypto"
)

// Protocol constants. Changing any of these is a consensus-breaking change and
// requires a protocol version bump (specifications/VERSIONING_POLICY.md).
const (
	// ProtocolVersion is written into every header and genesis.
	ProtocolVersion uint16 = 1

	// CoinScale is the number of base units ("uz") in one whole Z token.
	CoinScale uint64 = 100_000_000
	// Decimals is the number of decimal places of the native token.
	Decimals = 8
	// MaxSupply is the hard cap on minted supply: 1,000,000,000 Z.
	MaxSupply uint64 = 1_000_000_000 * CoinScale

	// MaxMemoLen bounds the memo field (bytes).
	MaxMemoLen = 256
	// MaxPayloadLen bounds a transaction payload (bytes).
	MaxPayloadLen = 1 << 20
	// MaxGasLimit bounds the gas a single transaction may declare.
	MaxGasLimit uint64 = 30_000_000
	// MaxTxPerBlock bounds transactions per block.
	MaxTxPerBlock = 4000
	// MaxBlockBytes bounds the encoded size of a block's transactions.
	MaxBlockBytes = 4 << 20
	// MaxChainIDLen bounds the chain identifier length.
	MaxChainIDLen = 64
	// MaxValidators bounds the active validator set size.
	MaxValidators = 128
	// MaxProposalFieldLen bounds governance text fields.
	MaxProposalFieldLen = 2048
	// MaxMonikerLen bounds validator monikers.
	MaxMonikerLen = 64
)

// Protocol error identifiers. These are stable: RPC and SDK layers map them to
// numeric codes (see specifications/APIs/API_STANDARDS.md).
var (
	ErrChainIDMismatch   = errors.New("ztypes: chain id mismatch")
	ErrProtocolVersion   = errors.New("ztypes: unsupported protocol version")
	ErrTxUnsigned        = errors.New("ztypes: transaction is not signed")
	ErrTxSignature       = errors.New("ztypes: invalid transaction signature")
	ErrTxType            = errors.New("ztypes: unknown transaction type")
	ErrTxPayload         = errors.New("ztypes: malformed transaction payload")
	ErrTxGasLimit        = errors.New("ztypes: gas limit out of range")
	ErrTxMemo            = errors.New("ztypes: memo too long")
	ErrTxSize            = errors.New("ztypes: transaction too large")
	ErrTxTime            = errors.New("ztypes: transaction timestamp out of range")
	ErrSenderMismatch    = errors.New("ztypes: sender does not match public key")
	ErrInvalidAddress    = errors.New("ztypes: invalid address")
	ErrZeroAmount        = errors.New("ztypes: amount must be greater than zero")
	ErrInsufficientFunds = errors.New("ztypes: insufficient balance")
	ErrNonceMismatch     = errors.New("ztypes: nonce does not match account nonce")
	ErrGasExhausted      = errors.New("ztypes: gas exhausted")
	ErrBlockInvalid      = errors.New("ztypes: invalid block")
	ErrGenesisInvalid    = errors.New("ztypes: invalid genesis")
	ErrParamInvalid      = errors.New("ztypes: invalid parameter")
	ErrStakeInvalid      = errors.New("ztypes: invalid staking operation")
	ErrGovInvalid        = errors.New("ztypes: invalid governance operation")
	ErrContractInvalid   = errors.New("ztypes: invalid contract operation")
	ErrStateInvalid      = errors.New("ztypes: invalid state transition")
)

// Type aliases re-exported so the rest of the codebase has a single import for
// protocol types. The canonical definitions live in zcrypto (AGENT_08).
type (
	// Address is a 20-byte account address.
	Address = zcrypto.Address
	// Hash is a 32-byte SHA-256 digest.
	Hash = zcrypto.Hash
	// PublicKey is an Ed25519 public key.
	PublicKey = zcrypto.PublicKey
	// Signature is an Ed25519 signature.
	Signature = zcrypto.Signature
)

// SystemTreasuryAddress is the account that holds escrowed governance deposits.
// It is a normal state account, derived from a fixed domain string, so treasury
// balances are visible, auditable and part of the supply invariant.
func SystemTreasuryAddress() Address {
	d := zcrypto.Digest("ZNET/v1/system-treasury")
	var a Address
	copy(a[:], d[12:32])
	return a
}

// FormatUZ renders a base-unit amount as a decimal Z string with trailing zeros
// trimmed ("100000000" -> "1", "150000000" -> "1.5").
func FormatUZ(uz uint64) string {
	whole := uz / CoinScale
	frac := uz % CoinScale
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	s := fmt.Sprintf("%d.%08d", whole, frac)
	return strings.TrimRight(s, "0")
}

// ParseUZ parses a decimal Z amount ("1.25") into base units, rejecting
// negative values, more than 8 decimals, and overflow.
func ParseUZ(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("ztypes: empty amount")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("ztypes: negative amount")
	}
	wholeStr, fracStr, hasFrac := strings.Cut(s, ".")
	if wholeStr == "" {
		if !hasFrac || fracStr == "" {
			return 0, fmt.Errorf("ztypes: invalid amount %q", s)
		}
		wholeStr = "0"
	}
	if hasFrac && fracStr == "" {
		// "1." is not a canonical amount.
		return 0, fmt.Errorf("ztypes: invalid amount %q", s)
	}
	whole, err := strconv.ParseUint(wholeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ztypes: invalid amount %q", s)
	}
	if whole > MaxSupply/CoinScale {
		return 0, fmt.Errorf("ztypes: amount overflows supply")
	}
	var frac uint64
	if hasFrac {
		if len(fracStr) > Decimals {
			return 0, fmt.Errorf("ztypes: more than %d decimals", Decimals)
		}
		if fracStr != "" {
			frac, err = strconv.ParseUint(fracStr+strings.Repeat("0", Decimals-len(fracStr)), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("ztypes: invalid fractional part %q", fracStr)
			}
		}
	}
	total := whole*CoinScale + frac
	if total/CoinScale != whole || total < frac {
		return 0, fmt.Errorf("ztypes: amount overflows")
	}
	return total, nil
}
