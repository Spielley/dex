package relayer

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"strings"

	"github.com/go-redis/redis"

	"hameid.net/cdex/dex/internal/store"
	"hameid.net/cdex/dex/internal/wrappers"

	"hameid.net/cdex/dex/internal/models"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"hameid.net/cdex/dex/_abi/DEXChain"
	"hameid.net/cdex/dex/_abi/HomeBridge"
	"hameid.net/cdex/dex/_abi/OrderMatchContract"
	"hameid.net/cdex/dex/_abi/Orderbook"
	"hameid.net/cdex/dex/internal/utils"
)

type bridgeRef struct {
	client *ethclient.Client
	abi    *abi.ABI
	// instance *HomeBridge.HomeBridge
}

type exchangeRef struct {
	client               *ethclient.Client
	exchangeABI          *abi.ABI
	orderbookABI         *abi.ABI
	ordermatcherABI      *abi.ABI
	ordermatcherInstance *OrderMatchContract.OrderMatchContract
}

// Relayer struct
type Relayer struct {
	networks    *utils.NetworksInfo
	contracts   *utils.ContractsInfo
	bridge      *bridgeRef
	exchange    *exchangeRef
	store       *store.DataStore
	redisClient *redis.Client

	matcherPrivateKey *ecdsa.PrivateKey
	matcherPublicKey  *ecdsa.PublicKey
	matcherAddress    *common.Address
}

type redisChannelMessage struct {
	MessageType string      `json:"messageType"`
	Payload     interface{} `json:"messageContent"`
}

var channelSize = 10000

// Initialize reads and decodes ABIs to be used for communicating with chain
func (r *Relayer) Initialize() {
	fmt.Printf("\nConnecting to %s...\n", r.networks.Bridge.WebSocketProvider)
	homeClient, err := ethclient.Dial(r.networks.Bridge.WebSocketProvider)
	if err != nil {
		log.Panic(err)
	}
	// bridge, err := HomeBridge.NewHomeBridge(r.contracts.Bridge.Address.Address, homeClient)
	// if err != nil {
	// 	log.Panic(err)
	// }
	fmt.Printf("Decoding Bridge contract ABI...\n")
	bridgeABI, err := abi.JSON(strings.NewReader(string(HomeBridge.HomeBridgeABI)))
	if err != nil {
		log.Fatal(err)
	}
	r.bridge.client = homeClient
	// r.bridge.instance = bridge
	r.bridge.abi = &bridgeABI

	fmt.Printf("\nConnecting to %s...\n", r.networks.Exchange.WebSocketProvider)
	exchangeClient, err := ethclient.Dial(r.networks.Exchange.WebSocketProvider)
	if err != nil {
		log.Panic(err)
	}
	fmt.Printf("Decoding Exchange contract ABI...\n")
	exchangeABI, err := abi.JSON(strings.NewReader(string(DEXChain.DEXChainABI)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Decoding Orderbook contract ABI...\n")
	orderbookABI, err := abi.JSON(strings.NewReader(string(Orderbook.OrderbookABI)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Decoding Order Matcher contract ABI...\n")
	ordermatcherABI, err := abi.JSON(strings.NewReader(string(OrderMatchContract.OrderMatchContractABI)))
	if err != nil {
		log.Fatal(err)
	}

	ordermatcherInstance, err := OrderMatchContract.NewOrderMatchContract(r.contracts.OrderMatcher.Address.Address, exchangeClient)
	if err != nil {
		log.Panic(err)
	}

	r.exchange.client = exchangeClient
	r.exchange.exchangeABI = &exchangeABI
	r.exchange.orderbookABI = &orderbookABI
	r.exchange.ordermatcherABI = &ordermatcherABI
	r.exchange.ordermatcherInstance = ordermatcherInstance

	fmt.Printf("\n")
	r.store.Initialize()

	fmt.Printf("\n\nRelayer initialization successful :)\n\n")
}

// RunOnBridgeNetwork runs relayer on the home network
func (r *Relayer) RunOnBridgeNetwork() {
	fmt.Printf("Trying to listen events on Bridge contract %s...\n", r.contracts.Bridge.Address.Address.String())
	query := ethereum.FilterQuery{
		Addresses: []common.Address{r.contracts.Bridge.Address.Address},
		Topics:    [][]common.Hash{{r.contracts.Bridge.Topics.Withdraw.Hash}},
	}

	logs := make(chan types.Log, channelSize)

	sub, err := r.bridge.client.SubscribeFilterLogs(context.Background(), query, logs)
	if err != nil {
		log.Panic(err)
	}

	go func() {
		for {
			select {
			case err := <-sub.Err():
				log.Fatal("Home Network Subcription Error:", err)

			case vLog := <-logs:
				r.bridgeWithdrawCallback(vLog)
			}
		}
	}()
}

// RunOnExchangeNetwork runs relayer on the exchange network
func (r *Relayer) RunOnExchangeNetwork() {
	fmt.Printf("Trying to listen events on Exchange contract %s...\n", r.contracts.Exchange.Address.Address.String())
	query := ethereum.FilterQuery{
		Addresses: []common.Address{
			r.contracts.Exchange.Address.Address,
			r.contracts.Orderbook.Address.Address,
			r.contracts.OrderMatcher.Address.Address,
		},
		Topics: [][]common.Hash{
			{
				r.contracts.Exchange.Topics.BalanceUpdate.Hash,
				r.contracts.Orderbook.Topics.PlaceBuyOrder.Hash,
				r.contracts.Orderbook.Topics.PlaceSellOrder.Hash,
				r.contracts.Orderbook.Topics.CancelOrder.Hash,
				r.contracts.OrderMatcher.Topics.Trade.Hash,
				r.contracts.OrderMatcher.Topics.OrderFilledVolumeUpdate.Hash,
				r.contracts.Exchange.Topics.Withdraw.Hash,
				r.contracts.Exchange.Topics.ReadyToWithdraw.Hash,
				r.contracts.Exchange.Topics.WithdrawSignatureSubmitted.Hash,
			},
		},
	}
	logs := make(chan types.Log, channelSize)
	sub, err := r.exchange.client.SubscribeFilterLogs(context.Background(), query, logs)
	if err != nil {
		log.Panic(err)
	}

	go func() {
		for {
			select {
			case err := <-sub.Err():
				log.Fatal("Foreign Network Subcription Error:", err)

			case vLog := <-logs:
				for _, topic := range vLog.Topics {
					switch topic {
					case r.contracts.Exchange.Topics.BalanceUpdate.Hash:
						r.balanceUpdateLogCallback(vLog)
					case r.contracts.Orderbook.Topics.PlaceBuyOrder.Hash:
						r.placeOrderLogCallback(vLog, true)
					case r.contracts.Orderbook.Topics.PlaceSellOrder.Hash:
						r.placeOrderLogCallback(vLog, false)
					case r.contracts.Orderbook.Topics.CancelOrder.Hash:
						r.cancelOrderLogCallback(vLog)
					case r.contracts.OrderMatcher.Topics.Trade.Hash:
						r.tradeLogCallback(vLog)
					case r.contracts.OrderMatcher.Topics.OrderFilledVolumeUpdate.Hash:
						r.updateFilledVolumeLogCallback(vLog)
					case r.contracts.Exchange.Topics.WithdrawSignatureSubmitted.Hash:
						r.withdrawSignSubmittedCallback(vLog)
					case r.contracts.Exchange.Topics.ReadyToWithdraw.Hash:
						r.readyToWithdrawCallback(vLog)
					case r.contracts.Exchange.Topics.Withdraw.Hash:
						r.dexWithdrawCallback(vLog)
					}
				}
			}
		}
	}()
}

// Quit terminates relayer instance
func (r *Relayer) Quit() {
	fmt.Printf("\nCleaning up...\n")
	r.store.Close()
	fmt.Printf("\nBye bye...\n")
}

// NewRelayer creates and populates a Relayer struct object
func NewRelayer(contractsFilePath, networksFilePath, connectionString, redisHostAddress, redisPassword, keystoreFilePath, passwordFilePath string) *Relayer {
	fmt.Printf("Starting relayer...\n")
	fmt.Printf("Reading config files...\n")
	fmt.Printf("Reading %s...\n", contractsFilePath)
	// Read config files
	contractsInfo, err := utils.ReadContractsInfo(contractsFilePath)
	if err != nil {
		log.Panic(err)
	}

	fmt.Printf("Reading %s...\n", networksFilePath)
	nwInfo, err := utils.ReadNetworksInfo(networksFilePath)
	if err != nil {
		log.Panic(err)
	}

	fmt.Printf("Decrypting order matcher keystore %s...\n", keystoreFilePath)
	// Decrypt keystore
	accountKey, err := utils.DecryptPrivateKeyFromKeystoreWithPasswordFile(keystoreFilePath, passwordFilePath)
	if err != nil {
		log.Panic(err)
	}

	privateKey := accountKey.PrivateKey
	publicKey := privateKey.Public()

	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("error casting public key to ECDSA")
	}

	fromAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	fmt.Printf("Order matcher account address: %s\n\n", fromAddress.String())

	return &Relayer{
		networks:  nwInfo,
		contracts: contractsInfo,
		bridge: &bridgeRef{
			client: nil,
			// instance: nil,
			abi: nil,
		},
		exchange: &exchangeRef{
			client:               nil,
			exchangeABI:          nil,
			orderbookABI:         nil,
			ordermatcherABI:      nil,
			ordermatcherInstance: nil,
		},
		store:             store.NewDataStore(connectionString),
		redisClient:       store.NewRedisClient(redisHostAddress, redisPassword),
		matcherPrivateKey: privateKey,
		matcherPublicKey:  publicKeyECDSA,
		matcherAddress:    &fromAddress,
	}
}

func (r *Relayer) balanceUpdateLogCallback(vLog types.Log) {
	buEvent := struct {
		Token   common.Address
		User    common.Address
		Balance *big.Int
		Escrow  *big.Int
	}{}
	err := r.exchange.exchangeABI.Unpack(&buEvent, "BalanceUpdate", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}
	wallet := models.Wallet{
		Token:         wrappers.WrapAddress(&buEvent.Token),
		Address:       wrappers.WrapAddress(&buEvent.User),
		Balance:       wrappers.WrapBigInt(buEvent.Balance),
		EscrowBalance: wrappers.WrapBigInt(buEvent.Escrow),
	}
	err = wallet.Save(r.store)
	if err != nil {
		log.Fatal("Commit: ", err)
	}
	fmt.Printf("\n\nUpdated %s token balance of wallet %s\n", buEvent.Token.Hex(), buEvent.User.Hex())
}

func (r *Relayer) placeOrderLogCallback(vLog types.Log, isBid bool) {
	placeOrderEvent := struct {
		OrderHash common.Hash
		Token     common.Address
		Base      common.Address
		Price     *big.Int
		Quantity  *big.Int
		// IsBid     bool
		Owner     common.Address
		Timestamp *big.Int
	}{}
	eventName := "PlaceBuyOrder"
	if !isBid {
		eventName = "PlaceSellOrder"
	}
	err := r.exchange.orderbookABI.Unpack(&placeOrderEvent, eventName, vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}
	order := models.Order{
		Hash:      wrappers.WrapHash(&placeOrderEvent.OrderHash),
		Token:     wrappers.WrapAddress(&placeOrderEvent.Token),
		Base:      wrappers.WrapAddress(&placeOrderEvent.Base),
		Price:     wrappers.WrapBigInt(placeOrderEvent.Price),
		Quantity:  wrappers.WrapBigInt(placeOrderEvent.Quantity),
		IsBid:     isBid, // placeOrderEvent.IsBid.Cmp(big.NewInt(1)) == 0,
		CreatedBy: wrappers.WrapAddress(&placeOrderEvent.Owner),
		CreatedAt: wrappers.WrapTimestamp((*(placeOrderEvent.Timestamp)).Uint64()),
		Volume:    wrappers.WrapBigInt(big.NewInt(0).Mul(placeOrderEvent.Price, placeOrderEvent.Quantity)),
		IsOpen:    true,
	}

	err = order.Save(r.store)
	if err != nil {
		log.Fatal("Commit: ", err)
	}

	channelKey := strings.ToLower(order.Token.Hex() + "/" + order.Base.Hex())
	pubCache := &redisChannelMessage{
		MessageType: "NEW_ORDER",
		Payload:     order,
	}
	marshalledResp, err := json.Marshal(pubCache)
	if err != nil {
		fmt.Println("MARSHAL:", err)
	}
	r.redisClient.Publish(channelKey, marshalledResp)

	fmt.Printf("\n\nReceived order at %s for pair %s/%s\n", placeOrderEvent.Timestamp.String(), placeOrderEvent.Token.Hex(), placeOrderEvent.Base.Hex())

	r.tryOrderMatching(&order)
}

func (r *Relayer) cancelOrderLogCallback(vLog types.Log) {
	cancelOrderEvent := struct {
		OrderHash common.Hash
	}{}
	err := r.exchange.orderbookABI.Unpack(&cancelOrderEvent, "CancelOrder", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}
	order := &models.Order{
		Hash: wrappers.WrapHash(&cancelOrderEvent.OrderHash),
	}

	err = order.Close(r.store)
	if err != nil {
		log.Fatal("Commit: ", err)
		return
	}

	err = order.Get(r.store)
	if err != nil {
		log.Fatal("Cannot get order: ", err)
		return
	}

	channelKey := strings.ToLower(order.Token.Hex() + "/" + order.Base.Hex())
	pubCache := &redisChannelMessage{
		MessageType: "CANCEL_ORDER",
		Payload:     order,
	}
	marshalledResp, err := json.Marshal(pubCache)
	if err != nil {
		fmt.Println("MARSHAL:", err)
	}
	r.redisClient.Publish(channelKey, marshalledResp)

	fmt.Printf("\n\nOrder cancelled/filled %s\n", cancelOrderEvent.OrderHash.Hex())
}

func (r *Relayer) tradeLogCallback(vLog types.Log) {
	tradeEvent := struct {
		BuyOrderHash  common.Hash
		SellOrderHash common.Hash
		Volume        *big.Int
		Timestamp     *big.Int
	}{}
	err := r.exchange.ordermatcherABI.Unpack(&tradeEvent, "Trade", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}
	sellOrder := &models.Order{
		Hash: wrappers.WrapHash(&tradeEvent.SellOrderHash),
	}
	err = sellOrder.Get(r.store)
	if err != nil {
		log.Fatal("Cannot get sell order: ", err)
		return
	}

	// buyOrder := &models.Order{
	// 	Hash: wrappers.WrapHash(&tradeEvent.BuyOrderHash),
	// }
	// err = buyOrder.Get(r.store)
	// if err != nil {
	// 	log.Fatal("Cannot get buy order: ", err)
	// 	return
	// }

	trade := models.Trade{
		BuyOrderHash:  wrappers.WrapHash(&tradeEvent.BuyOrderHash),
		SellOrderHash: wrappers.WrapHash(&tradeEvent.SellOrderHash),
		Volume:        wrappers.WrapBigInt(tradeEvent.Volume),
		TradedAt:      (*(tradeEvent.Timestamp)).Uint64(),
		TxHash:        wrappers.WrapHash(&vLog.TxHash),
		Token:         sellOrder.Token,
		Base:          sellOrder.Base,
		Price:         sellOrder.Price,
	}
	err = trade.Save(r.store)
	if err != nil {
		log.Fatal("Commit: ", err)
	}

	channelKey := strings.ToLower(trade.Token.Hex() + "/" + trade.Base.Hex())
	pubCache := &redisChannelMessage{
		MessageType: "TRADE",
		Payload:     trade,
	}
	marshalledResp, err := json.Marshal(pubCache)
	if err != nil {
		fmt.Println("MARSHAL:", err)
	}
	r.redisClient.Publish(channelKey, marshalledResp)

	fmt.Printf("\n\nReceived order match for %s/%s\n", tradeEvent.BuyOrderHash.Hex(), tradeEvent.SellOrderHash.Hex())
}

func (r *Relayer) updateFilledVolumeLogCallback(vLog types.Log) {
	updateFilledVolumeEvent := struct {
		OrderHash common.Hash
		Volume    *big.Int
	}{}
	err := r.exchange.ordermatcherABI.Unpack(&updateFilledVolumeEvent, "OrderFilledVolumeUpdate", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}
	order := &models.Order{
		Hash: wrappers.WrapHash(&updateFilledVolumeEvent.OrderHash),
	}
	order.Get(r.store)
	order.VolumeFilled = wrappers.WrapBigInt(updateFilledVolumeEvent.Volume)

	err = order.Update(r.store)
	if err != nil {
		log.Fatal("Commit: ", err)
		return
	}

	channelKey := strings.ToLower(order.Token.Hex() + "/" + order.Base.Hex())
	pubCache := &redisChannelMessage{
		MessageType: "ORDER_FILL",
		Payload:     order,
	}
	marshalledResp, err := json.Marshal(pubCache)
	if err != nil {
		fmt.Println("MARSHAL:", err)
	}
	r.redisClient.Publish(channelKey, marshalledResp)

	fmt.Printf("\n\nUpdate filled volume of order %s to %s\n", updateFilledVolumeEvent.OrderHash.Hex(), updateFilledVolumeEvent.Volume.String())
}

func (r *Relayer) withdrawSignSubmittedCallback(vLog types.Log) {
	withdrawSignEvent := struct {
		Authority common.Address
		Message   []byte
		Signature []byte
		Timestamp *big.Int
	}{}
	err := r.exchange.exchangeABI.Unpack(&withdrawSignEvent, "WithdrawSignatureSubmitted", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}

	withdrawSign := models.NewWithdrawSign()
	withdrawSign.Message = common.Bytes2Hex(withdrawSignEvent.Message)
	withdrawSign.Signature = common.Bytes2Hex(withdrawSignEvent.Signature)
	withdrawSign.Signer = wrappers.WrapAddress(&withdrawSignEvent.Authority)
	withdrawSign.SignedAt = wrappers.WrapTimestamp((*withdrawSignEvent.Timestamp).Uint64())

	_, _, _, txHash := utils.DeserializeMessage(withdrawSignEvent.Message)
	withdrawSign.TxHash = txHash.Hex()

	if err := withdrawSign.Save(r.store); err != nil {
		log.Fatal("COMMIT", err)
		return
	}

	fmt.Printf("\n\nWithdraw request %s signed by authority %s \n", withdrawSign.Message, withdrawSign.Signer.Hex())
}

func (r *Relayer) readyToWithdrawCallback(vLog types.Log) {
	withdrawEvent := struct {
		Message []byte
	}{}
	err := r.exchange.exchangeABI.Unpack(&withdrawEvent, "ReadyToWithdraw", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}

	_, _, _, txHash := utils.DeserializeMessage(withdrawEvent.Message)
	withdraw := models.NewWithdrawMeta()
	withdraw.TxHash = wrappers.WrapHash(txHash)
	// withdraw.Message = common.Bytes2Hex(withdrawEvent.Message)

	if err := withdraw.Get(r.store); err != nil {
		// HIGH ALERT
		log.Fatal("HIGH ALERT: POSSIBLE HACK: ", err)
		return
	}

	withdraw.Status = models.WITHDRAW_STATUS_SIGNED

	withdraw.UpdateStatus(r.store)

	fmt.Printf("\n\nWithdraw request %s is ready to be processed\n", withdraw.TxHash.Hex())
}

func (r *Relayer) dexWithdrawCallback(vLog types.Log) {
	withdrawEvent := struct {
		Recipient common.Address
		Token     common.Address
		Value     *big.Int
	}{}
	err := r.exchange.exchangeABI.Unpack(&withdrawEvent, "Withdraw", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}

	// message, err := utils.SerializeWithdrawalMessage(&withdrawEvent.Recipient, &withdrawEvent.Token, withdrawEvent.Value, &vLog.TxHash)

	// if err != nil {
	// 	log.Fatal("Serialize: ", err)
	// 	return
	// }

	withdraw := models.NewWithdrawMeta()
	withdraw.Recipient = wrappers.WrapAddress(&withdrawEvent.Recipient)
	withdraw.Token = wrappers.WrapAddress(&withdrawEvent.Token)
	withdraw.Amount = wrappers.WrapBigInt(withdrawEvent.Value)
	withdraw.TxHash = wrappers.WrapHash(&vLog.TxHash)
	// withdraw.Message = common.Bytes2Hex(message)
	withdraw.Status = models.WITHDRAW_STATUS_REQUESTED

	if err := withdraw.Save(r.store); err != nil {
		log.Fatal("COMMIT", err)
		return
	}

	fmt.Printf("\n\nCreated new withdraw request for wallet %s from tx %s\n", withdraw.Recipient.Hex(), withdraw.TxHash.Hex())
}

func (r *Relayer) bridgeWithdrawCallback(vLog types.Log) {
	fmt.Println("--------------------")
	// Unpack withdraw event
	fmt.Println("Received `Withdraw` event from Home Network")
	withdrawEvent := struct {
		Recipient       common.Address
		Token           common.Address
		Value           *big.Int
		TransactionHash common.Hash
	}{}
	err := r.bridge.abi.Unpack(&withdrawEvent, "Withdraw", vLog.Data)
	if err != nil {
		log.Fatal("Unpack: ", err)
		return
	}

	// message, err := utils.SerializeWithdrawalMessage(&withdrawEvent.Recipient, &withdrawEvent.Token, withdrawEvent.Value, &withdrawEvent.TransactionHash)

	// if err != nil {
	// 	log.Fatal("Serialize: ", err)
	// 	return
	// }

	withdraw := models.NewWithdrawMeta()
	withdraw.TxHash = wrappers.WrapHash(&withdrawEvent.TransactionHash)
	// withdraw.Message = common.Bytes2Hex(message)

	if err := withdraw.Get(r.store); err != nil {
		// HIGH ALERT
		log.Fatal("HIGH ALERT: POSSIBLE HACK: ", err)
		return
	}

	withdraw.Status = models.WITHDRAW_STATUS_PROCESSED
	withdraw.UpdateStatus(r.store)

	fmt.Println("Withdraw processed: ", withdrawEvent.TransactionHash.Hex())
	fmt.Println("--------------------")
}

func (r *Relayer) tryOrderMatching(order *models.Order) {
	matchingOrders, err := order.GetMatchingOrders(r.store)
	if err != nil {
		fmt.Println("MATCH_ORDER", err)
		return
	}

	orderHash := utils.ByteSliceToByte32(order.Hash.Bytes())
	volumeOfOrder := wrappers.WrapBigInt(big.NewInt(0))
	volumeOfOrder.SetBytes(order.Volume.Bytes())
	// zeroVolume := wrappers.WrapBigInt(big.NewInt(0))

	for _, matchedOrder := range matchingOrders {
		err = nil
		if orderClosed, err := r.redisClient.GetBit(matchedOrder.Hash.Hex(), 127).Result(); err == nil {
			if orderClosed == 1 {
				continue
			}
		}
		if order.IsBid {
			err = r.submitMatchedOrder(
				orderHash,
				utils.ByteSliceToByte32(matchedOrder.Hash.Bytes()),
			)
		} else {
			err = r.submitMatchedOrder(
				utils.ByteSliceToByte32(matchedOrder.Hash.Bytes()),
				orderHash,
			)
		}

		if err == nil {
			if order.Volume.Cmp(matchedOrder.VolumeLeft) >= 0 {
				r.redisClient.SetBit(matchedOrder.Hash.Hex(), 127, 1)
				volumeOfOrder.Sub(&volumeOfOrder.Int, &matchedOrder.VolumeLeft.Int)
			} else {
				r.redisClient.SetBit(order.Hash.Hex(), 127, 1)
				// volumeOfOrder.Sub(&volumeOfOrder.Int, &order.Volume.Int)
				fmt.Println("ORDER_FILLED")
				return
			}

			// tradedVolume := getMinVolume(order.Volume, matchedOrder.VolumeLeft)
			// volumeOfOrder.Sub(&volumeOfOrder.Int, &tradedVolume.Int)

			// if volumeOfOrder.Cmp(zeroVolume) == 0 {
			// 	fmt.Println("ORDER_FILLED")
			// 	return
			// }
		} else {
			fmt.Println(err)
		}
	}

	fmt.Println("ORDER_NOT_FULFILLED")
}

// func getMinVolume(a, b *wrappers.BigInt) *wrappers.BigInt {
// 	if a.Cmp(b) >= 0 {
// 		return b
// 	}
// 	return a
// }

func (r *Relayer) submitMatchedOrder(buyOrderHash [32]byte, sellOrderHash [32]byte) error {
	nonce, err := r.exchange.client.PendingNonceAt(context.Background(), *r.matcherAddress)
	if err != nil {
		log.Fatal("MATCHER_NONCE", err)
		return err
	}

	auth := bind.NewKeyedTransactor(r.matcherPrivateKey)

	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)
	auth.GasLimit = uint64(500000)
	auth.GasPrice = big.NewInt(0) // gasPrice

	tx, err := r.exchange.ordermatcherInstance.MatchOrders(
		auth,
		buyOrderHash,
		sellOrderHash,
	)
	if err != nil {
		return err
	}

	fmt.Println("tx match:", tx.Hash().Hex())

	// if receipt, err := r.exchange.client.TransactionReceipt(context.Background(), tx.Hash()); err != nil {
	// 	return err
	// } else if receipt.Status == 0 {
	// 	return errors.New("Transaction failed")
	// } else {
	// 	for _, vLog := range receipt.Logs {
	// 		for _, topic := range vLog.Topics {
	// 			switch topic {
	// 			case r.contracts.OrderMatcher.Topics.OrderFilledVolumeUpdate.Hash:
	// 				r.updateFilledVolumeLogCallback(*vLog)

	// 			case r.contracts.OrderMatcher.Topics.Trade.Hash:
	// 				r.tradeLogCallback(*vLog)
	// 			}
	// 		}
	// 	}
	// }

	return nil
}
