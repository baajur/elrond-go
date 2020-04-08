package spos

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	logger "github.com/ElrondNetwork/elrond-go-logger"
	"github.com/ElrondNetwork/elrond-go/consensus"
	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/check"
	"github.com/ElrondNetwork/elrond-go/crypto"
	"github.com/ElrondNetwork/elrond-go/data"
	"github.com/ElrondNetwork/elrond-go/data/block"
	"github.com/ElrondNetwork/elrond-go/hashing"
	"github.com/ElrondNetwork/elrond-go/marshal"
	"github.com/ElrondNetwork/elrond-go/ntp"
	"github.com/ElrondNetwork/elrond-go/p2p"
	"github.com/ElrondNetwork/elrond-go/process"
	"github.com/ElrondNetwork/elrond-go/sharding"
	"github.com/ElrondNetwork/elrond-go/statusHandler"
)

// Worker defines the data needed by spos to communicate between nodes which are in the validators group
type Worker struct {
	consensusService   ConsensusService
	blockChain         data.ChainHandler
	blockProcessor     process.BlockProcessor
	bootstrapper       process.Bootstrapper
	broadcastMessenger consensus.BroadcastMessenger
	consensusState     *ConsensusState
	forkDetector       process.ForkDetector
	keyGenerator       crypto.KeyGenerator
	marshalizer        marshal.Marshalizer
	hasher             hashing.Hasher
	rounder            consensus.Rounder
	shardCoordinator   sharding.Coordinator
	singleSigner       crypto.SingleSigner
	syncTimer          ntp.SyncTimer
	headerSigVerifier  RandSeedVerifier
	appStatusHandler   core.AppStatusHandler
	chainID            []byte

	networkShardingCollector consensus.NetworkShardingCollector

	receivedMessages      map[consensus.MessageType][]*consensus.Message
	receivedMessagesCalls map[consensus.MessageType]func(*consensus.Message) bool

	executeMessageChannel        chan *consensus.Message
	consensusStateChangedChannel chan bool

	mutReceivedMessages      sync.RWMutex
	mutReceivedMessagesCalls sync.RWMutex

	mapDisplayHashConsensusMessage map[string][]*consensus.Message
	mutDisplayHashConsensusMessage sync.RWMutex

	receivedHeadersHandlers   []func(headerHandler data.HeaderHandler)
	mutReceivedHeadersHandler sync.RWMutex

	antifloodHandler consensus.P2PAntifloodHandler
	poolAdder        PoolAdder
}

// WorkerArgs holds the consensus worker arguments
type WorkerArgs struct {
	ConsensusService         ConsensusService
	BlockChain               data.ChainHandler
	BlockProcessor           process.BlockProcessor
	Bootstrapper             process.Bootstrapper
	BroadcastMessenger       consensus.BroadcastMessenger
	ConsensusState           *ConsensusState
	ForkDetector             process.ForkDetector
	KeyGenerator             crypto.KeyGenerator
	Marshalizer              marshal.Marshalizer
	Hasher                   hashing.Hasher
	Rounder                  consensus.Rounder
	ShardCoordinator         sharding.Coordinator
	SingleSigner             crypto.SingleSigner
	SyncTimer                ntp.SyncTimer
	HeaderSigVerifier        RandSeedVerifier
	ChainID                  []byte
	NetworkShardingCollector consensus.NetworkShardingCollector
	AntifloodHandler         consensus.P2PAntifloodHandler
	PoolAdder                PoolAdder
}

// NewWorker creates a new Worker object
func NewWorker(args *WorkerArgs) (*Worker, error) {
	err := checkNewWorkerParams(
		args,
	)
	if err != nil {
		return nil, err
	}

	wrk := Worker{
		consensusService:         args.ConsensusService,
		blockChain:               args.BlockChain,
		blockProcessor:           args.BlockProcessor,
		bootstrapper:             args.Bootstrapper,
		broadcastMessenger:       args.BroadcastMessenger,
		consensusState:           args.ConsensusState,
		forkDetector:             args.ForkDetector,
		keyGenerator:             args.KeyGenerator,
		marshalizer:              args.Marshalizer,
		hasher:                   args.Hasher,
		rounder:                  args.Rounder,
		shardCoordinator:         args.ShardCoordinator,
		singleSigner:             args.SingleSigner,
		syncTimer:                args.SyncTimer,
		headerSigVerifier:        args.HeaderSigVerifier,
		chainID:                  args.ChainID,
		appStatusHandler:         statusHandler.NewNilStatusHandler(),
		networkShardingCollector: args.NetworkShardingCollector,
		antifloodHandler:         args.AntifloodHandler,
		poolAdder:                args.PoolAdder,
	}

	wrk.executeMessageChannel = make(chan *consensus.Message)
	wrk.receivedMessagesCalls = make(map[consensus.MessageType]func(*consensus.Message) bool)
	wrk.receivedHeadersHandlers = make([]func(data.HeaderHandler), 0)
	wrk.consensusStateChangedChannel = make(chan bool, 1)
	wrk.bootstrapper.AddSyncStateListener(wrk.receivedSyncState)
	wrk.initReceivedMessages()

	// set the limit for the antiflood handler
	topic := GetConsensusTopicIDFromShardCoordinator(args.ShardCoordinator)
	maxMessagesInARoundPerPeer := wrk.consensusService.GetMaxMessagesInARoundPerPeer()
	wrk.antifloodHandler.SetMaxMessagesForTopic(topic, maxMessagesInARoundPerPeer)

	go wrk.checkChannels()

	wrk.mapDisplayHashConsensusMessage = make(map[string][]*consensus.Message)

	return &wrk, nil
}

func checkNewWorkerParams(
	args *WorkerArgs,
) error {
	if args == nil {
		return ErrNilWorkerArgs
	}
	if check.IfNil(args.ConsensusService) {
		return ErrNilConsensusService
	}
	if check.IfNil(args.BlockChain) {
		return ErrNilBlockChain
	}
	if check.IfNil(args.BlockProcessor) {
		return ErrNilBlockProcessor
	}
	if check.IfNil(args.Bootstrapper) {
		return ErrNilBootstrapper
	}
	if check.IfNil(args.BroadcastMessenger) {
		return ErrNilBroadcastMessenger
	}
	if args.ConsensusState == nil {
		return ErrNilConsensusState
	}
	if check.IfNil(args.ForkDetector) {
		return ErrNilForkDetector
	}
	if check.IfNil(args.KeyGenerator) {
		return ErrNilKeyGenerator
	}
	if check.IfNil(args.Marshalizer) {
		return ErrNilMarshalizer
	}
	if check.IfNil(args.Hasher) {
		return ErrNilHasher
	}
	if check.IfNil(args.Rounder) {
		return ErrNilRounder
	}
	if check.IfNil(args.ShardCoordinator) {
		return ErrNilShardCoordinator
	}
	if check.IfNil(args.SingleSigner) {
		return ErrNilSingleSigner
	}
	if check.IfNil(args.SyncTimer) {
		return ErrNilSyncTimer
	}
	if check.IfNil(args.HeaderSigVerifier) {
		return ErrNilHeaderSigVerifier
	}
	if len(args.ChainID) == 0 {
		return ErrInvalidChainID
	}
	if check.IfNil(args.NetworkShardingCollector) {
		return ErrNilNetworkShardingCollector
	}
	if check.IfNil(args.AntifloodHandler) {
		return ErrNilAntifloodHandler
	}
	if check.IfNil(args.PoolAdder) {
		return ErrNilPoolAdder
	}

	return nil
}

func (wrk *Worker) receivedSyncState(isNodeSynchronized bool) {
	if isNodeSynchronized {
		select {
		case wrk.consensusStateChangedChannel <- true:
		default:
		}
	}
}

// ReceivedHeader process the received header, calling each received header handler registered in worker instance
func (wrk *Worker) ReceivedHeader(headerHandler data.HeaderHandler, _ []byte) {
	isHeaderForOtherShard := headerHandler.GetShardID() != wrk.shardCoordinator.SelfId()
	isHeaderForOtherRound := int64(headerHandler.GetRound()) != wrk.rounder.Index()
	headerCanNotBeProcessed := isHeaderForOtherShard || isHeaderForOtherRound
	if headerCanNotBeProcessed {
		return
	}

	wrk.mutReceivedHeadersHandler.RLock()
	for _, handler := range wrk.receivedHeadersHandlers {
		handler(headerHandler)
	}
	wrk.mutReceivedHeadersHandler.RUnlock()

	select {
	case wrk.consensusStateChangedChannel <- true:
	default:
	}
}

// AddReceivedHeaderHandler adds a new handler function for a received header
func (wrk *Worker) AddReceivedHeaderHandler(handler func(data.HeaderHandler)) {
	wrk.mutReceivedHeadersHandler.Lock()
	wrk.receivedHeadersHandlers = append(wrk.receivedHeadersHandlers, handler)
	wrk.mutReceivedHeadersHandler.Unlock()
}

func (wrk *Worker) initReceivedMessages() {
	wrk.mutReceivedMessages.Lock()
	wrk.receivedMessages = wrk.consensusService.InitReceivedMessages()
	wrk.mutReceivedMessages.Unlock()
}

// AddReceivedMessageCall adds a new handler function for a received messege type
func (wrk *Worker) AddReceivedMessageCall(messageType consensus.MessageType, receivedMessageCall func(cnsDta *consensus.Message) bool) {
	wrk.mutReceivedMessagesCalls.Lock()
	wrk.receivedMessagesCalls[messageType] = receivedMessageCall
	wrk.mutReceivedMessagesCalls.Unlock()
}

// RemoveAllReceivedMessagesCalls removes all the functions handlers
func (wrk *Worker) RemoveAllReceivedMessagesCalls() {
	wrk.mutReceivedMessagesCalls.Lock()
	wrk.receivedMessagesCalls = make(map[consensus.MessageType]func(*consensus.Message) bool)
	wrk.mutReceivedMessagesCalls.Unlock()
}

func (wrk *Worker) getCleanedList(cnsDataList []*consensus.Message) []*consensus.Message {
	cleanedCnsDataList := make([]*consensus.Message, 0)

	for i := 0; i < len(cnsDataList); i++ {
		if cnsDataList[i] == nil {
			continue
		}

		if wrk.rounder.Index() > cnsDataList[i].RoundIndex {
			continue
		}

		cleanedCnsDataList = append(cleanedCnsDataList, cnsDataList[i])
	}

	return cleanedCnsDataList
}

// ProcessReceivedMessage method redirects the received message to the channel which should handle it
func (wrk *Worker) ProcessReceivedMessage(message p2p.MessageP2P, fromConnectedPeer p2p.PeerID) error {
	if check.IfNil(message) {
		return ErrNilMessage
	}
	if message.Data() == nil {
		return ErrNilDataToProcess
	}

	err := wrk.antifloodHandler.CanProcessMessage(message, fromConnectedPeer)
	if err != nil {
		return err
	}

	topic := GetConsensusTopicIDFromShardCoordinator(wrk.shardCoordinator)
	err = wrk.antifloodHandler.CanProcessMessageOnTopic(message.Peer(), topic)
	if err != nil {
		return err
	}

	cnsDta := &consensus.Message{}
	err = wrk.marshalizer.Unmarshal(cnsDta, message.Data())
	if err != nil {
		return err
	}

	if !bytes.Equal(cnsDta.ChainID, wrk.chainID) {
		return fmt.Errorf("%w : received: %s, wanted: %s",
			ErrInvalidChainID,
			hex.EncodeToString(cnsDta.ChainID),
			hex.EncodeToString(wrk.chainID))
	}

	msgType := consensus.MessageType(cnsDta.MsgType)
	if !wrk.consensusService.IsMessageTypeValid(msgType) {
		return fmt.Errorf("%w : received message type from consensus topic is invalid: %d",
			ErrInvalidMessageType,
			msgType)
	}

	if len(cnsDta.BlockHeaderHash) != core.HashSizeInBytes {
		return fmt.Errorf("%w : received header hash from consensus topic has an invalid size: %d",
			ErrInvalidHeaderHashSize,
			len(cnsDta.BlockHeaderHash))
	}

	log.Trace("received message from consensus topic",
		"msg type", wrk.consensusService.GetStringValue(msgType),
		"from", core.GetTrimmedPk(hex.EncodeToString(cnsDta.PubKey)),
		"header hash", cnsDta.BlockHeaderHash,
		"round", cnsDta.RoundIndex,
		"size", len(message.Data()),
	)

	senderOK := wrk.consensusState.IsNodeInEligibleList(string(cnsDta.PubKey))
	if !senderOK {
		return fmt.Errorf("%w : node with public key %s is not in eligible list",
			ErrSenderNotOk,
			logger.DisplayByteSlice(cnsDta.PubKey))
	}

	if wrk.consensusState.RoundIndex+1 < cnsDta.RoundIndex {
		log.Trace("received message from consensus topic is for future round",
			"msg type", wrk.consensusService.GetStringValue(msgType),
			"from", core.GetTrimmedPk(hex.EncodeToString(cnsDta.PubKey)),
			"header hash", cnsDta.BlockHeaderHash,
			"msg round", cnsDta.RoundIndex,
			"round", wrk.consensusState.RoundIndex,
		)
		return ErrMessageForFutureRound
	}

	if wrk.consensusState.RoundIndex > cnsDta.RoundIndex {
		log.Trace("received message from consensus topic is for past round",
			"msg type", wrk.consensusService.GetStringValue(msgType),
			"from", core.GetTrimmedPk(hex.EncodeToString(cnsDta.PubKey)),
			"header hash", cnsDta.BlockHeaderHash,
			"msg round", cnsDta.RoundIndex,
			"round", wrk.consensusState.RoundIndex,
		)
		return ErrMessageForPastRound
	}

	sigVerifErr := wrk.checkSignature(cnsDta)
	if sigVerifErr != nil {
		return fmt.Errorf("%w : verify consensus data signature failed: %s",
			ErrInvalidSignature,
			sigVerifErr.Error())
	}

	go wrk.updateNetworkShardingVals(message, cnsDta)

	isMessageWithBlockBody := wrk.consensusService.IsMessageWithBlockBody(msgType)
	isMessageWithBlockHeader := wrk.consensusService.IsMessageWithBlockHeader(msgType)
	isMessageWithBlockBodyAndHeader := wrk.consensusService.IsMessageWithBlockBodyAndHeader(msgType)
	if isMessageWithBlockBody || isMessageWithBlockBodyAndHeader {
		wrk.addBlockToPool(cnsDta.GetBody())
	}

	if isMessageWithBlockHeader || isMessageWithBlockBodyAndHeader {
		headerHash := cnsDta.BlockHeaderHash
		header := wrk.blockProcessor.DecodeBlockHeader(cnsDta.Header)
		isHeaderInvalid := headerHash == nil || check.IfNil(header)
		if isHeaderInvalid {
			return fmt.Errorf("%w : received header from consensus topic is invalid",
				ErrInvalidHeader)
		}

		log.Debug("received proposed block",
			"from", core.GetTrimmedPk(hex.EncodeToString(cnsDta.PubKey)),
			"header hash", cnsDta.BlockHeaderHash,
			"round", header.GetRound(),
			"nonce", header.GetNonce(),
			"prev hash", header.GetPrevHash(),
			"nbTxs", header.GetTxCount(),
			"val stats root hash", header.GetValidatorStatsRootHash(),
		)

		err = header.CheckChainID(wrk.chainID)
		if err != nil {
			return fmt.Errorf("%w : chain ID in received header from consensus topic is invalid",
				err)
		}

		err = wrk.headerSigVerifier.VerifyRandSeed(header)
		if err != nil {
			return fmt.Errorf("%w : verify rand seed for received header from consensus topic failed",
				err)
		}

		wrk.processReceivedHeaderMetric(cnsDta)

		err = wrk.forkDetector.AddHeader(header, headerHash, process.BHProposed, nil, nil)
		if err != nil {
			log.Debug("add received header from consensus topic to fork detector failed",
				"error", err.Error())
			//we should not return error here because the other peers connected to self might need this message
			//to advance the consensus
		}
	}

	if wrk.consensusService.IsMessageWithSignature(msgType) {
		wrk.mutDisplayHashConsensusMessage.Lock()
		hash := string(cnsDta.BlockHeaderHash)
		wrk.mapDisplayHashConsensusMessage[hash] = append(wrk.mapDisplayHashConsensusMessage[hash], cnsDta)
		wrk.mutDisplayHashConsensusMessage.Unlock()
	}

	errNotCritical := wrk.checkSelfState(cnsDta)
	if errNotCritical != nil {
		log.Trace("checkSelfState", "error", errNotCritical.Error())
		//in this case should return nil but do not process the message
		//nil error will mean that the interceptor will validate this message and broadcast it to the connected peers
		return nil
	}

	go wrk.executeReceivedMessages(cnsDta)

	return nil
}

func (wrk *Worker) addBlockToPool(bodyBytes []byte) {
	bodyHandler := wrk.blockProcessor.DecodeBlockBody(bodyBytes)
	body, ok := bodyHandler.(*block.Body)
	if !ok {
		return
	}

	for _, miniblock := range body.MiniBlocks {
		hash, err := core.CalculateHash(wrk.marshalizer, wrk.hasher, miniblock)
		if err != nil {
			return
		}
		wrk.poolAdder.Put(hash, miniblock)
	}
}

func (wrk *Worker) processReceivedHeaderMetric(cnsDta *consensus.Message) {
	if !wrk.consensusState.IsNodeLeaderInCurrentRound(string(cnsDta.PubKey)) {
		return
	}

	sinceRoundStart := time.Since(wrk.rounder.TimeStamp())
	percent := sinceRoundStart * 100 / wrk.rounder.TimeDuration()
	wrk.appStatusHandler.SetUInt64Value(core.MetricReceivedProposedBlock, uint64(percent))
}

func (wrk *Worker) updateNetworkShardingVals(message p2p.MessageP2P, cnsMsg *consensus.Message) {
	wrk.networkShardingCollector.UpdatePeerIdPublicKey(message.Peer(), cnsMsg.PubKey)
	wrk.networkShardingCollector.UpdatePublicKeyShardId(cnsMsg.PubKey, wrk.shardCoordinator.SelfId())
}

func (wrk *Worker) checkSelfState(cnsDta *consensus.Message) error {
	if wrk.consensusState.SelfPubKey() == string(cnsDta.PubKey) {
		return ErrMessageFromItself
	}

	if wrk.consensusState.RoundCanceled && wrk.consensusState.RoundIndex == cnsDta.RoundIndex {
		return ErrRoundCanceled
	}

	return nil
}

func (wrk *Worker) checkSignature(cnsDta *consensus.Message) error {
	if cnsDta == nil {
		return ErrNilConsensusData
	}
	if cnsDta.PubKey == nil {
		return ErrNilPublicKey
	}
	if cnsDta.Signature == nil {
		return ErrNilSignature
	}

	pubKey, err := wrk.keyGenerator.PublicKeyFromByteArray(cnsDta.PubKey)
	if err != nil {
		return err
	}

	dataNoSig := *cnsDta
	signature := cnsDta.Signature
	dataNoSig.Signature = nil
	dataNoSigString, err := wrk.marshalizer.Marshal(&dataNoSig)
	if err != nil {
		return err
	}

	err = wrk.singleSigner.Verify(pubKey, dataNoSigString, signature)
	return err
}

func (wrk *Worker) executeReceivedMessages(cnsDta *consensus.Message) {
	wrk.mutReceivedMessages.Lock()

	msgType := consensus.MessageType(cnsDta.MsgType)
	cnsDataList := wrk.receivedMessages[msgType]
	cnsDataList = append(cnsDataList, cnsDta)
	wrk.receivedMessages[msgType] = cnsDataList
	wrk.executeStoredMessages()

	wrk.mutReceivedMessages.Unlock()
}

func (wrk *Worker) executeStoredMessages() {
	for _, i := range wrk.consensusService.GetMessageRange() {
		cnsDataList := wrk.receivedMessages[i]
		if len(cnsDataList) == 0 {
			continue
		}
		wrk.executeMessage(cnsDataList)
		cleanedCnsDtaList := wrk.getCleanedList(cnsDataList)
		wrk.receivedMessages[i] = cleanedCnsDtaList
	}
}

func (wrk *Worker) executeMessage(cnsDtaList []*consensus.Message) {
	for i, cnsDta := range cnsDtaList {
		if cnsDta == nil {
			continue
		}
		if wrk.consensusState.RoundIndex != cnsDta.RoundIndex {
			continue
		}

		msgType := consensus.MessageType(cnsDta.MsgType)
		if !wrk.consensusService.CanProceed(wrk.consensusState, msgType) {
			continue
		}

		cnsDtaList[i] = nil
		wrk.executeMessageChannel <- cnsDta
	}
}

// checkChannels method is used to listen to the channels through which node receives and consumes,
// during the round, different messages from the nodes which are in the validators group
func (wrk *Worker) checkChannels() {
	for {
		rcvDta := <-wrk.executeMessageChannel
		msgType := consensus.MessageType(rcvDta.MsgType)
		if callReceivedMessage, exist := wrk.receivedMessagesCalls[msgType]; exist {
			if callReceivedMessage(rcvDta) {
				select {
				case wrk.consensusStateChangedChannel <- true:
				default:
				}
			}
		}
	}
}

//Extend does an extension for the subround with subroundId
func (wrk *Worker) Extend(subroundId int) {
	wrk.consensusState.ExtendedCalled = true
	log.Debug("extend function is called",
		"subround", wrk.consensusService.GetSubroundName(subroundId))

	wrk.DisplayStatistics()

	if wrk.consensusService.IsSubroundStartRound(subroundId) {
		return
	}

	for wrk.consensusState.ProcessingBlock() {
		time.Sleep(time.Millisecond)
	}

	log.Debug("account state is reverted to snapshot")

	wrk.blockProcessor.RevertAccountState(wrk.consensusState.Header)

	shouldBroadcastLastCommittedHeader := wrk.consensusState.IsSelfLeaderInCurrentRound() &&
		wrk.consensusService.IsSubroundSignature(subroundId)
	if shouldBroadcastLastCommittedHeader {
		//TODO: Should be analyzed if call of wrk.broadcastLastCommittedHeader() is still necessary
	}
}

func (wrk *Worker) broadcastLastCommittedHeader() {
	header := wrk.blockChain.GetCurrentBlockHeader()
	if check.IfNil(header) {
		return
	}

	err := wrk.broadcastMessenger.BroadcastHeader(header)
	if err != nil {
		log.Debug("BroadcastHeader", "error", err.Error())
	}
}

// DisplayStatistics logs the consensus messages split on proposed headers
func (wrk *Worker) DisplayStatistics() {
	wrk.mutDisplayHashConsensusMessage.Lock()
	for hash, consensusMessages := range wrk.mapDisplayHashConsensusMessage {
		log.Debug("proposed header with signatures",
			"hash", []byte(hash),
			"sigs num", len(consensusMessages),
			"round", consensusMessages[0].RoundIndex,
		)

		for _, consensusMessage := range consensusMessages {
			log.Trace(core.GetTrimmedPk(hex.EncodeToString(consensusMessage.PubKey)))
		}

	}

	wrk.mapDisplayHashConsensusMessage = make(map[string][]*consensus.Message)

	wrk.mutDisplayHashConsensusMessage.Unlock()
}

// GetConsensusStateChangedChannel gets the channel for the consensusStateChanged
func (wrk *Worker) GetConsensusStateChangedChannel() chan bool {
	return wrk.consensusStateChangedChannel
}

// ExecuteStoredMessages tries to execute all the messages received which are valid for execution
func (wrk *Worker) ExecuteStoredMessages() {
	wrk.mutReceivedMessages.Lock()
	wrk.executeStoredMessages()
	wrk.mutReceivedMessages.Unlock()
}

// SetAppStatusHandler sets the status metric handler
func (wrk *Worker) SetAppStatusHandler(ash core.AppStatusHandler) error {
	if check.IfNil(ash) {
		return ErrNilAppStatusHandler
	}
	wrk.appStatusHandler = ash

	return nil
}

// IsInterfaceNil returns true if there is no value under the interface
func (wrk *Worker) IsInterfaceNil() bool {
	return wrk == nil
}
