package zvm

import (
	"fmt"
	"strconv"
	"strings"
)

// Assembler for the Z Network Virtual Machine.
//
// Syntax (one instruction per line; ';' or '#' starts a comment):
//
//	PUSH 42              ; push the literal 42 (decimal or 0x-hex)
//	PUSH 0xff
//	ADD                  ; pop b, pop a, push a+b
//	CALLER 0             ; push word 0 of the caller address
//	SLOAD / SSTORE       ; storage access
//	JUMP  loop           ; unconditional jump to a label (assembler emits PUSH4+JUMP)
//	JUMPI done           ; pop dest, pop cond, jump if cond != 0
//	LOG                  ; emit top of stack as an event
//	label:               ; a label; emits JUMPDEST and becomes a jump target
//	STOP
//
// Jump targets are always encoded as a fixed 4-byte push so that label addresses
// do not shift while the program is being assembled (a classic assembler bug that
// produces jump-into-immediate corruption).
//
// The assembler is part of the developer toolchain (AGENT_19), but the bytecode it
// emits is ordinary consensus-level ZVM code.
type AsmResult struct {
	Code   []byte
	Labels map[string]int
}

// Assemble converts assembly source into bytecode.
func Assemble(src string) (*AsmResult, error) {
	type instr struct {
		op    Op
		imm   []byte
		label string
		line  int
	}
	var prog []instr
	for ln, raw := range strings.Split(src, "\n") {
		line := raw
		if i := strings.IndexAny(line, ";#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, ":") {
			prog = append(prog, instr{op: OpJumpDest, label: strings.TrimSuffix(line, ":"), line: ln + 1})
			continue
		}
		fields := strings.Fields(line)
		name := strings.ToUpper(fields[0])
		switch name {
		case "PUSH":
			if len(fields) != 2 {
				return nil, fmt.Errorf("zvm asm: line %d: PUSH needs one operand", ln+1)
			}
			v, err := strconv.ParseUint(fields[1], 0, 64)
			if err != nil {
				return nil, fmt.Errorf("zvm asm: line %d: bad literal %q", ln+1, fields[1])
			}
			prog = append(prog, instr{op: OpPush, imm: le(v), line: ln + 1})
		case "JUMP", "JUMPI":
			if len(fields) != 2 {
				return nil, fmt.Errorf("zvm asm: line %d: %s needs a label", ln+1, name)
			}
			op := OpJump
			if name == "JUMPI" {
				op = OpJumpI
			}
			prog = append(prog, instr{op: op, label: fields[1], line: ln + 1})
		case "CALLER", "SELF":
			if len(fields) != 2 {
				return nil, fmt.Errorf("zvm asm: line %d: %s needs a word index", ln+1, name)
			}
			w, err := strconv.ParseUint(fields[1], 10, 8)
			if err != nil || w > 2 {
				return nil, fmt.Errorf("zvm asm: line %d: word index must be 0..2", ln+1)
			}
			op := OpCaller
			if name == "SELF" {
				op = OpSelf
			}
			prog = append(prog, instr{op: op, imm: []byte{byte(w)}, line: ln + 1})
		default:
			op, ok := nameToOp(name)
			if !ok {
				return nil, fmt.Errorf("zvm asm: line %d: unknown instruction %q", ln+1, name)
			}
			if len(fields) != 1 {
				return nil, fmt.Errorf("zvm asm: line %d: %s takes no operands", ln+1, name)
			}
			prog = append(prog, instr{op: op, line: ln + 1})
		}
	}

	// Pass 1: compute offsets with jump targets assumed to be PUSH4 (5 bytes).
	labels := make(map[string]int)
	off := 0
	for _, in := range prog {
		if in.label != "" && in.op == OpJumpDest {
			if _, dup := labels[in.label]; dup {
				return nil, fmt.Errorf("zvm asm: line %d: duplicate label %q", in.line, in.label)
			}
			labels[in.label] = off
		}
		off += instrSize(in.op, in.imm)
	}

	// Pass 2: emit, resolving labels.
	var code []byte
	for _, in := range prog {
		switch {
		case in.op == OpPush:
			code = append(code, byte(OpPush), byte(len(in.imm)))
			code = append(code, in.imm...)
		case in.op == OpJump || in.op == OpJumpI:
			target, ok := labels[in.label]
			if !ok {
				return nil, fmt.Errorf("zvm asm: line %d: undefined label %q", in.line, in.label)
			}
			if target > 0xffffffff {
				return nil, fmt.Errorf("zvm asm: line %d: program too large for a 4-byte jump target", in.line)
			}
			code = append(code, byte(OpPush), 4, byte(target>>24), byte(target>>16), byte(target>>8), byte(target))
			code = append(code, byte(in.op))
		case in.op == OpCaller || in.op == OpSelf:
			code = append(code, byte(in.op), in.imm[0])
		default:
			code = append(code, byte(in.op))
		}
	}
	if len(code) > MaxProgramSize {
		return nil, fmt.Errorf("zvm asm: program exceeds %d bytes", MaxProgramSize)
	}
	return &AsmResult{Code: code, Labels: labels}, nil
}

// le trims a big-endian literal to its minimal representation (at least 1 byte).
func le(v uint64) []byte {
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[7-i] = byte(v >> (8 * i))
	}
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	out := make([]byte, 8-i)
	copy(out, b[i:])
	return out
}

func instrSize(op Op, imm []byte) int {
	switch op {
	case OpPush:
		return 2 + len(imm)
	case OpJump, OpJumpI:
		return 7 // PUSH opcode + len + 4 immediate bytes + jump opcode
	case OpCaller, OpSelf:
		return 2
	default:
		return 1
	}
}

func nameToOp(name string) (Op, bool) {
	ops := map[string]Op{
		"STOP": OpStop, "ADD": OpAdd, "SUB": OpSub, "MUL": OpMul, "DIV": OpDiv, "MOD": OpMod,
		"LT": OpLt, "GT": OpGt, "EQ": OpEq, "ISZERO": OpIsZero,
		"AND": OpAnd, "OR": OpOr, "XOR": OpXor, "NOT": OpNot, "SHL": OpShl, "SHR": OpShr,
		"POP": OpPop, "DUP": OpDup, "SWAP": OpSwap,
		"JUMPDEST": OpJumpDest, "CALLVALUE": OpCallValue,
		"CALLDATASIZE": OpCallDataSize, "CALLDATAWORD": OpCallDataWord,
		"SLOAD": OpSLoad, "SSTORE": OpSStore, "LOG": OpLog, "REVERT": OpRevert,
	}
	op, ok := ops[name]
	return op, ok
}

// Disassemble renders bytecode as assembly text. It is used by the explorer and by
// the CLI, and it never panics on invalid input.
func Disassemble(code []byte) string {
	var sb strings.Builder
	for pc := 0; pc < len(code); {
		op := Op(code[pc])
		start := pc
		pc++
		switch op {
		case OpPush:
			if pc >= len(code) {
				fmt.Fprintf(&sb, "%04d INVALID(truncated PUSH)\n", start)
				return sb.String()
			}
			n := int(code[pc])
			pc++
			if n < 1 || n > 8 || pc+n > len(code) {
				fmt.Fprintf(&sb, "%04d INVALID(bad PUSH length)\n", start)
				return sb.String()
			}
			var v uint64
			for i := 0; i < n; i++ {
				v = v<<8 | uint64(code[pc+i])
			}
			fmt.Fprintf(&sb, "%04d PUSH %d (0x%x)\n", start, v, v)
			pc += n
		case OpCaller, OpSelf:
			if pc >= len(code) {
				fmt.Fprintf(&sb, "%04d INVALID(truncated %s)\n", start, OpName(op))
				return sb.String()
			}
			fmt.Fprintf(&sb, "%04d %s %d\n", start, OpName(op), code[pc])
			pc++
		default:
			fmt.Fprintf(&sb, "%04d %s\n", start, OpName(op))
		}
	}
	return sb.String()
}
