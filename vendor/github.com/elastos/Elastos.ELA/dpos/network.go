package dpos

import (
	"encoding/hex"
	"errors"
	"sync"

	"github.com/elastos/Elastos.ELA/blockchain"
	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/common/config"
	"github.com/elastos/Elastos.ELA/core/types"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/log"
	"github.com/elastos/Elastos.ELA/dpos/manager"
	"github.com/elastos/Elastos.ELA/dpos/p2p"
	"github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/dpos/store"
	elap2p "github.com/elastos/Elastos.ELA/p2p"
	elamsg "github.com/elastos/Elastos.ELA/p2p/msg"
)

type PeerItem struct {
	Address     p2p.PeerAddr
	NeedConnect bool
	Peer        *peer.Peer
}

type blockItem struct {
	Block     *types.Block
	Confirmed bool
}

type messageItem struct {
	ID      peer.PID
	Message elap2p.Message
}

type network struct {
	listener           manager.NetworkEventListener
	account            account.DposAccount
	proposalDispatcher *manager.ProposalDispatcher
	peersLock          sync.Mutex
	store              store.IDposStore
	publicKey          []byte

	p2pServer    p2p.Server
	messageQueue chan *messageItem
	quit         chan bool

	changeViewChan           chan bool
	blockReceivedChan        chan blockItem
	confirmReceivedChan      chan *payload.Confirm
	illegalBlocksEvidence    chan *payload.DPOSIllegalBlocks
	sidechainIllegalEvidence chan *payload.SidechainIllegalData
}

func (n *network) Initialize(dnConfig manager.DPOSNetworkConfig) {
	n.proposalDispatcher = dnConfig.ProposalDispatcher
	n.store = dnConfig.Store
	n.publicKey = dnConfig.PublicKey
}

func (n *network) Start() {
	n.p2pServer.Start()

	if err := n.UpdatePeers(blockchain.DefaultLedger.Arbitrators.
		GetNeedConnectArbiters(blockchain.DefaultLedger.Blockchain.
			GetHeight())); err != nil {
		log.Error(err)
	}

	go func() {
	out:
		for {
			select {
			case msgItem := <-n.messageQueue:
				n.processMessage(msgItem)
			case <-n.changeViewChan:
				n.changeView()
			case blockItem := <-n.blockReceivedChan:
				n.blockReceived(blockItem.Block, blockItem.Confirmed)
			case confirm := <-n.confirmReceivedChan:
				n.confirmReceived(confirm)
			case evidence := <-n.illegalBlocksEvidence:
				n.illegalBlocksReceived(evidence)
			case sidechainEvidence := <-n.sidechainIllegalEvidence:
				n.sidechainIllegalEvidenceReceived(sidechainEvidence)
			case <-n.quit:
				break out
			}
		}
	}()
}

func (n *network) getProducersConnectionInfo() (result map[string]p2p.PeerAddr) {
	result = make(map[string]p2p.PeerAddr)
	crcs := blockchain.DefaultLedger.Arbitrators.GetCRCArbitrators()
	for k, v := range crcs {
		pid := peer.PID{}
		copy(pid[:], v.NodePublicKey())
		result[k] = p2p.PeerAddr{PID: pid, Addr: v.Info().NetAddress}
	}

	producers := blockchain.DefaultLedger.Blockchain.GetState().GetActiveProducers()
	for _, p := range producers {
		if len(p.Info().NodePublicKey) != 33 {
			log.Warn("[getProducersConnectionInfo] invalid public key")
			continue
		}
		pid := peer.PID{}
		copy(pid[:], p.Info().NodePublicKey)
		result[hex.EncodeToString(p.Info().NodePublicKey)] =
			p2p.PeerAddr{PID: pid, Addr: p.Info().NetAddress}
	}

	return result
}

func (n *network) Stop() error {
	n.quit <- true
	return n.p2pServer.Stop()
}

func (n *network) UpdatePeers(peers map[string]*p2p.PeerAddr) error {
	log.Info("[UpdatePeers] peers:", len(peers), " height: ",
		blockchain.DefaultLedger.Blockchain.GetHeight())

	if p, _ := peers[common.BytesToHexString(n.publicKey)]; p == nil {
		return errors.New("self not in direct peers list")
	}

	var peerList []p2p.PeerAddr
	for k, v := range peers {
		if v == nil {
			log.Info("peer[", k, "] address empty")
			continue
		}
		log.Info("peer[", k, "] addr:", v.Addr, " pid:",
			common.BytesToHexString(v.PID[:]))
		peerList = append(peerList, *v)
	}
	n.p2pServer.ConnectPeers(peerList)

	return nil
}

func (n *network) SendMessageToPeer(id peer.PID, msg elap2p.Message) error {
	return n.p2pServer.SendMessageToPeer(id, msg)
}

func (n *network) BroadcastMessage(msg elap2p.Message) {
	log.Info("[BroadcastMessage] msg:", msg.CMD())
	n.p2pServer.BroadcastMessage(msg)
}

func (n *network) GetActivePeers() []p2p.Peer {
	return n.p2pServer.ConnectedPeers()
}

func (n *network) PostChangeViewTask() {
	n.changeViewChan <- true
}

func (n *network) PostBlockReceivedTask(b *types.Block, confirmed bool) {
	n.blockReceivedChan <- blockItem{b, confirmed}
}

func (n *network) PostIllegalBlocksTask(i *payload.DPOSIllegalBlocks) {
	n.illegalBlocksEvidence <- i
}

func (n *network) PostSidechainIllegalDataTask(s *payload.SidechainIllegalData) {
	n.sidechainIllegalEvidence <- s
}

func (n *network) PostConfirmReceivedTask(p *payload.Confirm) {
	n.confirmReceivedChan <- p
}

func (n *network) notifyFlag(flag p2p.NotifyFlag) {
	if flag == p2p.NFBadNetwork {
		n.listener.OnBadNetwork()
	}
}

func (n *network) handleMessage(pid peer.PID, msg elap2p.Message) {
	n.messageQueue <- &messageItem{pid, msg}
}

func (n *network) processMessage(msgItem *messageItem) {
	m := msgItem.Message
	switch m.CMD() {
	case msg.CmdReceivedProposal:
		msgProposal, processed := m.(*msg.Proposal)
		if processed {
			n.listener.OnProposalReceived(msgItem.ID, &msgProposal.Proposal)
		}
	case msg.CmdAcceptVote:
		msgVote, processed := m.(*msg.Vote)
		if processed {
			n.listener.OnVoteAccepted(msgItem.ID, &msgVote.Vote)
		}
	case msg.CmdRejectVote:
		msgVote, processed := m.(*msg.Vote)
		if processed {
			n.listener.OnVoteRejected(msgItem.ID, &msgVote.Vote)
		}
	case msg.CmdPing:
		msgPing, processed := m.(*msg.Ping)
		if processed {
			n.listener.OnPing(msgItem.ID, uint32(msgPing.Nonce))
		}
	case msg.CmdPong:
		msgPong, processed := m.(*msg.Pong)
		if processed {
			n.listener.OnPong(msgItem.ID, uint32(msgPong.Nonce))
		}
	case elap2p.CmdBlock:
		msgBlock, processed := m.(*elamsg.Block)
		if processed {
			if block, ok := msgBlock.Serializable.(*types.Block); ok {
				n.listener.OnBlock(msgItem.ID, block)
			}
		}
	case msg.CmdInv:
		msgInv, processed := m.(*msg.Inventory)
		if processed {
			n.listener.OnInv(msgItem.ID, msgInv.BlockHash)
		}
	case msg.CmdGetBlock:
		msgGetBlock, processed := m.(*msg.GetBlock)
		if processed {
			n.listener.OnGetBlock(msgItem.ID, msgGetBlock.BlockHash)
		}
	case msg.CmdGetBlocks:
		msgGetBlocks, processed := m.(*msg.GetBlocks)
		if processed {
			n.listener.OnGetBlocks(msgItem.ID, msgGetBlocks.StartBlockHeight, msgGetBlocks.EndBlockHeight)
		}
	case msg.CmdResponseBlocks:
		msgResponseBlocks, processed := m.(*msg.ResponseBlocks)
		if processed {
			n.listener.OnResponseBlocks(msgItem.ID, msgResponseBlocks.BlockConfirms)
		}
	case msg.CmdRequestConsensus:
		msgRequestConsensus, processed := m.(*msg.RequestConsensus)
		if processed {
			n.listener.OnRequestConsensus(msgItem.ID, msgRequestConsensus.Height)
		}
	case msg.CmdResponseConsensus:
		msgResponseConsensus, processed := m.(*msg.ResponseConsensus)
		if processed {
			n.listener.OnResponseConsensus(msgItem.ID, &msgResponseConsensus.Consensus)
		}
	case msg.CmdRequestProposal:
		msgRequestProposal, processed := m.(*msg.RequestProposal)
		if processed {
			n.listener.OnRequestProposal(msgItem.ID, msgRequestProposal.ProposalHash)
		}
	case msg.CmdIllegalProposals:
		msgIllegalProposals, processed := m.(*msg.IllegalProposals)
		if processed {
			n.listener.OnIllegalProposalReceived(msgItem.ID, &msgIllegalProposals.Proposals)
		}
	case msg.CmdIllegalVotes:
		msgIllegalVotes, processed := m.(*msg.IllegalVotes)
		if processed {
			n.listener.OnIllegalVotesReceived(msgItem.ID, &msgIllegalVotes.Votes)
		}
	case msg.CmdSidechainIllegalData:
		msgSidechainIllegal, processed := m.(*msg.SidechainIllegalData)
		if processed {
			n.listener.OnSidechainIllegalEvidenceReceived(&msgSidechainIllegal.Data)
		}
	case elap2p.CmdTx:
		msgTx, processed := m.(*elamsg.Tx)
		if processed {
			if tx, ok := msgTx.Serializable.(*types.Transaction); ok && tx.IsInactiveArbitrators() {
				n.listener.OnInactiveArbitratorsReceived(tx)
			}
		}
	case msg.CmdResponseInactiveArbitrators:
		msgResponse, processed := m.(*msg.ResponseInactiveArbitrators)
		if processed {
			n.listener.OnResponseInactiveArbitratorsReceived(
				&msgResponse.TxHash, msgResponse.Signer, msgResponse.Sign)
		}
	}
}

func (n *network) changeView() {
	n.listener.OnChangeView()
}

func (n *network) blockReceived(b *types.Block, confirmed bool) {
	n.listener.OnBlockReceived(b, confirmed)
}

func (n *network) confirmReceived(p *payload.Confirm) {
	n.listener.OnConfirmReceived(p)
}

func (n *network) illegalBlocksReceived(i *payload.DPOSIllegalBlocks) {
	n.listener.OnIllegalBlocksTxReceived(i)
}

func (n *network) sidechainIllegalEvidenceReceived(
	s *payload.SidechainIllegalData) {
	n.BroadcastMessage(&msg.SidechainIllegalData{Data: *s})
	n.listener.OnSidechainIllegalEvidenceReceived(s)
}

func (n *network) getCurrentHeight(pid peer.PID) uint64 {
	return uint64(n.proposalDispatcher.CurrentHeight())
}

func NewDposNetwork(pid peer.PID, listener manager.NetworkEventListener,
	dposAccount account.DposAccount) (*network, error) {
	network := &network{
		listener:                 listener,
		messageQueue:             make(chan *messageItem, 10000), //todo config handle capacity though config file
		quit:                     make(chan bool),
		changeViewChan:           make(chan bool),
		blockReceivedChan:        make(chan blockItem, 10),        //todo config handle capacity though config file
		confirmReceivedChan:      make(chan *payload.Confirm, 10), //todo config handle capacity though config file
		illegalBlocksEvidence:    make(chan *payload.DPOSIllegalBlocks),
		sidechainIllegalEvidence: make(chan *payload.SidechainIllegalData),
		account:                  dposAccount,
	}

	notifier := p2p.NewNotifier(p2p.NFNetStabled|p2p.NFBadNetwork, network.notifyFlag)

	server, err := p2p.NewServer(&p2p.Config{
		PID:              pid,
		MagicNumber:      config.Parameters.ArbiterConfiguration.Magic,
		ProtocolVersion:  config.Parameters.ArbiterConfiguration.ProtocolVersion,
		Services:         config.Parameters.ArbiterConfiguration.Services,
		DefaultPort:      config.Parameters.ArbiterConfiguration.NodePort,
		MakeEmptyMessage: makeEmptyMessage,
		HandleMessage:    network.handleMessage,
		PingNonce:        network.getCurrentHeight,
		PongNonce:        network.getCurrentHeight,
		SignNonce:        dposAccount.SignPeerNonce,
		StateNotifier:    notifier,
	})
	if err != nil {
		return nil, err
	}

	network.p2pServer = server
	return network, nil
}

func makeEmptyMessage(cmd string) (message elap2p.Message, err error) {
	switch cmd {
	case elap2p.CmdBlock:
		message = elamsg.NewBlock(&types.Block{})
	case msg.CmdAcceptVote:
		message = &msg.Vote{Command: msg.CmdAcceptVote}
	case msg.CmdReceivedProposal:
		message = &msg.Proposal{}
	case msg.CmdRejectVote:
		message = &msg.Vote{Command: msg.CmdRejectVote}
	case msg.CmdInv:
		message = &msg.Inventory{}
	case msg.CmdGetBlock:
		message = &msg.GetBlock{}
	case msg.CmdGetBlocks:
		message = &msg.GetBlocks{}
	case msg.CmdResponseBlocks:
		message = &msg.ResponseBlocks{}
	case msg.CmdRequestConsensus:
		message = &msg.RequestConsensus{}
	case msg.CmdResponseConsensus:
		message = &msg.ResponseConsensus{}
	case msg.CmdRequestProposal:
		message = &msg.RequestProposal{}
	case msg.CmdIllegalProposals:
		message = &msg.IllegalProposals{}
	case msg.CmdIllegalVotes:
		message = &msg.IllegalVotes{}
	case msg.CmdSidechainIllegalData:
		message = &msg.SidechainIllegalData{}
	default:
		return nil, errors.New("Received unsupported message, CMD " + cmd)
	}
	return message, nil
}