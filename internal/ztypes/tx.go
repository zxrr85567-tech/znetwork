package ztypes

import (
	"fmt"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
)

// Hash domains used by this package. They extend the crypto module's domains.
const (
	domainTxID       = "ZNET/v1/tx-id"
	domainPayload    = "ZNET/v1/tx-payload"
	domainHeader     = zcrypto.DomainBlock
	domainGenesis    = zcrypto.DomainGenesis
	domainTxRoot     = zcrypto.DomainTxRoot
	domainReceipt    = zcrypto.DomainReceiptRoot
	domainVote       = zcrypto.DomainVote
	domainGovernance = "ZNET/v1/governance-id"
	domainContract   = zcrypto.DomainContractAddr
)

// TxType enumerates transaction kinds. Values are part of the wire protocol.
type TxType uint8

// Transaction types.
const (
	// TxTransfer moves the native token between accounts.
	TxTransfer TxType = 1
	// TxDelegate bonds stake to a validator.
	TxDelegate TxType = 2
	// TxUndelegate starts unbonding of delegated stake.
	TxUndelegate TxType = 3
	// TxValidatorRegister registers (or updates) a validator's on-chain record.
	TxValidatorRegister TxType = 4
	// TxGovProposal submits a governance proposal.
	TxGovProposal TxType = 5
	// TxGovVote votes on a governance proposal.
	TxGovVote TxType = 6
	// TxContractDeploy deploys ZVM bytecode.
	TxContractDeploy TxType = 7
	// TxContractCall invokes deployed bytecode.
	TxContractCall TxType = 8
)

// String returns a stable human-readable name.
func (t TxType) String() string {
	switch t {
	case TxTransfer:
		return "transfer"
	case TxDelegate:
		return "delegate"
	case TxUndelegate:
		return "undelegate"
	case TxValidatorRegister:
		return "validator_register"
	case TxGovProposal:
		return "gov_proposal"
	case TxGovVote:
		return "gov_vote"
	case TxContractDeploy:
		return "contract_deploy"
	case TxContractCall:
		return "contract_call"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Valid reports whether t is a defined transaction type.
func (t TxType) Valid() bool { return t >= TxTransfer && t <= TxContractCall }

// Tx is a signed Z Network transaction.
//
// Signing model:
//
//	body  = 0x01 || chainID || type || sender || pubkey || nonce || gasLimit ||
//	        gasPrice || timeMs || memo || payload
//	txID  = Digest("ZNET/v1/tx-id", body)
//	sig   = Ed25519_sign(priv, Digest("ZNET/v1/transaction", txID))
//
// Because the chain id is inside txID, a transaction can never be replayed on
// another network (threat T-03). Because the nonce is inside txID and the account
// nonce must match exactly, a transaction can never be replayed on the same
// network (threat T-04). Ed25519 is non-malleable and verification uses the
// strict stdlib implementation (threat T-02).
type Tx struct {
	ChainID   string
	Type      TxType
	Sender    Address
	PublicKey PublicKey
	Nonce     uint64
	GasLimit  uint64
	GasPrice  uint64
	TimeMs    int64
	Memo      string
	Payload   []byte
	Signature Signature
}

// EncodeBody returns the canonical unsigned encoding of the transaction.
func (tx *Tx) EncodeBody() []byte {
	w := zencoding.NewWriterSize(256)
	w.U8(0x01) // envelope version
	w.Str(tx.ChainID)
	w.U8(uint8(tx.Type))
	w.Raw(tx.Sender[:])
	w.Raw(tx.PublicKey[:])
	w.U64(tx.Nonce)
	w.U64(tx.GasLimit)
	w.U64(tx.GasPrice)
	w.I64(tx.TimeMs)
	w.Str(tx.Memo)
	w.Bytes(tx.Payload)
	return w.Done()
}

// ID returns the transaction identifier (its hash).
func (tx *Tx) ID() Hash { return zcrypto.Digest(domainTxID, tx.EncodeBody()) }

// SigningMessage is the exact digest that is signed.
func (tx *Tx) SigningMessage() []byte {
	id := tx.ID()
	return id[:]
}

// Sign attaches a signature made by k. It refuses to sign a transaction whose
// sender field is not the key's own address, which prevents accidentally
// producing an unusable (or misleading) transaction.
func (tx *Tx) Sign(k *zcrypto.PrivateKey) error {
	if k == nil {
		return ErrTxUnsigned
	}
	if tx.Sender != k.Address() {
		return ErrSenderMismatch
	}
	tx.PublicKey = k.PublicKey()
	tx.Signature = k.Sign(zcrypto.DomainTx, tx.SigningMessage())
	return nil
}

// VerifySignature checks the signature and the sender/public-key binding.
func (tx *Tx) VerifySignature() error {
	if tx.Signature.IsZero() {
		return ErrTxUnsigned
	}
	if err := zcrypto.VerifyAddressSignature(tx.Sender, tx.PublicKey, zcrypto.DomainTx, tx.SigningMessage(), tx.Signature); err != nil {
		return fmt.Errorf("%w: %v", ErrTxSignature, err)
	}
	return nil
}

// Encode returns the canonical signed encoding: body followed by the signature.
func (tx *Tx) Encode() []byte {
	body := tx.EncodeBody()
	w := zencoding.NewWriterSize(len(body) + 64)
	w.Raw(body)
	w.Raw(tx.Signature[:])
	return w.Done()
}

// DecodeTx parses a canonical signed transaction.
func DecodeTx(b []byte) (*Tx, error) {
	tx := &Tx{}
	r := zencoding.NewReader(b)
	if v := r.U8(); v != 0x01 {
		return nil, fmt.Errorf("%w: envelope version %d", ErrTxPayload, v)
	}
	tx.ChainID = r.Str()
	tx.Type = TxType(r.U8())
	sender := r.Raw(20)
	pub := r.Raw(32)
	copy(tx.Sender[:], sender)
	copy(tx.PublicKey[:], pub)
	tx.Nonce = r.U64()
	tx.GasLimit = r.U64()
	tx.GasPrice = r.U64()
	tx.TimeMs = r.I64()
	tx.Memo = r.Str()
	tx.Payload = append([]byte(nil), r.Bytes(MaxPayloadLen)...)
	sig := r.Raw(64)
	copy(tx.Signature[:], sig)
	if err := r.Done(); err != nil {
		// Done rejects both truncation and trailing bytes: a transaction has
		// exactly one valid encoding, so no two byte strings map to the same tx.
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	return tx, nil
}

// ValidateStatic performs every check that does not require chain state:
// chain identity, type, ranges, signature and sender binding.
func (tx *Tx) ValidateStatic(chainID string) error {
	if tx.ChainID != chainID {
		return fmt.Errorf("%w: got %q want %q", ErrChainIDMismatch, tx.ChainID, chainID)
	}
	if !tx.Type.Valid() {
		return fmt.Errorf("%w: %d", ErrTxType, uint8(tx.Type))
	}
	if tx.GasLimit == 0 || tx.GasLimit > MaxGasLimit {
		return fmt.Errorf("%w: %d", ErrTxGasLimit, tx.GasLimit)
	}
	if len(tx.Memo) > MaxMemoLen {
		return ErrTxMemo
	}
	if len(tx.Payload) > MaxPayloadLen {
		return ErrTxSize
	}
	if tx.TimeMs <= 0 {
		return fmt.Errorf("%w: missing timestamp", ErrTxTime)
	}
	if tx.Sender.IsZero() || tx.PublicKey.IsZero() {
		return ErrInvalidAddress
	}
	if err := tx.VerifySignature(); err != nil {
		return err
	}
	return tx.validatePayloadShape()
}

// Fee returns gasLimit*gasPrice with overflow detection.
func (tx *Tx) Fee() (uint64, error) {
	hi, _ := mulDivOverflow(tx.GasLimit, tx.GasPrice)
	if hi {
		return 0, fmt.Errorf("%w: fee overflow", ErrTxGasLimit)
	}
	return tx.GasLimit * tx.GasPrice, nil
}

// mulDivOverflow reports whether a*b overflows uint64.
func mulDivOverflow(a, b uint64) (bool, uint64) {
	if a == 0 || b == 0 {
		return false, 0
	}
	p := a * b
	if p/b != a {
		return true, 0
	}
	return false, p
}

// ---- payload codecs --------------------------------------------------------
//
// Every payload begins with a one-byte version tag unique to its type, so a
// payload of one kind can never be decoded as another (threat T-15).

const (
	payloadTagTransfer    = 0x11
	payloadTagDelegate    = 0x12
	payloadTagUndelegate  = 0x13
	payloadTagValidator   = 0x14
	payloadTagGovProposal = 0x15
	payloadTagGovVote     = 0x16
	payloadTagDeploy      = 0x17
	payloadTagCall        = 0x18
)

// TransferPayload is the body of a transfer transaction.
type TransferPayload struct {
	To     Address
	Amount uint64
}

// Encode returns the canonical payload bytes.
func (p *TransferPayload) Encode() []byte {
	w := zencoding.NewWriter()
	w.U8(payloadTagTransfer)
	w.Raw(p.To[:])
	w.U64(p.Amount)
	return w.Done()
}

// DecodeTransferPayload parses and validates a transfer payload.
func DecodeTransferPayload(b []byte) (*TransferPayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != payloadTagTransfer {
		return nil, fmt.Errorf("%w: not a transfer payload", ErrTxPayload)
	}
	p := &TransferPayload{}
	to := r.Raw(20)
	copy(p.To[:], to)
	p.Amount = r.U64()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if p.Amount == 0 {
		return nil, ErrZeroAmount
	}
	if p.To.IsZero() {
		return nil, ErrInvalidAddress
	}
	return p, nil
}

// StakePayload is the body of delegate/undelegate transactions.
type StakePayload struct {
	Validator Address
	Amount    uint64
	tag       uint8
}

// Encode returns the canonical payload bytes.
func (p *StakePayload) Encode() []byte {
	w := zencoding.NewWriter()
	w.U8(p.tag)
	w.Raw(p.Validator[:])
	w.U64(p.Amount)
	return w.Done()
}

// NewDelegatePayload builds a delegation payload.
func NewDelegatePayload(v Address, amount uint64) *StakePayload {
	return &StakePayload{Validator: v, Amount: amount, tag: payloadTagDelegate}
}

// NewUndelegatePayload builds an undelegation payload.
func NewUndelegatePayload(v Address, amount uint64) *StakePayload {
	return &StakePayload{Validator: v, Amount: amount, tag: payloadTagUndelegate}
}

func decodeStakePayload(b []byte, tag uint8) (*StakePayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != tag {
		return nil, fmt.Errorf("%w: wrong stake payload tag", ErrTxPayload)
	}
	p := &StakePayload{tag: tag}
	v := r.Raw(20)
	copy(p.Validator[:], v)
	p.Amount = r.U64()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if p.Amount == 0 {
		return nil, ErrZeroAmount
	}
	if p.Validator.IsZero() {
		return nil, ErrInvalidAddress
	}
	return p, nil
}

// DecodeDelegatePayload parses a delegation payload.
func DecodeDelegatePayload(b []byte) (*StakePayload, error) {
	return decodeStakePayload(b, payloadTagDelegate)
}

// DecodeUndelegatePayload parses an undelegation payload.
func DecodeUndelegatePayload(b []byte) (*StakePayload, error) {
	return decodeStakePayload(b, payloadTagUndelegate)
}

// ValidatorRegisterPayload registers validator metadata on chain.
type ValidatorRegisterPayload struct {
	PubKey        PublicKey
	Moniker       string
	CommissionBps uint16
}

// Encode returns the canonical payload bytes.
func (p *ValidatorRegisterPayload) Encode() []byte {
	w := zencoding.NewWriter()
	w.U8(payloadTagValidator)
	w.Raw(p.PubKey[:])
	w.Str(p.Moniker)
	w.U16(p.CommissionBps)
	return w.Done()
}

// DecodeValidatorRegisterPayload parses and validates a validator registration.
func DecodeValidatorRegisterPayload(b []byte) (*ValidatorRegisterPayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != payloadTagValidator {
		return nil, fmt.Errorf("%w: not a validator payload", ErrTxPayload)
	}
	p := &ValidatorRegisterPayload{}
	pub := r.Raw(32)
	copy(p.PubKey[:], pub)
	p.Moniker = r.Str()
	p.CommissionBps = r.U16()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if p.PubKey.IsZero() {
		return nil, fmt.Errorf("%w: empty validator key", ErrStakeInvalid)
	}
	if len(p.Moniker) > MaxMonikerLen {
		return nil, fmt.Errorf("%w: moniker too long", ErrStakeInvalid)
	}
	if p.CommissionBps > 10000 {
		return nil, fmt.Errorf("%w: commission above 100%%", ErrStakeInvalid)
	}
	return p, nil
}

// GovProposalKind enumerates proposal kinds.
type GovProposalKind uint8

// Proposal kinds.
const (
	// GovProposalText is a signalling proposal with no on-chain effect.
	GovProposalText GovProposalKind = 1
	// GovProposalParam changes a whitelisted chain parameter.
	GovProposalParam GovProposalKind = 2
)

// GovProposalPayload submits a governance proposal.
type GovProposalPayload struct {
	Title       string
	Description string
	Kind        GovProposalKind
	ParamKey    string
	ParamValue  uint64
	Deposit     uint64
}

// Encode returns the canonical payload bytes.
func (p *GovProposalPayload) Encode() []byte {
	w := zencoding.NewWriter()
	w.U8(payloadTagGovProposal)
	w.Str(p.Title)
	w.Str(p.Description)
	w.U8(uint8(p.Kind))
	w.Str(p.ParamKey)
	w.U64(p.ParamValue)
	w.U64(p.Deposit)
	return w.Done()
}

// DecodeGovProposalPayload parses and validates a proposal payload.
func DecodeGovProposalPayload(b []byte) (*GovProposalPayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != payloadTagGovProposal {
		return nil, fmt.Errorf("%w: not a proposal payload", ErrTxPayload)
	}
	p := &GovProposalPayload{}
	p.Title = r.Str()
	p.Description = r.Str()
	p.Kind = GovProposalKind(r.U8())
	p.ParamKey = r.Str()
	p.ParamValue = r.U64()
	p.Deposit = r.U64()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if len(p.Title) == 0 || len(p.Title) > MaxProposalFieldLen || len(p.Description) > MaxProposalFieldLen*4 {
		return nil, fmt.Errorf("%w: title/description out of bounds", ErrGovInvalid)
	}
	if p.Kind != GovProposalText && p.Kind != GovProposalParam {
		return nil, fmt.Errorf("%w: unknown proposal kind %d", ErrGovInvalid, p.Kind)
	}
	if p.Kind == GovProposalText && p.ParamKey != "" {
		return nil, fmt.Errorf("%w: text proposal must not set a parameter", ErrGovInvalid)
	}
	if p.Kind == GovProposalParam {
		if _, ok := ParamByKey(p.ParamKey); !ok {
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrGovInvalid, p.ParamKey)
		}
	}
	return p, nil
}

// VoteOption enumerates governance vote choices.
type VoteOption uint8

// Vote options.
const (
	VoteYes     VoteOption = 1
	VoteNo      VoteOption = 2
	VoteAbstain VoteOption = 3
)

// String returns a stable name.
func (v VoteOption) String() string {
	switch v {
	case VoteYes:
		return "yes"
	case VoteNo:
		return "no"
	case VoteAbstain:
		return "abstain"
	default:
		return "invalid"
	}
}

// GovVotePayload votes on a proposal.
type GovVotePayload struct {
	ProposalID uint64
	Option     VoteOption
}

// Encode returns the canonical payload bytes.
func (p *GovVotePayload) Encode() []byte {
	w := zencoding.NewWriter()
	w.U8(payloadTagGovVote)
	w.U64(p.ProposalID)
	w.U8(uint8(p.Option))
	return w.Done()
}

// DecodeGovVotePayload parses and validates a vote payload.
func DecodeGovVotePayload(b []byte) (*GovVotePayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != payloadTagGovVote {
		return nil, fmt.Errorf("%w: not a vote payload", ErrTxPayload)
	}
	p := &GovVotePayload{}
	p.ProposalID = r.U64()
	p.Option = VoteOption(r.U8())
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if p.ProposalID == 0 {
		return nil, fmt.Errorf("%w: proposal id must be non-zero", ErrGovInvalid)
	}
	if p.Option != VoteYes && p.Option != VoteNo && p.Option != VoteAbstain {
		return nil, fmt.Errorf("%w: unknown vote option", ErrGovInvalid)
	}
	return p, nil
}

// ContractDeployPayload deploys contract code.
type ContractDeployPayload struct {
	Code     []byte
	InitData []byte
}

// Encode returns the canonical payload bytes.
func (p *ContractDeployPayload) Encode() []byte {
	w := zencoding.NewWriterSize(len(p.Code) + len(p.InitData) + 16)
	w.U8(payloadTagDeploy)
	w.Bytes(p.Code)
	w.Bytes(p.InitData)
	return w.Done()
}

// DecodeContractDeployPayload parses and validates a deployment payload.
func DecodeContractDeployPayload(b []byte) (*ContractDeployPayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != payloadTagDeploy {
		return nil, fmt.Errorf("%w: not a deploy payload", ErrTxPayload)
	}
	p := &ContractDeployPayload{}
	p.Code = append([]byte(nil), r.Bytes(MaxPayloadLen)...)
	p.InitData = append([]byte(nil), r.Bytes(MaxPayloadLen)...)
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if len(p.Code) == 0 {
		return nil, fmt.Errorf("%w: empty bytecode", ErrContractInvalid)
	}
	return p, nil
}

// ContractCallPayload invokes a contract.
type ContractCallPayload struct {
	Contract Address
	Input    []byte
}

// Encode returns the canonical payload bytes.
func (p *ContractCallPayload) Encode() []byte {
	w := zencoding.NewWriterSize(len(p.Input) + 32)
	w.U8(payloadTagCall)
	w.Raw(p.Contract[:])
	w.Bytes(p.Input)
	return w.Done()
}

// DecodeContractCallPayload parses and validates a call payload.
func DecodeContractCallPayload(b []byte) (*ContractCallPayload, error) {
	r := zencoding.NewReader(b)
	if r.U8() != payloadTagCall {
		return nil, fmt.Errorf("%w: not a call payload", ErrTxPayload)
	}
	p := &ContractCallPayload{}
	c := r.Raw(20)
	copy(p.Contract[:], c)
	p.Input = append([]byte(nil), r.Bytes(MaxPayloadLen)...)
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	if p.Contract.IsZero() {
		return nil, fmt.Errorf("%w: empty contract address", ErrContractInvalid)
	}
	return p, nil
}

// validatePayloadShape checks that the payload parses as its declared type.
func (tx *Tx) validatePayloadShape() error {
	var err error
	switch tx.Type {
	case TxTransfer:
		var p *TransferPayload
		p, err = DecodeTransferPayload(tx.Payload)
		if err == nil && p.To == tx.Sender {
			// Self-transfer is legal but useless; allow it (fees are still paid).
			_ = p
		}
	case TxDelegate:
		_, err = DecodeDelegatePayload(tx.Payload)
	case TxUndelegate:
		_, err = DecodeUndelegatePayload(tx.Payload)
	case TxValidatorRegister:
		var p *ValidatorRegisterPayload
		p, err = DecodeValidatorRegisterPayload(tx.Payload)
		if err == nil && p.PubKey.Address() != tx.Sender {
			// The registering account must own the validator key it claims.
			err = fmt.Errorf("%w: validator key not owned by sender", ErrStakeInvalid)
		}
	case TxGovProposal:
		_, err = DecodeGovProposalPayload(tx.Payload)
	case TxGovVote:
		_, err = DecodeGovVotePayload(tx.Payload)
	case TxContractDeploy:
		_, err = DecodeContractDeployPayload(tx.Payload)
	case TxContractCall:
		_, err = DecodeContractCallPayload(tx.Payload)
	default:
		err = fmt.Errorf("%w: %d", ErrTxType, uint8(tx.Type))
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTxPayload, err)
	}
	return nil
}

// ContractAddress derives the deterministic address of a deployed contract:
//
//	addr = Digest("ZNET/v1/contract-address", sender, nonce)[12:32]
//
// The nonce makes redeployment of identical bytecode produce a distinct address.
func ContractAddress(sender Address, nonce uint64) Address {
	w := zencoding.NewWriter()
	w.Raw(sender[:])
	w.U64(nonce)
	d := zcrypto.Digest(domainContract, w.Done())
	var a Address
	copy(a[:], d[12:32])
	return a
}

// GovernanceID derives a deterministic identifier used to bind votes.
func GovernanceID(proposalID uint64, voter Address) Hash {
	w := zencoding.NewWriter()
	w.U64(proposalID)
	w.Raw(voter[:])
	return zcrypto.Digest(domainGovernance, w.Done())
}
