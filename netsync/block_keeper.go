package netsync

import (
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/errors"
	"github.com/bytom/p2p"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc/types"
)

const (
	maxKnownTxs    = 32768 // Maximum transactions hashes to keep in the known list (prevent DOS)
	maxKnownBlocks = 1024  // Maximum block hashes to keep in the known list (prevent DOS)

	syncTimeout        = 30 * time.Second
	requestRetryTicker = 15 * time.Second

	maxBlocksPending = 1024
	maxtxsPending    = 32768
	maxQuitReq       = 256
)

var (
	errGetBlockTimeout = errors.New("Get block Timeout")
	errPeerDropped     = errors.New("Peer dropped")
	errGetBlockByHash  = errors.New("Get block by hash error")
	errBroadcastStatus = errors.New("Broadcast new status block error")
	errReqBlock        = errors.New("Request block error")
)

//TODO: add retry mechanism
type blockKeeper struct {
	chain *protocol.Chain
	sw    *p2p.Switch
	peers *peerSet

	pendingProcessCh chan *blockPending
	txsProcessCh     chan *txsNotify
	quitReqBlockCh   chan *string
}

func newBlockKeeper(chain *protocol.Chain, sw *p2p.Switch, peers *peerSet, quitReqBlockCh chan *string) *blockKeeper {
	bk := &blockKeeper{
		chain:            chain,
		sw:               sw,
		peers:            peers,
		pendingProcessCh: make(chan *blockPending, maxBlocksPending),
		txsProcessCh:     make(chan *txsNotify, maxtxsPending),
		quitReqBlockCh:   quitReqBlockCh,
	}
	go bk.txsProcessWorker()
	return bk
}

func (bk *blockKeeper) AddBlock(block *types.Block, peerID string) {
	bk.pendingProcessCh <- &blockPending{block: block, peerID: peerID}
}

func (bk *blockKeeper) AddTx(tx *types.Tx, peerID string) {
	bk.txsProcessCh <- &txsNotify{tx: tx, peerID: peerID}
}

func (bk *blockKeeper) IsCaughtUp() bool {
	_, height := bk.peers.BestPeer()
	return bk.chain.BestBlockHeight() < height
}

func (bk *blockKeeper) BlockRequestWorker(peerID string, maxPeerHeight uint64) error {
	num := bk.chain.BestBlockHeight() + 1
	currentHash := bk.chain.BestBlockHash()
	orphanNum := uint64(0)
	reqNum := uint64(0)
	isOrphan := false
	peer := bk.peers.Peer(peerID)
	for num <= maxPeerHeight && num > 0 {
		if isOrphan {
			reqNum = orphanNum
		} else {
			reqNum = num
		}
		block, err := bk.BlockRequest(peerID, reqNum)
		if errors.Root(err) == errPeerDropped || errors.Root(err) == errGetBlockTimeout || errors.Root(err) == errReqBlock {
			log.WithField("Peer abnormality. PeerID: ", peerID).Info(err)
			if peer == nil {
				log.Info("peer is not registered")
				break
			}
			log.Info("Peer communication abnormality")
			if ban := peer.addBanScore(0, 50, "block request error"); ban {
				bk.sw.AddBannedPeer(peer.getPeer())
			}
			bk.sw.StopPeerGracefully(peer.getPeer())
			break
		}
		isOrphan, err = bk.chain.ProcessBlock(block)
		if err != nil {
			if ban := peer.addBanScore(50, 0, "block process error"); ban {
				bk.sw.AddBannedPeer(peer.getPeer())
				bk.sw.StopPeerGracefully(peer.getPeer())
			}
			log.WithField("hash:", block.Hash()).Errorf("blockKeeper fail process block %v ", err)
			break
		}
		if isOrphan {
			orphanNum = block.Height - 1
			continue
		}
		num++
	}
	bestHash := bk.chain.BestBlockHash()
	log.Info("Block sync complete. height:", bk.chain.BestBlockHeight(), " hash:", bestHash)
	if currentHash.String() != bestHash.String() {
		log.Info("Broadcast new chain status.")

		block, err := bk.chain.GetBlockByHash(bestHash)
		if err != nil {
			log.Errorf("Failed on sync complete broadcast status get block %v", err)
			return errGetBlockByHash
		}

		peers, err := bk.peers.BroadcastNewStatus(block)
		if err != nil {
			log.Errorf("Failed on broadcast new status block %v", err)
			return errBroadcastStatus
		}
		for _, peer := range peers {
			if ban := peer.addBanScore(0, 50, "Broadcast block error"); ban {
				peer := bk.peers.Peer(peer.id).getPeer()
				bk.sw.AddBannedPeer(peer)
				bk.sw.StopPeerGracefully(peer)
			}
		}
	}
	return nil
}

func (bk *blockKeeper) blockRequest(peerID string, height uint64) error {
	return bk.peers.requestBlockByHeight(peerID, height)
}

func (bk *blockKeeper) BlockRequest(peerID string, height uint64) (*types.Block, error) {
	var block *types.Block

	if err := bk.blockRequest(peerID, height); err != nil {
		return nil, errReqBlock
	}
	retryTicker := time.Tick(requestRetryTicker)
	syncWait := time.NewTimer(syncTimeout)

	for {
		select {
		case pendingResponse := <-bk.pendingProcessCh:
			block = pendingResponse.block
			if pendingResponse.peerID != peerID {
				log.Warning("From different peer")
				continue
			}
			if block.Height != height {
				log.Warning("Block height error")
				continue
			}
			return block, nil
		case <-retryTicker:
			if err := bk.blockRequest(peerID, height); err != nil {
				return nil, errReqBlock
			}
		case <-syncWait.C:
			log.Warning("Request block timeout")
			return nil, errGetBlockTimeout
		case peerid := <-bk.quitReqBlockCh:
			if *peerid == peerID {
				log.Info("Quite block request worker")
				return nil, errPeerDropped
			}
		}
	}
}

func (bk *blockKeeper) txsProcessWorker() {
	for txsResponse := range bk.txsProcessCh {
		tx := txsResponse.tx
		peer:=bk.peers.Peer(txsResponse.peerID)
		log.Info("Receive new tx from remote peer. TxID:", tx.ID.String())
		bk.peers.MarkTransaction(txsResponse.peerID, &tx.ID)
		if isOrphan, err := bk.chain.ValidateTx(tx); err != nil && isOrphan == false {
			if ban := peer.addBanScore(50, 0, "tx error"); ban {
				bk.sw.AddBannedPeer(peer.getPeer())
				bk.sw.StopPeerGracefully(peer.getPeer())
			}
		}
	}
}