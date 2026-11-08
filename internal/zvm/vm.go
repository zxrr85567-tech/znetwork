// Package zvm implements the Z Network Virtual Machine (AGENT_07).
//
// ZVM is a deterministic, gas-metered, 64-bit stack machine. It is deliberately
// small and auditable rather than EVM-compatible: Z Network does NOT claim EVM
// compatibility anywhere, because partial compatibility is worse than none
// (see ADR-0003).
//
// Determinism guarantees:
//   - no wall clock, no randomness, no floating point, no host introspection
//   - no unbounded loops without gas: every instruction costs at least 1 gas
//   - arithmetic wraps modulo 2^64. Wrapping is specified, tested, and is the only
//     arithmetic failure mode: it never panics and never depends on Go version.
//   - division by zero yields 0 (specified, tested) rather than trapping
//   - stack depth, program length, calldata and output are hard bounded
//
// Execution model:
//
//	state     = contract storage (uint64 slot -> uint64 value), journaled by host
//	input     = calldata bytes, readable as 8-byte big-endian words
//	output    = the operand stack at termination, serialized bottom-to-top
//	events    = values logged with LOG, surfaced in the transaction receipt
//
// A REVERT, out-of-gas or invalid-instruction condition aborts the call. The host
// is responsible for discarding storage writes made during an aborted call; the VM
// signals this with Result.Reverted.
package zvm

import (
	"errors"
	"fmt"

	"znetwork/internal/ztypes"
)

// Execution limits. These are part of consensus: changing them is a protocol
// change.
const (
	// MaxStackDepth is the maximum operand stack size.
	MaxStackDepth = 1024
	// MaxProgramSize bounds deployed bytecode.
	MaxProgramSize = 64 * 1024
	// MaxSteps bounds the number of instructions executed per call.
	MaxSteps = 200_000
	// MaxCalldata bounds call input size.
	MaxCalldata = 32 * 1024
	// MaxEvents bounds emitted log entries per call.
	MaxEvents = 32
)

// VM errors.
var (
	ErrOutOfGas       = errors.New("zvm: out of gas")
	ErrInvalidOpcode  = errors.New("zvm: invalid opcode")
	ErrStackUnderflow = errors.New("zvm: stack underflow")
	ErrStackOverflow  = errors.New("zvm: stack overflow")
	ErrBadJump        = errors.New("zvm: jump target out of range")
	ErrReverted       = errors.New("zvm: execution reverted")
	ErrTooManySteps   = errors.New("zvm: step limit exceeded")
	ErrProgramTooBig  = errors.New("zvm: program too large")
	ErrCalldataTooBig = errors.New("zvm: calldata too large")
)

// Host is the state interface the VM uses. Implementations must journal writes so
// that aborted calls leave no trace.
type Host interface {
	// Slot reads a storage slot (0 when unset).
	Slot(slot uint64) uint64
	// SetSlot writes a storage slot.
	SetSlot(slot, value uint64)
}

// Config is the immutable call context.
type Config struct {
	GasLimit uint64
	Caller   ztypes.Address
	Self     ztypes.Address
	Value    uint64
	Calldata []byte
}

// Result is the outcome of a call.
type Result struct {
	// Stack is the final operand stack (bottom first).
	Stack []uint64
	// Output is Stack serialized as 8-byte big-endian words.
	Output []byte
	// GasUsed is the gas consumed.
	GasUsed uint64
	// Events holds values emitted with LOG.
	Events []uint64
	// Reverted is true when the call failed and its writes must be discarded.
	Reverted bool
	// Err is the failure reason (nil on success).
	Err error
}

// AddressWord returns word i (0..2) of a 20-byte address, big-endian, where word 2
// contains only the final 4 bytes (upper 4 bytes zero).
func AddressWord(a ztypes.Address, i uint8) uint64 {
	switch i {
	case 0:
		return be64(a[0:8])
	case 1:
		return be64(a[8:16])
	case 2:
		// bytes 16..19, zero-extended to 64 bits
		return be64(a[16:20])
	default:
		return 0
	}
}

func be64(b []byte) uint64 {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// CalldataWord returns the i-th 8-byte big-endian word of calldata, or 0 when out
// of range.
func CalldataWord(data []byte, i uint64) uint64 {
	start := i * 8
	if start >= uint64(len(data)) {
		return 0
	}
	end := start + 8
	if end > uint64(len(data)) {
		end = uint64(len(data))
	}
	return be64(data[start:end])
}

// Run executes code with the given context. It never panics on malformed input:
// every failure path returns a Result with Reverted=true and Err set.
func Run(code []byte, cfg Config, host Host) (res *Result) {
	res = &Result{}
	if len(code) > MaxProgramSize {
		res.Reverted, res.Err = true, ErrProgramTooBig
		return res
	}
	if len(cfg.Calldata) > MaxCalldata {
		res.Reverted, res.Err = true, ErrCalldataTooBig
		return res
	}
	if host == nil {
		host = &nullHost{}
	}
	stack := make([]uint64, 0, 64)
	gas := uint64(0)
	steps := 0

	fail := func(err error) *Result {
		res.Reverted = true
		res.Err = err
		res.GasUsed = gas
		return res
	}
	charge := func(n uint64) bool {
		if gas+n > cfg.GasLimit {
			return false
		}
		gas += n
		return true
	}
	push := func(v uint64) bool {
		if len(stack) >= MaxStackDepth {
			return false
		}
		stack = append(stack, v)
		return true
	}
	pop := func() (uint64, bool) {
		if len(stack) == 0 {
			return 0, false
		}
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return v, true
	}

	pc := 0
	for {
		if pc >= len(code) {
			break // implicit STOP at end of code
		}
		steps++
		if steps > MaxSteps {
			return fail(ErrTooManySteps)
		}
		op := Op(code[pc])
		pc++
		if !charge(GasCost(op)) {
			return fail(ErrOutOfGas)
		}
		switch op {
		case OpStop:
			goto done
		case OpAdd, OpSub, OpMul, OpDiv, OpMod, OpLt, OpGt, OpEq, OpAnd, OpOr, OpXor,
			OpShl, OpShr:
			b, ok1 := pop()
			a, ok2 := pop()
			if !ok1 || !ok2 {
				return fail(ErrStackUnderflow)
			}
			var r uint64
			switch op {
			case OpAdd:
				r = a + b // specified wraparound
			case OpSub:
				r = a - b
			case OpMul:
				r = a * b
			case OpDiv:
				if b == 0 {
					r = 0 // specified, tested
				} else {
					r = a / b
				}
			case OpMod:
				if b == 0 {
					r = 0
				} else {
					r = a % b
				}
			case OpLt:
				if a < b {
					r = 1
				}
			case OpGt:
				if a > b {
					r = 1
				}
			case OpEq:
				if a == b {
					r = 1
				}
			case OpAnd:
				r = a & b
			case OpOr:
				r = a | b
			case OpXor:
				r = a ^ b
			case OpShl:
				if b < 64 {
					r = a << b
				}
			case OpShr:
				if b < 64 {
					r = a >> b
				}
			}
			if !push(r) {
				return fail(ErrStackOverflow)
			}
		case OpNot, OpIsZero:
			a, ok := pop()
			if !ok {
				return fail(ErrStackUnderflow)
			}
			if op == OpNot {
				if !push(^a) {
					return fail(ErrStackOverflow)
				}
			} else {
				if a == 0 {
					if !push(1) {
						return fail(ErrStackOverflow)
					}
				} else if !push(0) {
					return fail(ErrStackOverflow)
				}
			}
		case OpPush:
			if pc >= len(code) {
				// Truncated operand: the width byte is missing entirely.
				return fail(ErrInvalidOpcode)
			}
			n := int(code[pc])
			pc++
			if n < 1 || n > 8 || pc+n > len(code) {
				return fail(ErrInvalidOpcode)
			}
			if !charge(uint64(n)) {
				return fail(ErrOutOfGas)
			}
			v := be64(code[pc : pc+n])
			pc += n
			if !push(v) {
				return fail(ErrStackOverflow)
			}
		case OpPop:
			if _, ok := pop(); !ok {
				return fail(ErrStackUnderflow)
			}
		case OpDup:
			if len(stack) == 0 {
				return fail(ErrStackUnderflow)
			}
			if !push(stack[len(stack)-1]) {
				return fail(ErrStackOverflow)
			}
		case OpSwap:
			if len(stack) < 2 {
				return fail(ErrStackUnderflow)
			}
			stack[len(stack)-1], stack[len(stack)-2] = stack[len(stack)-2], stack[len(stack)-1]
		case OpJump:
			dest, ok := pop()
			if !ok {
				return fail(ErrStackUnderflow)
			}
			if dest >= uint64(len(code)) {
				return fail(ErrBadJump)
			}
			pc = int(dest)
		case OpJumpI:
			dest, ok1 := pop()
			cond, ok2 := pop()
			if !ok1 || !ok2 {
				return fail(ErrStackUnderflow)
			}
			if cond != 0 {
				if dest >= uint64(len(code)) {
					return fail(ErrBadJump)
				}
				pc = int(dest)
			}
		case OpJumpDest:
			// no-op marker so the assembler can emit stable targets
		case OpCaller, OpSelf:
			if pc >= len(code) {
				return fail(ErrInvalidOpcode)
			}
			word := code[pc]
			pc++
			var addr ztypes.Address
			if op == OpCaller {
				addr = cfg.Caller
			} else {
				addr = cfg.Self
			}
			if !push(AddressWord(addr, word)) {
				return fail(ErrStackOverflow)
			}
		case OpCallValue:
			if !push(cfg.Value) {
				return fail(ErrStackOverflow)
			}
		case OpCallDataSize:
			if !push(uint64(len(cfg.Calldata))) {
				return fail(ErrStackOverflow)
			}
		case OpCallDataWord:
			i, ok := pop()
			if !ok {
				return fail(ErrStackUnderflow)
			}
			if !push(CalldataWord(cfg.Calldata, i)) {
				return fail(ErrStackOverflow)
			}
		case OpSLoad:
			slot, ok := pop()
			if !ok {
				return fail(ErrStackUnderflow)
			}
			if !push(host.Slot(slot)) {
				return fail(ErrStackOverflow)
			}
		case OpSStore:
			// Stack order (top first): slot, value.
			slot, ok1 := pop()
			val, ok2 := pop()
			if !ok1 || !ok2 {
				return fail(ErrStackUnderflow)
			}
			host.SetSlot(slot, val)
		case OpLog:
			v, ok := pop()
			if !ok {
				return fail(ErrStackUnderflow)
			}
			if len(res.Events) >= MaxEvents {
				return fail(ErrStackOverflow)
			}
			res.Events = append(res.Events, v)
		case OpRevert:
			_, _ = pop()
			return fail(ErrReverted)
		default:
			return fail(fmt.Errorf("%w: 0x%02x", ErrInvalidOpcode, byte(op)))
		}
	}
done:
	// Serialize remaining stack (bottom first) as the call output.
	out := make([]byte, 0, len(stack)*8)
	for _, v := range stack {
		var b [8]byte
		for i := 0; i < 8; i++ {
			b[7-i] = byte(v >> (8 * i))
		}
		out = append(out, b[:]...)
	}
	res.Stack = stack
	res.Output = out
	res.GasUsed = gas
	return res
}

type nullHost struct{}

func (*nullHost) Slot(uint64) uint64     { return 0 }
func (*nullHost) SetSlot(uint64, uint64) {}
