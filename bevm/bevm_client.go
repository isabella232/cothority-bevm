package bevm

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"path"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"go.dedis.ch/cothority/v3/byzcoin"
	"go.dedis.ch/cothority/v3/darc"
	"go.dedis.ch/onet/v3/log"
	"go.dedis.ch/protobuf"
)

// WeiPerEther represents the number of Wei (the smallest currency denomination
// in Ethereum) in a single Ether.
const WeiPerEther = 1e18

// ---------------------------------------------------------------------------

// EvmContract is the abstraction for an Ethereum contract
type EvmContract struct {
	Abi      abi.ABI
	Bytecode []byte
	Address  common.Address
	name     string // For informational purposes only
}

// NewEvmContract creates a new EvmContract and fills its ABI and bytecode from
// the filesystem.
// 'filepath' represents the complete directory and name of the contract files,
// without the extensions.
func NewEvmContract(filepath string) (*EvmContract, error) {
	abiJSON, err := ioutil.ReadFile(filepath + ".abi")
	if err != nil {
		return nil, errors.New("Error reading contract ABI: " + err.Error())
	}

	contractAbi, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		return nil, errors.New("Error decoding contract ABI JSON: " + err.Error())
	}

	contractBytecode, err := ioutil.ReadFile(filepath + ".bin")
	if err != nil {
		return nil, errors.New("Error reading contract Bytecode: " + err.Error())
	}

	return &EvmContract{
		name:     path.Base(filepath),
		Abi:      contractAbi,
		Bytecode: common.Hex2Bytes(string(contractBytecode)),
	}, nil
}

func (contract EvmContract) String() string {
	return fmt.Sprintf("EvmContract[%s @%s]", contract.name, contract.Address.Hex())
}

func (contract EvmContract) packConstructor(args ...interface{}) ([]byte, error) {
	return contract.packMethod("", args...)
}

func (contract EvmContract) packMethod(method string, args ...interface{}) ([]byte, error) {
	return contract.Abi.Pack(method, args...)
}

func (contract EvmContract) unpackResult(result interface{}, method string, resultBytes []byte) error {
	return contract.Abi.Unpack(result, method, resultBytes)
}

// ---------------------------------------------------------------------------

// EvmAccount is the abstraction for an Ethereum account
type EvmAccount struct {
	Address    common.Address
	PrivateKey *ecdsa.PrivateKey
	Nonce      uint64
}

// NewEvmAccount creates a new EvmAccount
func NewEvmAccount(privateKey string) (*EvmAccount, error) {
	privKey, err := crypto.HexToECDSA(privateKey)
	if err != nil {
		return nil, err
	}

	address := crypto.PubkeyToAddress(privKey.PublicKey)

	return &EvmAccount{
		Address:    address,
		PrivateKey: privKey,
	}, nil
}

func (account EvmAccount) String() string {
	return fmt.Sprintf("EvmAccount[%s]", account.Address.Hex())
}

// SignAndMarshalTx signs an Ethereum transaction and returns it in byte
// format, ready to be included into a Byzcoin transaction
func (account EvmAccount) SignAndMarshalTx(tx *types.Transaction) ([]byte, error) {
	var signer types.Signer = types.HomesteadSigner{}

	signedTx, err := types.SignTx(tx, signer, account.PrivateKey)
	if err != nil {
		return nil, err
	}

	signedBuffer, err := signedTx.MarshalJSON()
	if err != nil {
		return nil, err
	}

	return signedBuffer, err
}

// ---------------------------------------------------------------------------

// Client is the abstraction for the ByzCoin EVM client
type Client struct {
	bcClient   *byzcoin.Client
	signer     darc.Signer
	instanceID byzcoin.InstanceID
}

// NewBEvm creates a new ByzCoin EVM instance
func NewBEvm(bcClient *byzcoin.Client, signer darc.Signer, gDarc *darc.Darc) (byzcoin.InstanceID, error) {
	instanceID := byzcoin.NewInstanceID(nil)

	counters, err := bcClient.GetSignerCounters(signer.Identity().String())
	if err != nil {
		return instanceID, err
	}

	ctx := byzcoin.ClientTransaction{
		Instructions: []byzcoin.Instruction{{
			InstanceID:    byzcoin.NewInstanceID(gDarc.GetBaseID()),
			SignerCounter: []uint64{counters.Counters[0] + 1},
			Spawn: &byzcoin.Spawn{
				ContractID: ContractBEvmID,
				Args:       byzcoin.Arguments{},
			},
		}},
	}

	err = ctx.FillSignersAndSignWith(signer)
	if err != nil {
		return instanceID, err
	}

	// Sending this transaction to ByzCoin does not directly include it in the
	// global state - first we must wait for the new block to be created.
	_, err = bcClient.AddTransactionAndWait(ctx, 5)
	if err != nil {
		return instanceID, err
	}

	instanceID = ctx.Instructions[0].DeriveID("")

	return instanceID, nil
}

// NewClient creates a new ByzCoin EVM client, connected to the given ByzCoin instance
func NewClient(bcClient *byzcoin.Client, signer darc.Signer, instanceID byzcoin.InstanceID) (*Client, error) {
	return &Client{
		bcClient:   bcClient,
		signer:     signer,
		instanceID: instanceID,
	}, nil
}

// Deploy deploys a new Ethereum contract on the EVM
func (client *Client) Deploy(gasLimit uint64, gasPrice *big.Int, amount uint64, account *EvmAccount, contract *EvmContract, args ...interface{}) error {
	log.Lvlf2(">>> Deploy EVM contract '%s'", contract.name)
	defer log.Lvlf2("<<< Deploy EVM contract '%s'", contract.name)

	packedArgs, err := contract.packConstructor(args...)
	if err != nil {
		return err
	}

	callData := append(contract.Bytecode, packedArgs...)
	tx := types.NewContractCreation(account.Nonce, big.NewInt(int64(amount)), gasLimit, gasPrice, callData)
	signedTxBuffer, err := account.SignAndMarshalTx(tx)
	if err != nil {
		return err
	}

	err = client.invoke("transaction", byzcoin.Arguments{
		{Name: "tx", Value: signedTxBuffer},
	})
	if err != nil {
		return err
	}

	contract.Address = crypto.CreateAddress(account.Address, account.Nonce)
	account.Nonce++

	return nil
}

// Transaction performs a new transaction (contract method call with state change) on the EVM
func (client *Client) Transaction(gasLimit uint64, gasPrice *big.Int, amount uint64, account *EvmAccount, contract *EvmContract, method string, args ...interface{}) error {
	log.Lvlf2(">>> EVM method '%s()' on %s", method, contract)
	defer log.Lvlf2("<<< EVM method '%s()' on %s", method, contract)

	callData, err := contract.packMethod(method, args...)
	if err != nil {
		return err
	}

	tx := types.NewTransaction(account.Nonce, contract.Address, big.NewInt(int64(amount)), gasLimit, gasPrice, callData)
	signedTxBuffer, err := account.SignAndMarshalTx(tx)
	if err != nil {
		return err
	}

	err = client.invoke("transaction", byzcoin.Arguments{
		{Name: "tx", Value: signedTxBuffer},
	})
	if err != nil {
		return err
	}

	account.Nonce++

	return nil
}

// Call performs a new call (contract view method call, without state change) on the EVM
func (client *Client) Call(account *EvmAccount, result interface{}, contract *EvmContract, method string, args ...interface{}) error {
	log.Lvlf2(">>> EVM view method '%s()' on %s", method, contract)
	defer log.Lvlf2("<<< EVM view method '%s()' on %s", method, contract)

	// Pack the method call and arguments
	callData, err := contract.packMethod(method, args...)
	if err != nil {
		return err
	}

	// Retrieve the EVM state
	stateDb, err := getEvmDb(client.bcClient, client.instanceID)
	if err != nil {
		return err
	}

	// Instantiate a new EVM
	evm := vm.NewEVM(getContext(), stateDb, getChainConfig(), getVMConfig())

	// Perform the call (1 Ether should be enough for everyone [tm]...)
	ret, _, err := evm.Call(vm.AccountRef(account.Address), contract.Address, callData, uint64(1*WeiPerEther), big.NewInt(0))
	if err != nil {
		return err
	}

	// Unpack the result into the caller's variable
	err = contract.unpackResult(&result, method, ret)
	if err != nil {
		return err
	}

	return nil
}

// CreditAccount credits the given Ethereum address with the given amount
func (client *Client) CreditAccount(amount *big.Int, address common.Address) error {
	err := client.invoke("credit", byzcoin.Arguments{
		{Name: "address", Value: address.Bytes()},
		{Name: "amount", Value: amount.Bytes()},
	})
	if err != nil {
		return err
	}

	log.Lvlf2("Credited %d wei on '%x'", amount, address)

	return nil
}

// GetAccountBalance returns the current balance of a Ethereum address
func (client *Client) GetAccountBalance(address common.Address) (*big.Int, error) {
	stateDb, err := getEvmDb(client.bcClient, client.instanceID)
	if err != nil {
		return nil, err
	}

	balance := stateDb.GetBalance(address)

	log.Lvlf2("Balance of '%x' is %d wei", address, balance)

	return balance, nil
}

// ---------------------------------------------------------------------------
// Helper functions

// Retrieve a read-only EVM state database from ByzCoin
func getEvmDb(bcClient *byzcoin.Client, instID byzcoin.InstanceID) (*state.StateDB, error) {
	// Retrieve the proof of the Byzcoin instance
	proofResponse, err := bcClient.GetProof(instID[:])
	if err != nil {
		return nil, err
	}

	// Validate the proof
	err = proofResponse.Proof.Verify(bcClient.ID)
	if err != nil {
		return nil, err
	}

	// Extract the value from the proof
	_, value, _, _, err := proofResponse.Proof.KeyValue()
	if err != nil {
		return nil, err
	}

	// Decode the proof value into an EVM State
	var bs State
	err = protobuf.Decode(value, &bs)
	if err != nil {
		return nil, err
	}

	// Create a client ByzDB instance
	byzDb, err := NewClientByzDatabase(instID, bcClient)
	if err != nil {
		return nil, err
	}

	db := state.NewDatabase(byzDb)

	return state.New(bs.RootHash, db)
}

// Invoke a method on a ByzCoin EVM instance
func (client *Client) invoke(command string, args byzcoin.Arguments) error {
	counters, err := client.bcClient.GetSignerCounters(client.signer.Identity().String())
	if err != nil {
		return err
	}

	ctx := byzcoin.ClientTransaction{
		Instructions: []byzcoin.Instruction{{
			InstanceID:    client.instanceID,
			SignerCounter: []uint64{counters.Counters[0] + 1},
			Invoke: &byzcoin.Invoke{
				ContractID: ContractBEvmID,
				Command:    command,
				Args:       args,
			},
		}},
	}

	err = ctx.FillSignersAndSignWith(client.signer)
	if err != nil {
		return err
	}

	// Sending this transaction to ByzCoin does not directly include it in the
	// global state - first we must wait for the new block to be created.
	_, err = client.bcClient.AddTransactionAndWait(ctx, 5)

	return err
}
