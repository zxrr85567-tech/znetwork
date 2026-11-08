# ZVM Specification

**Implementation:** `internal/zvm/` (`vm.go`, `opcodes.go`, `asm.go`)
**Verified by:** `internal/zvm/vm_test.go`, `FuzzRun` (1.3M+ executions)

ZVM is a small deterministic stack machine for on-chain contracts. It is **not**
the EVM: there is no ABI, no dynamic memory, no call stack between contracts, no
self-destruct, and no floating point. That is a deliberate scope decision
(ADR-0003) — the goal is a contract layer that can be fully reviewed, not one
that is Turing-compatible with an existing ecosystem.

## Machine model

| Property | Value |
|---|---|
| Word size | 256-bit values represented as `uint64` |
| Operand stack | max depth 1024 |
| Instruction budget | `MaxSteps = 200,000` per call |
| Memory | none — calldata words are addressed directly |
| Storage | per-contract slots, key `(contract, slot u64)`, value `uint64` |
| Persistence | storage writes are journaled and reverted on failure |
| Cross-contract calls | **not supported** (LIMITATION below) |

Because values are `uint64`, arithmetic wraps modulo 2⁶⁴ and there are no
overflow gas costs in this build. Division and modulo by zero return 0 rather
than trapping, so a program cannot be halted by a hostile input value — it still
pays gas for every step.

## Instruction set

### Control

| Op | Code | Gas | Effect |
|---|---|---|---|
| `STOP` | 0x00 | 0 | halt successfully |
| `JUMP` | 0x40 | 8 | pop destination; `pc = dest` (must be < code length) |
| `JUMPI` | 0x41 | 10 | pop dest, cond; jump if `cond != 0` |
| `JUMPDEST` | 0x42 | 1 | marker, no effect (targets are stable) |
| `REVERT` | 0x71 | 0 | halt, revert all storage writes |

### Arithmetic and comparison

| Op | Code | Gas | Effect |
|---|---|---|---|
| `ADD` `SUB` `MUL` | 0x01–0x03 | 3/3/5 | wrapping arithmetic |
| `DIV` `MOD` | 0x04, 0x05 | 5 | by zero ⇒ 0 |
| `LT` `GT` `EQ` | 0x10–0x12 | 3 | push 1/0 |
| `ISZERO` | 0x13 | 3 | push 1 if zero |

### Bitwise

| Op | Code | Gas |
|---|---|---|
| `AND` `OR` `XOR` `NOT` | 0x20–0x23 | 3 |
| `SHL` `SHR` | 0x24, 0x25 | 3 |

Shifts by ≥ 64 produce 0 (they do not wrap).

### Stack

| Op | Code | Gas | Effect |
|---|---|---|---|
| `PUSH` | 0x30 | 3 + n | `u8 n (1..8) ‖ n bytes big-endian`; a missing width byte or a truncated operand is `invalid opcode`, never a panic |
| `POP` | 0x31 | 2 | discard top |
| `DUP` | 0x32 | 3 | duplicate top |
| `SWAP` | 0x33 | 3 | swap top two |

`PUSH` costs `3 + n` gas, so a program cannot cheaply encode huge constants; and
`PUSH` with `n = 0` or `n > 8` is rejected, so constant size is bounded by the
word size rather than by the code length.

### Context

| Op | Code | Gas | Effect |
|---|---|---|---|
| `CALLER` | 0x50 | 2 | push word `i` (u8, 0..2) of the caller address |
| `SELF` | 0x51 | 2 | push word `i` of the contract address |
| `CALLVALUE` | 0x52 | 2 | push attached value |
| `CALLDATASIZE` | 0x53 | 2 | push calldata length |
| `CALLDATALOAD` | 0x54 | 3 | push word `i` of calldata (0 if out of range) |

Addresses are 20 bytes; the three words are the 8-byte big-endian slices
`[0:8]`, `[8:16]`, `[16:24]` with the last word right-padded with zeroes.

### Storage and events

| Op | Code | Gas | Effect |
|---|---|---|---|
| `SLOAD` | 0x60 | 100 | push storage slot value |
| `SSTORE` | 0x61 | 20,000 | write storage slot |
| `LOG` | 0x70 | 375 | emit an event with two words from the stack |

Unknown opcodes cost 1 gas and halt with `invalid opcode`. There is no "silently
ignore" path: an unknown byte is a failure, which is what makes contract
behaviour stable across upgrades.

## Failure semantics

The distinction that matters to a node, and that the execution engine maps to a
receipt status:

| Outcome | Receipt | Gas charged | State changes |
|---|---|---|---|
| `STOP` / end of code | success | gas used | kept |
| `REVERT` | reverted | gas used | discarded |
| out of gas / bad jump / stack underflow / invalid opcode | reverted | gas **limit** (all of it) | discarded |

Storage writes are journaled during execution: on failure the journal is
replayed backwards, so the contract's storage is exactly what it was before the
call. A failed transaction still pays its fee and still consumes its nonce; it is
a valid transaction, just an unsuccessful one.

## Contract lifecycle

```
deploy:  address = Digest("ZNET/v1/contract-address", sender, nonce)[12:32]
         codeHash = Digest("ZNET/v1/contract-code", code)
         account.CodeHash = codeHash; state[CodeKey(codeHash)] = code
         account.CreatedHeight = height
         event: contract_deployed {contract, code_hash, code_size, deployer}

call:    load code by the account's code hash (a call to a non-contract fails)
         run with caller = tx.Sender, self = contract address, value = tx value
         value is transferred to the contract account before execution
         on failure the value transfer is rolled back with the state
         event: contract_called {contract, gas_used, success}
```

Verified on a live 4-validator network: deploying
`PUSH 7; PUSH 0; SSTORE; PUSH 0; SLOAD; STOP` produced a receipt with
`contract_address`, `code_hash`, `code_size = 12`, `deployer`, 46,712 gas used
and event `contract_deployed`, with `output = 0000000000000007`.

## Assembler

`internal/zvm/asm.go` compiles a line-oriented assembly format used by
`zwallet deploy --asm-file` / `--code` and by the tests:

```
PUSH 7        ; opcode with one decimal operand
PUSH 0
SSTORE
PUSH 0
SLOAD
STOP
```

Comments (`;`) and blank lines are ignored. The assembler rejects unknown
mnemonics, out-of-range operands and operands given to zero-operand opcodes;
`zwallet deploy` refuses to sign a transaction whose assembly does not compile.

## Determinism and gas metering

- Every instruction charges gas **before** it executes, so an instruction that
  would push the total over the limit never runs.
- `MaxSteps` is a second, independent bound: a program that somehow pays no gas
  (opcode gas 0) still cannot run forever.
- No instruction reads time, randomness, previous block hashes or other
  contracts, so execution depends only on `(code, calldata, storage, caller,
  self, value)`. Every node therefore computes the same result.
- `LOG` output is capped by the receipt encoder (see EXECUTION.md), so a
  contract cannot produce an unbounded receipt.

## Fuzzing result

`FuzzRun` feeds arbitrary byte strings as contracts with a fresh state and a
fixed gas limit, and asserts: no panic, gas never exceeds the limit, and a
successful run never leaves a partially applied storage write. During the
campaign it found a real crash: `PUSH` at the last byte read `code[pc]` without
a bounds check. Fixed, with a regression input kept in
`internal/zvm/testdata/fuzz/FuzzRun/`. A later 45-second campaign executed
1,322,610 programs with no further crashes.

## Limitations (explicit)

1. **No cross-contract calls.** A contract cannot call another contract. Value
   transfer to a contract account works, but code execution is single-frame.
2. **64-bit words.** No 256-bit arithmetic; a contract doing wide arithmetic must
   implement it by hand over multiple slots.
3. **No dynamic memory.** `LOG` is the only explicit output channel. The receipt
   `Output` field is the entire remaining operand stack serialised as 8-byte
   big-endian words from bottom to top, so a caller can read results without an
   ABI — but a program that leaves several values on the stack produces a
   concatenation, not a typed return value.
4. **No metered storage rent.** Storage is permanent and paid for once.
5. **No formal verification.** The instruction set is small enough to review by
   hand, and the tests cover every opcode, but there is no machine-checked proof.
