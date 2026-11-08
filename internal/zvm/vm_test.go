package zvm

import (
	"errors"
	"testing"

	"znetwork/internal/ztypes"
)

type memHost struct {
	slots  map[uint64]uint64
	writes int
}

func newMemHost() *memHost { return &memHost{slots: map[uint64]uint64{}} }

func (m *memHost) Slot(s uint64) uint64 { return m.slots[s] }

func (m *memHost) SetSlot(s, v uint64) {
	m.slots[s] = v
	m.writes++
}

func addr(b byte) ztypes.Address {
	var a ztypes.Address
	for i := range a {
		a[i] = b
	}
	return a
}

func mustAssemble(t *testing.T, src string) []byte {
	t.Helper()
	r, err := Assemble(src)
	if err != nil {
		t.Fatalf("assemble: %v\n%s", err, src)
	}
	return r.Code
}

func TestArithmetic(t *testing.T) {
	cases := []struct {
		src  string
		want uint64
	}{
		{"PUSH 2\nPUSH 3\nADD", 5},
		{"PUSH 7\nPUSH 3\nSUB", 4},
		{"PUSH 6\nPUSH 7\nMUL", 42},
		{"PUSH 20\nPUSH 4\nDIV", 5},
		{"PUSH 22\nPUSH 5\nMOD", 2},
		{"PUSH 9\nPUSH 1\nDIV", 9}, // divisor 0 -> 0, tested below
		{"PUSH 1\nPUSH 1\nEQ", 1},
		{"PUSH 1\nPUSH 2\nEQ", 0},
		{"PUSH 3\nPUSH 5\nLT", 1},
		{"PUSH 3\nPUSH 5\nGT", 0},
		{"PUSH 0\nISZERO", 1},
		{"PUSH 8\nPUSH 1\nSHL", 16},
		{"PUSH 16\nPUSH 1\nSHR", 8},
		{"PUSH 0xf0\nPUSH 0x0f\nAND", 0x00},
		{"PUSH 0xf0\nPUSH 0x0f\nOR", 0xff},
		{"PUSH 0xff\nPUSH 0x0f\nXOR", 0xf0},
	}
	for _, c := range cases {
		res := Run(mustAssemble(t, c.src), Config{GasLimit: 100000}, newMemHost())
		if res.Reverted {
			t.Fatalf("%q reverted: %v", c.src, res.Err)
		}
		if len(res.Stack) != 1 || res.Stack[0] != c.want {
			t.Fatalf("%q => %v, want %d", c.src, res.Stack, c.want)
		}
	}
}

func TestDivisionByZeroIsSpecified(t *testing.T) {
	res := Run(mustAssemble(t, "PUSH 5\nPUSH 0\nDIV"), Config{GasLimit: 1000}, newMemHost())
	if res.Reverted || res.Stack[0] != 0 {
		t.Fatalf("div by zero => %v (reverted=%v)", res.Stack, res.Reverted)
	}
	res = Run(mustAssemble(t, "PUSH 5\nPUSH 0\nMOD"), Config{GasLimit: 1000}, newMemHost())
	if res.Reverted || res.Stack[0] != 0 {
		t.Fatalf("mod by zero => %v", res.Stack)
	}
}

func TestWraparoundArithmetic(t *testing.T) {
	// 2^64-1 + 1 == 0 (specified wraparound; not an error).
	res := Run(mustAssemble(t, "PUSH 0xffffffffffffffff\nPUSH 1\nADD"), Config{GasLimit: 1000}, newMemHost())
	if res.Reverted || res.Stack[0] != 0 {
		t.Fatalf("wraparound => %v", res.Stack)
	}
	// 0 - 1 == 2^64-1
	res = Run(mustAssemble(t, "PUSH 0\nPUSH 1\nSUB"), Config{GasLimit: 1000}, newMemHost())
	if res.Stack[0] != ^uint64(0) {
		t.Fatalf("underflow => %x", res.Stack[0])
	}
}

func TestStorageRoundTrip(t *testing.T) {
	host := newMemHost()
	code := mustAssemble(t, "PUSH 99\nPUSH 7\nSSTORE\nPUSH 7\nSLOAD")
	res := Run(code, Config{GasLimit: 100000}, host)
	if res.Reverted {
		t.Fatalf("reverted: %v", res.Err)
	}
	if res.Stack[0] != 99 {
		t.Fatalf("sload => %d", res.Stack[0])
	}
	if host.slots[7] != 99 {
		t.Fatalf("host slot not written: %v", host.slots)
	}
}

func TestControlFlowLoop(t *testing.T) {
	// sum 1..10 into slot 0
	src := `
		PUSH 0
		PUSH 0
		SSTORE          ; memory[0] = 0
		PUSH 1          ; i = 1
	loop:
		DUP
		PUSH 10
		GT
		JUMPI done
		DUP
		PUSH 0
		SLOAD
		ADD
		PUSH 0
		SSTORE
		PUSH 1
		ADD
		JUMP loop
	done:
		PUSH 0
		SLOAD
		STOP
	`
	res := Run(mustAssemble(t, src), Config{GasLimit: 1_000_000}, newMemHost())
	if res.Reverted {
		t.Fatalf("loop reverted: %v", res.Err)
	}
	if len(res.Stack) == 0 || res.Stack[len(res.Stack)-1] != 55 {
		t.Fatalf("sum 1..10 => %v, want 55", res.Stack)
	}
}

func TestCallerAccessControl(t *testing.T) {
	// Only the owner's address word 2 may write slot 1; anyone else reverts.
	src := `
		CALLER 2
		PUSH 0x01020304
		EQ
		JUMPI allowed
		PUSH 1
		REVERT
	allowed:
		PUSH 42
		PUSH 1
		SSTORE
		STOP
	`
	code := mustAssemble(t, src)
	owner := addr(0xaa)
	owner[16], owner[17], owner[18], owner[19] = 0x01, 0x02, 0x03, 0x04

	host := newMemHost()
	res := Run(code, Config{GasLimit: 100000, Caller: owner}, host)
	if res.Reverted {
		t.Fatalf("owner rejected: %v", res.Err)
	}
	if host.slots[1] != 42 {
		t.Fatalf("owner write missing: %v", host.slots)
	}

	stranger := addr(0xbb)
	host2 := newMemHost()
	res = Run(code, Config{GasLimit: 100000, Caller: stranger}, host2)
	if !res.Reverted || res.Err == nil {
		t.Fatal("stranger was not rejected")
	}
}

func TestCalldataReading(t *testing.T) {
	// echo calldata word 0
	code := mustAssemble(t, "PUSH 0\nCALLDATAWORD")
	res := Run(code, Config{GasLimit: 10000, Calldata: []byte{0, 0, 0, 0, 0, 0, 0, 9, 1}}, newMemHost())
	if res.Reverted || res.Stack[0] != 9 {
		t.Fatalf("calldata word => %v", res.Stack)
	}
	// out of range reads as zero, never panics
	res = Run(code, Config{GasLimit: 10000, Calldata: nil}, newMemHost())
	if res.Reverted || res.Stack[0] != 0 {
		t.Fatalf("empty calldata => %v", res.Stack)
	}
}

func TestGasAccountingIsExact(t *testing.T) {
	// PUSH 2 (3+1) + PUSH 3 (3+1) + ADD 3 + STOP 0 = 11
	r, err := Assemble("PUSH 2\nPUSH 3\nADD\nSTOP")
	if err != nil {
		t.Fatal(err)
	}
	res := Run(r.Code, Config{GasLimit: 100}, newMemHost())
	want := uint64(GasCost(OpPush) + 1 + GasCost(OpPush) + 1 + GasCost(OpAdd) + GasCost(OpStop))
	if res.GasUsed != want {
		t.Fatalf("gas used = %d, want %d", res.GasUsed, want)
	}
}

func TestOutOfGas(t *testing.T) {
	code := mustAssemble(t, "PUSH 1\nPUSH 1\nSSTORE")
	res := Run(code, Config{GasLimit: 100}, newMemHost())
	if !errors.Is(res.Err, ErrOutOfGas) || !res.Reverted {
		t.Fatalf("expected out of gas, got %v", res.Err)
	}
}

func TestStackUnderflowAndOverflow(t *testing.T) {
	res := Run(mustAssemble(t, "ADD"), Config{GasLimit: 1000}, newMemHost())
	if !errors.Is(res.Err, ErrStackUnderflow) {
		t.Fatalf("underflow: %v", res.Err)
	}
	// 1025 pushes must overflow.
	src := ""
	for i := 0; i < MaxStackDepth+1; i++ {
		src += "PUSH 1\n"
	}
	res = Run(mustAssemble(t, src), Config{GasLimit: 1_000_000}, newMemHost())
	if !errors.Is(res.Err, ErrStackOverflow) {
		t.Fatalf("overflow: %v", res.Err)
	}
}

func TestInvalidOpcodeAndJump(t *testing.T) {
	res := Run([]byte{0xfe}, Config{GasLimit: 1000}, newMemHost())
	if !errors.Is(res.Err, ErrInvalidOpcode) {
		t.Fatalf("invalid opcode: %v", res.Err)
	}
	// jump far outside the program: PUSH2 0xffff then JUMP
	code := []byte{byte(OpPush), 2, 0xff, 0xff, byte(OpJump)}
	res = Run(code, Config{GasLimit: 1000}, newMemHost())
	if !errors.Is(res.Err, ErrBadJump) {
		t.Fatalf("bad jump: %v", res.Err)
	}
}

func TestRevertDiscardsNothingByItself(t *testing.T) {
	// The VM reports Reverted; the host (and only the host) must journal writes.
	host := newMemHost()
	res := Run(mustAssemble(t, "PUSH 1\nPUSH 1\nSSTORE\nPUSH 1\nREVERT"), Config{GasLimit: 100000}, host)
	if !res.Reverted {
		t.Fatal("expected revert")
	}
	// Documented contract: VM does not roll back; zexec journals storage.
	if host.writes != 1 {
		t.Fatalf("expected the write to reach the host (journaled by caller), got %d", host.writes)
	}
}

func TestEventLogging(t *testing.T) {
	res := Run(mustAssemble(t, "PUSH 7\nLOG\nPUSH 8\nLOG"), Config{GasLimit: 10000}, newMemHost())
	if len(res.Events) != 2 || res.Events[0] != 7 || res.Events[1] != 8 {
		t.Fatalf("events => %v", res.Events)
	}
}

func TestDeterminism(t *testing.T) {
	code := mustAssemble(t, "PUSH 5\nPUSH 3\nMUL\nPUSH 2\nADD")
	var first *Result
	for i := 0; i < 50; i++ {
		res := Run(code, Config{GasLimit: 1000}, newMemHost())
		if first == nil {
			first = res
			continue
		}
		if res.GasUsed != first.GasUsed || len(res.Stack) != len(first.Stack) || res.Stack[0] != first.Stack[0] {
			t.Fatal("non-deterministic execution")
		}
	}
}

func TestAssemblerRejectsBadInput(t *testing.T) {
	bad := []string{
		"PUSH",         // missing operand
		"PUSH abc",     // bad literal
		"JUMP nowhere", // undefined label
		"NOPE",         // unknown op
		"CALLER 9",     // bad word index
		"STOP\nSTOP 1", // operand where none allowed
		"a:\na:",       // duplicate label
	}
	for _, src := range bad {
		if _, err := Assemble(src); err == nil {
			t.Fatalf("assembler accepted %q", src)
		}
	}
}

func TestDisassemblerNeverPanics(t *testing.T) {
	for _, code := range [][]byte{{}, {0x30}, {0x30, 9}, {0x50}, {0xff, 0x00, 0x30, 3}} {
		_ = Disassemble(code)
	}
	if out := Disassemble(mustAssemble(t, "PUSH 5\nADD")); out == "" {
		t.Fatal("empty disassembly")
	}
}

// FuzzRun asserts the VM cannot panic on arbitrary bytecode.
func FuzzRun(f *testing.F) {
	f.Add([]byte{0x30, 1, 5, 0x01, 0x00})
	f.Add([]byte{0x61, 0x70})
	f.Add([]byte{0x40})
	f.Fuzz(func(t *testing.T, code []byte) {
		if len(code) > MaxProgramSize {
			return
		}
		_ = Run(code, Config{GasLimit: 200_000, Calldata: []byte{1, 2, 3}}, newMemHost())
	})
}
