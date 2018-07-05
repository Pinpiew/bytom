package api

import (
	"context"
	"encoding/hex"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/account"
	"github.com/bytom/blockchain/query"
	"github.com/bytom/blockchain/signers"
	"github.com/bytom/consensus"
	"github.com/bytom/crypto/ed25519/chainkd"
	chainjson "github.com/bytom/encoding/json"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/types"
)

// POST /list-accounts
func (a *API) listAccounts(ctx context.Context, filter struct {
	ID string `json:"id"`
}) Response {
	accounts, err := a.wallet.AccountMgr.ListAccounts(filter.ID)
	if err != nil {
		log.Errorf("listAccounts: %v", err)
		return NewErrorResponse(err)
	}

	annotatedAccounts := []query.AnnotatedAccount{}
	for _, acc := range accounts {
		annotatedAccounts = append(annotatedAccounts, *account.Annotated(acc))
	}

	return NewSuccessResponse(annotatedAccounts)
}

// POST /get-asset
func (a *API) getAsset(ctx context.Context, filter struct {
	ID string `json:"id"`
}) Response {
	asset, err := a.wallet.AssetReg.GetAsset(filter.ID)
	if err != nil {
		log.Errorf("getAsset: %v", err)
		return NewErrorResponse(err)
	}

	return NewSuccessResponse(asset)
}

// POST /list-assets
func (a *API) listAssets(ctx context.Context, filter struct {
	ID string `json:"id"`
}) Response {
	assets, err := a.wallet.AssetReg.ListAssets(filter.ID)
	if err != nil {
		log.Errorf("listAssets: %v", err)
		return NewErrorResponse(err)
	}

	return NewSuccessResponse(assets)
}

// POST /list-balances
func (a *API) listBalances(ctx context.Context) Response {
	balances, err := a.wallet.GetAccountBalances("")
	if err != nil {
		return NewErrorResponse(err)
	}
	return NewSuccessResponse(balances)
}

// POST /get-transaction
func (a *API) getTransaction(ctx context.Context, txInfo struct {
	TxID string `json:"tx_id"`
}) Response {
	var annotatedTx *query.AnnotatedTx
	var err error

	annotatedTx, err = a.wallet.GetTransactionByTxID(txInfo.TxID)
	if err != nil {
		// transaction not found in blockchain db, search it from unconfirmed db
		annotatedTx, err = a.wallet.GetUnconfirmedTxByTxID(txInfo.TxID)
		if err != nil {
			return NewErrorResponse(err)
		}
	}

	return NewSuccessResponse(annotatedTx)
}

// POST /list-transactions
func (a *API) listTransactions(ctx context.Context, filter struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Detail      bool   `json:"detail"`
	Unconfirmed bool   `json:"unconfirmed"`
}) Response {
	transactions := []*query.AnnotatedTx{}
	var err error
	var transaction *query.AnnotatedTx

	if filter.ID != "" {
		transaction, err = a.wallet.GetTransactionByTxID(filter.ID)
		if err != nil && filter.Unconfirmed {
			transaction, err = a.wallet.GetUnconfirmedTxByTxID(filter.ID)
			if err != nil {
				return NewErrorResponse(err)
			}
		}
		transactions = []*query.AnnotatedTx{transaction}
	} else {
		transactions, err = a.wallet.GetTransactions(filter.AccountID)
		if err != nil {
			return NewErrorResponse(err)
		}

		if filter.Unconfirmed {
			unconfirmedTxs, err := a.wallet.GetUnconfirmedTxs(filter.AccountID)
			if err != nil {
				return NewErrorResponse(err)
			}
			transactions = append(unconfirmedTxs, transactions...)
		}
	}

	if filter.Detail == false {
		txSummary := a.wallet.GetTransactionsSummary(transactions)
		return NewSuccessResponse(txSummary)
	}
	return NewSuccessResponse(transactions)
}

// POST /get-unconfirmed-transaction
func (a *API) getUnconfirmedTx(ctx context.Context, filter struct {
	TxID chainjson.HexBytes `json:"tx_id"`
}) Response {
	var tmpTxID [32]byte
	copy(tmpTxID[:], filter.TxID[:])

	txHash := bc.NewHash(tmpTxID)
	txPool := a.chain.GetTxPool()
	txDesc, err := txPool.GetTransaction(&txHash)
	if err != nil {
		return NewErrorResponse(err)
	}

	tx := &BlockTx{
		ID:         txDesc.Tx.ID,
		Version:    txDesc.Tx.Version,
		Size:       txDesc.Tx.SerializedSize,
		TimeRange:  txDesc.Tx.TimeRange,
		Inputs:     []*query.AnnotatedInput{},
		Outputs:    []*query.AnnotatedOutput{},
		StatusFail: false,
	}

	for i := range txDesc.Tx.Inputs {
		tx.Inputs = append(tx.Inputs, a.wallet.BuildAnnotatedInput(txDesc.Tx, uint32(i)))
	}
	for i := range txDesc.Tx.Outputs {
		tx.Outputs = append(tx.Outputs, a.wallet.BuildAnnotatedOutput(txDesc.Tx, i))
	}

	return NewSuccessResponse(tx)
}

type unconfirmedTxsResp struct {
	Total uint64    `json:"total"`
	TxIDs []bc.Hash `json:"tx_ids"`
}

// POST /list-unconfirmed-transactions
func (a *API) listUnconfirmedTxs(ctx context.Context) Response {
	txIDs := []bc.Hash{}

	txPool := a.chain.GetTxPool()
	txs := txPool.GetTransactions()
	for _, txDesc := range txs {
		txIDs = append(txIDs, bc.Hash(txDesc.Tx.ID))
	}

	return NewSuccessResponse(&unconfirmedTxsResp{
		Total: uint64(len(txIDs)),
		TxIDs: txIDs,
	})
}

// RawTx is the tx struct for getRawTransaction
type RawTx struct {
	Version   uint64                   `json:"version"`
	Size      uint64                   `json:"size"`
	TimeRange uint64                   `json:"time_range"`
	Inputs    []*query.AnnotatedInput  `json:"inputs"`
	Outputs   []*query.AnnotatedOutput `json:"outputs"`
	Fee       int64                    `json:"fee"`
}

// POST /decode-raw-transaction
func (a *API) decodeRawTransaction(ctx context.Context, ins struct {
	Tx types.Tx `json:"raw_transaction"`
}) Response {
	tx := &RawTx{
		Version:   ins.Tx.Version,
		Size:      ins.Tx.SerializedSize,
		TimeRange: ins.Tx.TimeRange,
		Inputs:    []*query.AnnotatedInput{},
		Outputs:   []*query.AnnotatedOutput{},
	}

	for i := range ins.Tx.Inputs {
		tx.Inputs = append(tx.Inputs, a.wallet.BuildAnnotatedInput(&ins.Tx, uint32(i)))
	}
	for i := range ins.Tx.Outputs {
		tx.Outputs = append(tx.Outputs, a.wallet.BuildAnnotatedOutput(&ins.Tx, i))
	}

	totalInputBtm := uint64(0)
	totalOutputBtm := uint64(0)
	for _, input := range tx.Inputs {
		if input.AssetID.String() == consensus.BTMAssetID.String() {
			totalInputBtm += input.Amount
		}
	}

	for _, output := range tx.Outputs {
		if output.AssetID.String() == consensus.BTMAssetID.String() {
			totalOutputBtm += output.Amount
		}
	}

	tx.Fee = int64(totalInputBtm) - int64(totalOutputBtm)
	return NewSuccessResponse(tx)
}

// POST /list-unspent-outputs
func (a *API) listUnspentOutputs(ctx context.Context, filter struct {
	ID            string `json:"id"`
	SmartContract bool   `json:"smart_contract"`
}) Response {
	accountUTXOs := a.wallet.GetAccountUTXOs(filter.ID, filter.SmartContract)

	UTXOs := []query.AnnotatedUTXO{}
	for _, utxo := range accountUTXOs {
		UTXOs = append([]query.AnnotatedUTXO{{
			AccountID:           utxo.AccountID,
			OutputID:            utxo.OutputID.String(),
			SourceID:            utxo.SourceID.String(),
			AssetID:             utxo.AssetID.String(),
			Amount:              utxo.Amount,
			SourcePos:           utxo.SourcePos,
			Program:             fmt.Sprintf("%x", utxo.ControlProgram),
			ControlProgramIndex: utxo.ControlProgramIndex,
			Address:             utxo.Address,
			ValidHeight:         utxo.ValidHeight,
			Alias:               a.wallet.AccountMgr.GetAliasByID(utxo.AccountID),
			AssetAlias:          a.wallet.AssetReg.GetAliasByID(utxo.AssetID.String()),
			Change:              utxo.Change,
		}}, UTXOs...)
	}

	return NewSuccessResponse(UTXOs)
}

// return gasRate
func (a *API) gasRate() Response {
	gasrate := map[string]int64{"gas_rate": consensus.VMGasRate}
	return NewSuccessResponse(gasrate)
}

// PubKeyInfo is structure of pubkey info
type PubKeyInfo struct {
	Pubkey string               `json:"pubkey"`
	Path   []chainjson.HexBytes `json:"derivation_path"`
}

// AccountPubkey is detail of account pubkey info
type AccountPubkey struct {
	RootXPub    chainkd.XPub `json:"root_xpub"`
	PubKeyInfos []PubKeyInfo `json:"pubkey_infos"`
}

// POST /list-pubkeys
func (a *API) listPubKeys(ctx context.Context, ins struct {
	AccountID string `json:"account_id"`
}) Response {
	account, err := a.wallet.AccountMgr.FindByID(ctx, ins.AccountID)
	if err != nil {
		return NewErrorResponse(err)
	}

	pubKeyInfos := []PubKeyInfo{}
	idx := a.wallet.AccountMgr.GetContractIndex(ins.AccountID)
	for i := uint64(1); i <= idx; i++ {
		rawPath := signers.Path(account.Signer, signers.AccountKeySpace, i)
		derivedXPub := account.XPubs[0].Derive(rawPath)
		pubkey := derivedXPub.PublicKey()

		var path []chainjson.HexBytes
		for _, p := range rawPath {
			path = append(path, chainjson.HexBytes(p))
		}

		pubKeyInfos = append([]PubKeyInfo{{
			Pubkey: hex.EncodeToString(pubkey),
			Path:   path,
		}}, pubKeyInfos...)
	}

	return NewSuccessResponse(&AccountPubkey{
		RootXPub:    account.XPubs[0],
		PubKeyInfos: pubKeyInfos,
	})
}
