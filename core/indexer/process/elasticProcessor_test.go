package process

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/indexer/client"
	"github.com/ElrondNetwork/elrond-go/core/indexer/disabled"
	"github.com/ElrondNetwork/elrond-go/core/indexer/process/block"
	"github.com/ElrondNetwork/elrond-go/core/indexer/process/miniblocks"
	"github.com/ElrondNetwork/elrond-go/core/indexer/process/transactions"
	"github.com/ElrondNetwork/elrond-go/core/indexer/types"
	"github.com/ElrondNetwork/elrond-go/core/mock"
	"github.com/ElrondNetwork/elrond-go/data"
	dataBlock "github.com/ElrondNetwork/elrond-go/data/block"
	"github.com/ElrondNetwork/elrond-go/data/receipt"
	"github.com/ElrondNetwork/elrond-go/data/rewardTx"
	"github.com/ElrondNetwork/elrond-go/data/smartContractResult"
	"github.com/ElrondNetwork/elrond-go/data/transaction"
	"github.com/ElrondNetwork/elrond-go/testscommon"
	"github.com/ElrondNetwork/elrond-go/testscommon/economicsmocks"
	"github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/stretchr/testify/require"
)

func newTestElasticSearchDatabase(elasticsearchWriter DatabaseClientHandler, arguments *ArgElasticProcessor) *elasticProcessor {
	return &elasticProcessor{
		elasticClient:            elasticsearchWriter,
		enabledIndexes:           arguments.EnabledIndexes,
		blockProc:                arguments.BlockProc,
		txProc:                   arguments.TxProc,
		miniblocksProc:           arguments.MiniblocksProc,
		accountsProc:             arguments.AccountsProc,
		validatorPubkeyConverter: arguments.ValidatorPubkeyConverter,
	}
}

func createMockElasticProcessorArgs() *ArgElasticProcessor {
	return &ArgElasticProcessor{
		ValidatorPubkeyConverter: mock.NewPubkeyConverterMock(32),
		DBClient:                 &mock.DatabaseWriterStub{},
		EnabledIndexes: map[string]struct{}{
			blockIndex: {}, txIndex: {}, miniblocksIndex: {}, tpsIndex: {}, validatorsIndex: {}, roundIndex: {}, accountsIndex: {}, ratingIndex: {}, accountsHistoryIndex: {},
		},
		CalculateHashFunc: func(object interface{}) ([]byte, error) {
			return core.CalculateHash(&mock.MarshalizerMock{}, &mock.HasherMock{}, object)
		},
		BlockProc: &mock.DbBlockProcHandlerStub{},
		TxProc:    &mock.DBTxsProcStub{},
	}
}

func newTestTxPool() map[string]data.TransactionHandler {
	txPool := map[string]data.TransactionHandler{
		"tx1": &transaction.Transaction{
			Nonce:     uint64(1),
			Value:     big.NewInt(1),
			RcvAddr:   []byte("receiver_address1"),
			SndAddr:   []byte("sender_address1"),
			GasPrice:  uint64(10000),
			GasLimit:  uint64(1000),
			Data:      []byte("tx_data1"),
			Signature: []byte("signature1"),
		},
		"tx2": &transaction.Transaction{
			Nonce:     uint64(2),
			Value:     big.NewInt(2),
			RcvAddr:   []byte("receiver_address2"),
			SndAddr:   []byte("sender_address2"),
			GasPrice:  uint64(10000),
			GasLimit:  uint64(1000),
			Data:      []byte("tx_data2"),
			Signature: []byte("signature2"),
		},
		"tx3": &transaction.Transaction{
			Nonce:     uint64(3),
			Value:     big.NewInt(3),
			RcvAddr:   []byte("receiver_address3"),
			SndAddr:   []byte("sender_address3"),
			GasPrice:  uint64(10000),
			GasLimit:  uint64(1000),
			Data:      []byte("tx_data3"),
			Signature: []byte("signature3"),
		},
	}

	return txPool
}

func newTestBlockBody() *dataBlock.Body {
	return &dataBlock.Body{
		MiniBlocks: []*dataBlock.MiniBlock{
			{
				TxHashes: [][]byte{
					[]byte("tx1"),
					[]byte("tx2"),
				},
				ReceiverShardID: 2,
				SenderShardID:   2,
			},
			{
				TxHashes: [][]byte{
					[]byte("tx3"),
				},
				ReceiverShardID: 4,
				SenderShardID:   1,
			},
		},
	}
}

func TestNewElasticProcessorWithKibana(t *testing.T) {
	args := createMockElasticProcessorArgs()
	args.UseKibana = true
	args.DBClient = &mock.DatabaseWriterStub{}

	elasticProc, err := NewElasticProcessor(args)
	require.NoError(t, err)
	require.NotNil(t, elasticProc)
}

func TestElasticProcessor_RemoveHeader(t *testing.T) {
	called := false

	args := createMockElasticProcessorArgs()
	args.DBClient = &mock.DatabaseWriterStub{
		DoBulkRemoveCalled: func(index string, hashes []string) error {
			called = true
			return nil
		},
	}

	args.CalculateHashFunc = func(object interface{}) ([]byte, error) {
		return core.CalculateHash(&mock.MarshalizerMock{}, &mock.HasherMock{}, object)
	}

	elasticProc, err := NewElasticProcessor(args)
	require.NoError(t, err)

	err = elasticProc.RemoveHeader(&dataBlock.Header{})
	require.Nil(t, err)
	require.True(t, called)
}

func TestElasticProcessor_RemoveMiniblocks(t *testing.T) {
	called := false

	mb1 := &dataBlock.MiniBlock{
		Type: dataBlock.PeerBlock,
	}
	mb2 := &dataBlock.MiniBlock{
		ReceiverShardID: 0,
		SenderShardID:   1,
	} // should be removed
	mb3 := &dataBlock.MiniBlock{
		ReceiverShardID: 1,
		SenderShardID:   1,
	} // should be removed
	mb4 := &dataBlock.MiniBlock{
		ReceiverShardID: 1,
		SenderShardID:   0,
	} // should NOT be removed

	args := createMockElasticProcessorArgs()

	mbHash2, _ := core.CalculateHash(&mock.MarshalizerMock{}, &mock.HasherMock{}, mb2)
	mbHash3, _ := core.CalculateHash(&mock.MarshalizerMock{}, &mock.HasherMock{}, mb3)

	args.DBClient = &mock.DatabaseWriterStub{
		DoBulkRemoveCalled: func(index string, hashes []string) error {
			called = true
			require.Equal(t, hashes[0], hex.EncodeToString(mbHash2))
			require.Equal(t, hashes[1], hex.EncodeToString(mbHash3))
			return nil
		},
	}

	elasticProc, err := NewElasticProcessor(args)
	require.NoError(t, err)

	header := &dataBlock.Header{
		ShardID: 1,
		MiniBlockHeaders: []dataBlock.MiniBlockHeader{
			{
				Hash: []byte("hash1"),
			},
			{
				Hash: []byte("hash2"),
			},
			{
				Hash: []byte("hash3"),
			},
			{
				Hash: []byte("hash4"),
			},
		},
	}
	body := &dataBlock.Body{
		MiniBlocks: dataBlock.MiniBlockSlice{
			mb1, mb2, mb3, mb4,
		},
	}
	err = elasticProc.RemoveMiniblocks(header, body)
	require.Nil(t, err)
	require.True(t, called)
}

func TestElasticseachDatabaseSaveHeader_RequestError(t *testing.T) {
	localErr := errors.New("localErr")
	header := &dataBlock.Header{Nonce: 1}
	signerIndexes := []uint64{0, 1}
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoRequestCalled: func(req *esapi.IndexRequest) error {
			return localErr
		},
	}
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveHeader(header, signerIndexes, &dataBlock.Body{}, nil, 1)
	require.Equal(t, localErr, err)
}

func TestElasticseachDatabaseSaveHeader_CheckRequestBody(t *testing.T) {
	header := &dataBlock.Header{
		Nonce: 1,
	}
	signerIndexes := []uint64{0, 1}

	miniBlock := &dataBlock.MiniBlock{
		Type: dataBlock.TxBlock,
	}
	blockBody := &dataBlock.Body{
		MiniBlocks: []*dataBlock.MiniBlock{
			miniBlock,
		},
	}

	arguments := createMockElasticProcessorArgs()

	mbHash, _ := core.CalculateHash(&mock.MarshalizerMock{}, &mock.HasherMock{}, miniBlock)
	hexEncodedHash := hex.EncodeToString(mbHash)

	dbWriter := &mock.DatabaseWriterStub{
		DoRequestCalled: func(req *esapi.IndexRequest) error {
			require.Equal(t, blockIndex, req.Index)

			var bl types.Block
			blockBytes, _ := ioutil.ReadAll(req.Body)
			_ = json.Unmarshal(blockBytes, &bl)
			require.Equal(t, header.Nonce, bl.Nonce)
			require.Equal(t, hexEncodedHash, bl.MiniBlocksHashes[0])
			require.Equal(t, signerIndexes, bl.Validators)

			return nil
		},
	}

	arguments.BlockProc = block.NewBlockProcessor(&mock.HasherMock{}, &mock.MarshalizerMock{})
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)
	err := elasticDatabase.SaveHeader(header, signerIndexes, blockBody, nil, 1)
	require.Nil(t, err)
}

func TestElasticseachSaveTransactions(t *testing.T) {
	localErr := errors.New("localErr")
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoBulkRequestCalled: func(buff *bytes.Buffer, index string) error {
			return localErr
		},
	}

	body := newTestBlockBody()
	header := &dataBlock.Header{Nonce: 1, TxCount: 2}
	txPool := newTestTxPool()

	calculateHash := func(object interface{}) ([]byte, error) {
		return core.CalculateHash(&mock.MarshalizerMock{}, &mock.HasherMock{}, object)
	}
	txDbProc := transactions.NewTransactionsProcessor(
		&mock.PubkeyConverterMock{},
		&economicsmocks.EconomicsHandlerStub{},
		false,
		&mock.ShardCoordinatorMock{},
		false,
		calculateHash,
		disabled.NewNilTxLogsProcessor(),
	)
	arguments.TxProc = txDbProc

	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)
	err := elasticDatabase.SaveTransactions(body, header, txPool, 0, map[string]bool{})
	require.Equal(t, localErr, err)
}

func TestElasticProcessor_SaveValidatorsRating(t *testing.T) {
	docID := "0_1"
	localErr := errors.New("localErr")

	blsKey := "bls"

	arguments := createMockElasticProcessorArgs()
	arguments.DBClient = &mock.DatabaseWriterStub{
		DoBulkRequestCalled: func(buff *bytes.Buffer, index string) error {
			return localErr
		},
	}

	elasticProc, _ := NewElasticProcessor(arguments)

	err := elasticProc.SaveValidatorsRating(
		docID,
		[]types.ValidatorRatingInfo{
			{
				PublicKey: blsKey,
				Rating:    100,
			},
		},
	)
	require.Equal(t, localErr, err)
}

func TestElasticProcessor_SaveMiniblocks(t *testing.T) {
	localErr := errors.New("localErr")

	arguments := createMockElasticProcessorArgs()
	arguments.DBClient = &mock.DatabaseWriterStub{
		DoBulkRequestCalled: func(buff *bytes.Buffer, index string) error {
			return localErr
		},
		DoMultiGetCalled: func(hashes []string, index string) (map[string]interface{}, error) {
			return nil, nil
		},
	}

	arguments.MiniblocksProc = miniblocks.NewMiniblocksProcessor(&mock.HasherMock{}, &mock.MarshalizerMock{})
	elasticProc, _ := NewElasticProcessor(arguments)

	header := &dataBlock.Header{}
	body := &dataBlock.Body{MiniBlocks: dataBlock.MiniBlockSlice{
		{SenderShardID: 0, ReceiverShardID: 1},
	}}
	mbsInDB, err := elasticProc.SaveMiniblocks(header, body)
	require.Equal(t, localErr, err)
	require.Equal(t, 0, len(mbsInDB))
}

func TestElasticsearch_saveShardValidatorsPubKeys_RequestError(t *testing.T) {
	shardID := uint32(0)
	epoch := uint32(0)
	valPubKeys := [][]byte{[]byte("key1"), []byte("key2")}
	localErr := errors.New("localErr")
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoRequestCalled: func(req *esapi.IndexRequest) error {
			return localErr
		},
	}
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveShardValidatorsPubKeys(shardID, epoch, valPubKeys)
	require.Equal(t, localErr, err)
}

func TestElasticsearch_saveShardValidatorsPubKeys(t *testing.T) {
	shardID := uint32(0)
	epoch := uint32(0)
	valPubKeys := [][]byte{[]byte("key1"), []byte("key2")}
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoRequestCalled: func(req *esapi.IndexRequest) error {
			require.Equal(t, fmt.Sprintf("%d_%d", shardID, epoch), req.DocumentID)
			return nil
		},
	}
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveShardValidatorsPubKeys(shardID, epoch, valPubKeys)
	require.Nil(t, err)
}

func TestElasticsearch_saveShardStatistics_reqError(t *testing.T) {
	tpsBenchmark := &testscommon.TpsBenchmarkMock{}
	metaBlock := &dataBlock.MetaBlock{
		TxCount: 2, Nonce: 1,
		ShardInfo: []dataBlock.ShardData{{HeaderHash: []byte("hash")}},
	}
	tpsBenchmark.UpdateWithShardStats(metaBlock)

	localError := errors.New("local err")
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoBulkRequestCalled: func(buff *bytes.Buffer, index string) error {
			return localError
		},
	}

	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveShardStatistics(tpsBenchmark)
	require.Equal(t, localError, err)
}

func TestElasticsearch_saveShardStatistics(t *testing.T) {
	tpsBenchmark := &testscommon.TpsBenchmarkMock{}
	metaBlock := &dataBlock.MetaBlock{
		TxCount: 2, Nonce: 1,
		ShardInfo: []dataBlock.ShardData{{HeaderHash: []byte("hash")}},
	}
	tpsBenchmark.UpdateWithShardStats(metaBlock)

	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoBulkRequestCalled: func(buff *bytes.Buffer, index string) error {
			require.Equal(t, tpsIndex, index)
			return nil
		},
	}
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveShardStatistics(tpsBenchmark)
	require.Nil(t, err)
}

func TestElasticsearch_saveRoundInfo(t *testing.T) {
	roundInfo := types.RoundInfo{
		Index: 1, ShardId: 0, BlockWasProposed: true,
	}
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoRequestCalled: func(req *esapi.IndexRequest) error {
			require.Equal(t, strconv.FormatUint(uint64(roundInfo.ShardId), 10)+"_"+strconv.FormatUint(roundInfo.Index, 10), req.DocumentID)
			return nil
		},
	}
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveRoundsInfo([]types.RoundInfo{roundInfo})
	require.Nil(t, err)
}

func TestElasticsearch_saveRoundInfoRequestError(t *testing.T) {
	roundInfo := types.RoundInfo{}
	localError := errors.New("local err")
	arguments := createMockElasticProcessorArgs()
	dbWriter := &mock.DatabaseWriterStub{
		DoBulkRequestCalled: func(buff *bytes.Buffer, index string) error {
			return localError
		},
	}
	elasticDatabase := newTestElasticSearchDatabase(dbWriter, arguments)

	err := elasticDatabase.SaveRoundsInfo([]types.RoundInfo{roundInfo})
	require.Equal(t, localError, err)

}

func TestUpdateMiniBlock(t *testing.T) {
	t.Skip("test must run only if you have an elasticsearch server on address http://localhost:9200")

	indexTemplates, indexPolicies, _ := GetElasticTemplatesAndPolicies("./testdata/noKibana", false)
	dbClient, _ := client.NewElasticClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	args := &ArgElasticProcessor{
		DBClient:                 dbClient,
		IndexTemplates:           indexTemplates,
		IndexPolicies:            indexPolicies,
		ValidatorPubkeyConverter: mock.NewPubkeyConverterMock(32),
		EnabledIndexes: map[string]struct{}{
			"miniblocks": {},
		},
	}

	esDatabase, err := NewElasticProcessor(args)
	require.Nil(t, err)

	header1 := &dataBlock.Header{
		ShardID: 0,
	}
	body1 := &dataBlock.Body{
		MiniBlocks: []*dataBlock.MiniBlock{
			{SenderShardID: 1, ReceiverShardID: 0, TxHashes: [][]byte{[]byte("hash12")}},
			{SenderShardID: 0, ReceiverShardID: 1, TxHashes: [][]byte{[]byte("hash1")}},
		},
	}

	header2 := &dataBlock.Header{
		ShardID: 1,
	}

	// insert
	_, _ = esDatabase.SaveMiniblocks(header1, body1)
	// update
	_, _ = esDatabase.SaveMiniblocks(header2, body1)
}

func TestSaveRoundsInfo(t *testing.T) {
	t.Skip("test must run only if you have an elasticsearch server on address http://localhost:9200")

	indexTemplates, indexPolicies, _ := GetElasticTemplatesAndPolicies("", false)
	dbClient, _ := client.NewElasticClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	args := &ArgElasticProcessor{
		DBClient:       dbClient,
		IndexTemplates: indexTemplates,
		IndexPolicies:  indexPolicies,
	}

	esDatabase, _ := NewElasticProcessor(args)

	roundInfo1 := types.RoundInfo{
		Index: 1, ShardId: 0, BlockWasProposed: true,
	}
	roundInfo2 := types.RoundInfo{
		Index: 2, ShardId: 0, BlockWasProposed: true,
	}
	roundInfo3 := types.RoundInfo{
		Index: 3, ShardId: 0, BlockWasProposed: true,
	}

	_ = esDatabase.SaveRoundsInfo([]types.RoundInfo{roundInfo1, roundInfo2, roundInfo3})
}

func TestUpdateTransaction(t *testing.T) {
	t.Skip("test must run only if you have an elasticsearch server on address http://localhost:9200")

	indexTemplates, indexPolicies, _ := GetElasticTemplatesAndPolicies("", false)
	dbClient, _ := client.NewElasticClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	args := ArgElasticProcessor{
		DBClient:                 dbClient,
		IndexTemplates:           indexTemplates,
		IndexPolicies:            indexPolicies,
		ValidatorPubkeyConverter: mock.NewPubkeyConverterMock(96),
		EnabledIndexes: map[string]struct{}{
			"transactions": {},
		},
	}

	esDatabase, err := NewElasticProcessor(&args)
	require.Nil(t, err)

	txHash1 := []byte("txHash1")
	tx1 := &transaction.Transaction{
		GasPrice: 10,
		GasLimit: 500,
	}
	txHash2 := []byte("txHash2")
	sndAddr := []byte("snd")
	tx2 := &transaction.Transaction{
		GasPrice: 10,
		GasLimit: 500,
		SndAddr:  sndAddr,
	}
	txHash3 := []byte("txHash3")
	tx3 := &transaction.Transaction{}

	recHash1 := []byte("recHash1")
	rec1 := &receipt.Receipt{
		Value:  big.NewInt(100),
		TxHash: txHash1,
	}

	scHash1 := []byte("scHash1")
	scResult1 := &smartContractResult.SmartContractResult{
		OriginalTxHash: txHash1,
	}

	scHash2 := []byte("scHash2")
	scResult2 := &smartContractResult.SmartContractResult{
		OriginalTxHash: txHash2,
		RcvAddr:        sndAddr,
		GasLimit:       500,
		GasPrice:       1,
		Value:          big.NewInt(150),
	}

	rTx1Hash := []byte("rTxHash1")
	rTx1 := &rewardTx.RewardTx{
		Round: 1113,
	}
	rTx2Hash := []byte("rTxHash2")
	rTx2 := &rewardTx.RewardTx{
		Round: 1114,
	}

	body := &dataBlock.Body{
		MiniBlocks: []*dataBlock.MiniBlock{
			{
				TxHashes: [][]byte{txHash1, txHash2},
				Type:     dataBlock.TxBlock,
			},
			{
				TxHashes: [][]byte{txHash3},
				Type:     dataBlock.TxBlock,
			},
			{
				Type:     dataBlock.RewardsBlock,
				TxHashes: [][]byte{rTx1Hash, rTx2Hash},
			},
			{
				TxHashes: [][]byte{recHash1},
				Type:     dataBlock.ReceiptBlock,
			},
			{
				TxHashes: [][]byte{scHash1, scHash2},
				Type:     dataBlock.SmartContractResultBlock,
			},
		},
	}
	header := &dataBlock.Header{}
	txPool := map[string]data.TransactionHandler{
		string(txHash1):  tx1,
		string(txHash2):  tx2,
		string(txHash3):  tx3,
		string(recHash1): rec1,
		string(rTx1Hash): rTx1,
		string(rTx2Hash): rTx2,
	}

	body.MiniBlocks[0].ReceiverShardID = 1
	// insert
	_ = esDatabase.SaveTransactions(body, header, txPool, 1, map[string]bool{})

	fmt.Println(hex.EncodeToString(txHash1))

	header.TimeStamp = 1234
	txPool = map[string]data.TransactionHandler{
		string(txHash1): tx1,
		string(txHash2): tx2,
		string(scHash1): scResult1,
		string(scHash2): scResult2,
	}

	body.MiniBlocks[0].TxHashes = append(body.MiniBlocks[0].TxHashes, txHash3)

	// update
	_ = esDatabase.SaveTransactions(body, header, txPool, 0, map[string]bool{})
}

func TestGetMultiple(t *testing.T) {
	t.Skip("test must run only if you have an elasticsearch server on address http://localhost:9200")

	indexTemplates, indexPolicies, _ := GetElasticTemplatesAndPolicies("", false)
	dbClient, _ := client.NewElasticClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	args := ArgElasticProcessor{
		DBClient:       dbClient,
		IndexTemplates: indexTemplates,
		IndexPolicies:  indexPolicies,
	}

	es, _ := NewElasticProcessor(&args)

	hashes := []string{
		"57cf251084cd7f79563207c52f938359eebdaf27f91fef1335a076f5dc4873351",
		"9a3beb87930e42b820cbcb5e73b224ebfc707308aa377905eda18d4589e2b093",
	}
	response, _ := es.getExistingObjMap(hashes, "transactions")
	fmt.Println(response)
}

func TestIndexTransactionDestinationBeforeSourceShard(t *testing.T) {
	t.Skip("test must run only if you have an elasticsearch server on address http://localhost:9200")

	indexTemplates, indexPolicies, _ := GetElasticTemplatesAndPolicies("", false)
	dbClient, _ := client.NewElasticClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	args := ArgElasticProcessor{
		DBClient:                 dbClient,
		ValidatorPubkeyConverter: &mock.PubkeyConverterMock{},
		IndexTemplates:           indexTemplates,
		IndexPolicies:            indexPolicies,
	}

	esDatabase, _ := NewElasticProcessor(&args)

	txHash1 := []byte("txHash1")
	tx1 := &transaction.Transaction{
		GasPrice: 10,
		GasLimit: 500,
	}
	txHash2 := []byte("txHash2")
	sndAddr := []byte("snd")
	tx2 := &transaction.Transaction{
		GasPrice: 10,
		GasLimit: 500,
		SndAddr:  sndAddr,
	}

	header := &dataBlock.Header{}
	txPool := map[string]data.TransactionHandler{
		string(txHash1): tx1,
		string(txHash2): tx2,
	}
	body := &dataBlock.Body{
		MiniBlocks: []*dataBlock.MiniBlock{
			{
				TxHashes: [][]byte{txHash1, txHash2},
				Type:     dataBlock.TxBlock,
			},
		},
	}
	body.MiniBlocks[0].ReceiverShardID = 2
	body.MiniBlocks[0].SenderShardID = 1
	isMBSInDB, _ := esDatabase.SaveMiniblocks(header, body)
	_ = esDatabase.SaveTransactions(body, header, txPool, 2, isMBSInDB)

	txPool = map[string]data.TransactionHandler{
		string(txHash1): tx1,
		string(txHash2): tx2,
	}

	header.ShardID = 1
	isMBSInDB, _ = esDatabase.SaveMiniblocks(header, body)
	_ = esDatabase.SaveTransactions(body, header, txPool, 0, isMBSInDB)
}

func TestDoBulkRequestLimit(t *testing.T) {
	t.Skip("test must run only if you have an elasticsearch server on address http://localhost:9200")

	indexTemplates, indexPolicies, _ := GetElasticTemplatesAndPolicies("", false)
	dbClient, _ := client.NewElasticClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	args := ArgElasticProcessor{
		DBClient:                 dbClient,
		ValidatorPubkeyConverter: &mock.PubkeyConverterMock{},
		IndexTemplates:           indexTemplates,
		IndexPolicies:            indexPolicies,
	}

	esDatabase, _ := NewElasticProcessor(&args)
	//Generate transaction and hashes
	numTransactions := 1
	dataSize := 900001
	for i := 0; i < 1000; i++ {
		txs, hashes := generateTransactions(numTransactions, dataSize)

		header := &dataBlock.Header{}
		txsPool := make(map[string]data.TransactionHandler)
		for j := 0; j < numTransactions; j++ {
			txsPool[hashes[j]] = &txs[j]
		}

		miniblock := &dataBlock.MiniBlock{
			TxHashes: make([][]byte, numTransactions),
			Type:     dataBlock.TxBlock,
		}
		for j := 0; j < numTransactions; j++ {
			miniblock.TxHashes[j] = []byte(hashes[j])
		}

		body := &dataBlock.Body{
			MiniBlocks: []*dataBlock.MiniBlock{
				miniblock,
			},
		}
		body.MiniBlocks[0].ReceiverShardID = 2
		body.MiniBlocks[0].SenderShardID = 1

		_ = esDatabase.SaveTransactions(body, header, txsPool, 2, map[string]bool{})
	}
}

func generateTransactions(numTxs int, datFieldSize int) ([]transaction.Transaction, []string) {
	txs := make([]transaction.Transaction, numTxs)
	hashes := make([]string, numTxs)

	randomByteArray := make([]byte, datFieldSize)
	_, _ = rand.Read(randomByteArray)

	for i := 0; i < numTxs; i++ {
		txs[i] = transaction.Transaction{
			Nonce:     uint64(i),
			Value:     big.NewInt(int64(i)),
			RcvAddr:   []byte("443e79a8d99ba093262c1db48c58ab3d59bcfeb313ca5cddf2a9d1d06f9894ec"),
			SndAddr:   []byte("443e79a8d99ba093262c1db48c58ab3d59bcfeb313ca5cddf2a9d1d06f9894ec"),
			GasPrice:  200000000000,
			GasLimit:  20000,
			Data:      randomByteArray,
			Signature: []byte("443e79a8d99ba093262c1db48c58ab3d"),
		}
		hashes[i] = fmt.Sprintf("%v", time.Now())
	}

	return txs, hashes
}

func TestElasticProcessor_RemoveTransactions(t *testing.T) {
	arguments := createMockElasticProcessorArgs()

	called := false
	txsHashes := [][]byte{[]byte("txHas1"), []byte("txHash2")}
	expectedHashes := []string{hex.EncodeToString(txsHashes[0]), hex.EncodeToString(txsHashes[1])}
	dbWriter := &mock.DatabaseWriterStub{
		DoBulkRemoveCalled: func(index string, hashes []string) error {
			require.Equal(t, txIndex, index)
			require.Equal(t, expectedHashes, expectedHashes)
			called = true
			return nil
		},
	}

	elasticSearchProc := newTestElasticSearchDatabase(dbWriter, arguments)

	header := &dataBlock.Header{ShardID: core.MetachainShardId, MiniBlockHeaders: []dataBlock.MiniBlockHeader{{}}}
	blk := &dataBlock.Body{
		MiniBlocks: dataBlock.MiniBlockSlice{
			{
				TxHashes:        txsHashes,
				Type:            dataBlock.RewardsBlock,
				SenderShardID:   core.MetachainShardId,
				ReceiverShardID: 0,
			},
			{
				Type: dataBlock.TxBlock,
			},
		},
	}

	err := elasticSearchProc.RemoveTransactions(header, blk)
	require.Nil(t, err)
	require.True(t, called)
}
