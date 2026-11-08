package zvm

// Op is a ZVM opcode.
type Op byte

// Opcode table. Values are part of the ABI and the bytecode format; they must not
// be renumbered (see specifications/smart-contracts/ZVM_SPECIFICATION.md).
const (
	// control
	OpStop     Op = 0x00
	OpJump     Op = 0x40
	OpJumpI    Op = 0x41
	OpJumpDest Op = 0x42
	OpRevert   Op = 0x71

	// arithmetic / logic
	OpAdd    Op = 0x01
	OpSub    Op = 0x02
	OpMul    Op = 0x03
	OpDiv    Op = 0x04
	OpMod    Op = 0x05
	OpLt     Op = 0x10
	OpGt     Op = 0x11
	OpEq     Op = 0x12
	OpIsZero Op = 0x13
	OpAnd    Op = 0x20
	OpOr     Op = 0x21
	OpXor    Op = 0x22
	OpNot    Op = 0x23
	OpShl    Op = 0x24
	OpShr    Op = 0x25

	// stack
	OpPush Op = 0x30 // followed by u8 length (1..8) and that many big-endian bytes
	OpPop  Op = 0x31
	OpDup  Op = 0x32
	OpSwap Op = 0x33

	// context
	OpCaller       Op = 0x50 // followed by u8 word index (0..2)
	OpSelf         Op = 0x51 // followed by u8 word index (0..2)
	OpCallValue    Op = 0x52
	OpCallDataSize Op = 0x53
	OpCallDataWord Op = 0x54

	// storage / effects
	OpSLoad  Op = 0x60
	OpSStore Op = 0x61
	OpLog    Op = 0x70
)

// Gas schedule. Costs are consensus constants.
//
// Rationale: SSTORE dominates because it is the only operation with permanent
// state growth; SLOAD is priced to discourage storage-heavy loops; jumps cost more
// than arithmetic to bound control-flow throughput. These values are initial
// proposals for a development network, not measured production pricing.
var gasCosts = map[Op]uint64{
	OpStop: 0, OpAdd: 3, OpSub: 3, OpMul: 5, OpDiv: 5, OpMod: 5,
	OpLt: 3, OpGt: 3, OpEq: 3, OpIsZero: 3,
	OpAnd: 3, OpOr: 3, OpXor: 3, OpNot: 3, OpShl: 3, OpShr: 3,
	OpPush: 3, OpPop: 2, OpDup: 3, OpSwap: 3,
	OpJump: 8, OpJumpI: 10, OpJumpDest: 1,
	OpCaller: 2, OpSelf: 2, OpCallValue: 2, OpCallDataSize: 2, OpCallDataWord: 3,
	OpSLoad: 100, OpSStore: 20_000, OpLog: 375,
	OpRevert: 0,
}

// GasCost returns the base gas for an opcode. Unknown opcodes cost 1 gas so that
// an invalid instruction still terminates deterministically via charge-then-fail.
func GasCost(op Op) uint64 {
	if c, ok := gasCosts[op]; ok {
		return c
	}
	return 1
}

// OpName returns the mnemonic for an opcode.
func OpName(op Op) string {
	switch op {
	case OpStop:
		return "STOP"
	case OpAdd:
		return "ADD"
	case OpSub:
		return "SUB"
	case OpMul:
		return "MUL"
	case OpDiv:
		return "DIV"
	case OpMod:
		return "MOD"
	case OpLt:
		return "LT"
	case OpGt:
		return "GT"
	case OpEq:
		return "EQ"
	case OpIsZero:
		return "ISZERO"
	case OpAnd:
		return "AND"
	case OpOr:
		return "OR"
	case OpXor:
		return "XOR"
	case OpNot:
		return "NOT"
	case OpShl:
		return "SHL"
	case OpShr:
		return "SHR"
	case OpPush:
		return "PUSH"
	case OpPop:
		return "POP"
	case OpDup:
		return "DUP"
	case OpSwap:
		return "SWAP"
	case OpJump:
		return "JUMP"
	case OpJumpI:
		return "JUMPI"
	case OpJumpDest:
		return "JUMPDEST"
	case OpCaller:
		return "CALLER"
	case OpSelf:
		return "SELF"
	case OpCallValue:
		return "CALLVALUE"
	case OpCallDataSize:
		return "CALLDATASIZE"
	case OpCallDataWord:
		return "CALLDATAWORD"
	case OpSLoad:
		return "SLOAD"
	case OpSStore:
		return "SSTORE"
	case OpLog:
		return "LOG"
	case OpRevert:
		return "REVERT"
	default:
		return "INVALID"
	}
}
