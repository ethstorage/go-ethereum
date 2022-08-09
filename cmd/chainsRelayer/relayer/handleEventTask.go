package relayer

import (
	"context"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/syndtr/goleveldb/leveldb"
	"math/big"
	"time"
)

type ListenTask interface {
	running(co *ChainOperator)
	isStart() bool
	stop()
}

// 如果receive chan 不为nil ，则代表这个event启用单独的处理线程，否则一律使用ChainOperator.LogsReceiveChan
type HandleEventTask struct {
	start                  bool
	address                common.Address
	eventName              string
	independentReceiveChan chan types.Log
	sub                    ethereum.Subscription
	handleFunc             func(log2 types.Log)

	ctx        context.Context
	cancleFunc func()
}

func NewHandleEventTask(address common.Address, eventName string, recChan chan types.Log, subscription ethereum.Subscription, sCtx context.Context, cf func()) *HandleEventTask {
	return &HandleEventTask{start: true, address: address, eventName: eventName, independentReceiveChan: recChan, sub: subscription, ctx: sCtx, cancleFunc: cf}
}

func (he *HandleEventTask) setIndependentReceiveChan(c chan types.Log) {
	he.independentReceiveChan = c
}

func (task *HandleEventTask) isStart() bool {
	return task.start
}

func (task *HandleEventTask) stop() {
	task.start = false
	task.sub.Unsubscribe()
	task.cancleFunc()
}

func (task *HandleEventTask) running(co *ChainOperator) {
	for {
		select {
		case log := <-task.independentReceiveChan:
			co.config.logger.Info("receive log", "event", task.eventName, "Log Info", log)
			if task.handleFunc != nil {
				task.handleFunc(log)
			}
		case err := <-task.sub.Err():
			co.config.logger.Error("receive error", "event", task.eventName, "err", err)
			co.reSubscribeEvent(task)
		case <-task.ctx.Done():
			delete(co.contracts[task.address].HandleEventList, co.contracts[task.address].getEventId(task.eventName))
			task.sub.Unsubscribe()
			task.start = false

			co.config.logger.Info("listen task quit ", "event", task.eventName)
			return
		default:
			co.config.logger.Info("listen task working", "event", task.eventName)
			time.Sleep(10 * time.Second)

		}
	}
}

type CommonListenTask struct {
	name        string
	start       bool
	ctx         context.Context
	cancleFunc  func()
	receiveChan chan interface{}
	handleFunc  func(val interface{})
	sub         ethereum.Subscription
}

func NewCommonListenTask(name string, ctx context.Context, cf func(), receiveChan chan interface{}, sub ethereum.Subscription, handleFunc func(val interface{})) *CommonListenTask {
	return &CommonListenTask{name: name, start: true, ctx: ctx, cancleFunc: cf, receiveChan: receiveChan, sub: sub, handleFunc: handleFunc}
}

const Web3QBlockTime = 6 * time.Second

func getLatestHeadLoop(headChan chan interface{}, w3q *ChainOperator) func(<-chan struct{}) error {
	return func(unSubscribe <-chan struct{}) error {
		for {
			select {
			case <-unSubscribe:
				log.Info("unSubscribe latest head")
				return nil
			default:
				// don't do anything
			}

			// get latestBlockNumber
			latestHead, err := w3q.clientExecutor.HeaderByNumber(w3q.Ctx, nil)
			if err != nil {
				return err
			}
			w3q.config.logger.Info("getLatestHeadLoop:get latest head....", "headNumber", latestHead.Number.String())

			//if latestHead.Number.Cmp(big.NewInt(0)) != 0 {
			// store the blockNumber of lastestHeader in db
			key := []byte("latestHead")
			bn, dberr := w3q.db.Get(key)
			if dberr != nil {
				if dberr != leveldb.ErrNotFound {
					w3q.config.logger.Error("getLatestHeadLoop:get db error", "error", dberr.Error())
					return err
				}
			}

			//Verify whether the latest block header is stored in the database
			if big.NewInt(0).SetBytes(bn).Cmp(latestHead.Number) < 0 {
				w3q.config.logger.Info("getLatestHeadLoop:set latestHeader number at db", "headNumber", latestHead.Number.String())
				headChan <- latestHead
				err = w3q.db.Put(key, latestHead.Number.Bytes())
				if err != nil {
					return err
				}
			}
			w3q.config.logger.Info("getLatestHeadLoop:sleep", "headNumber", latestHead.Number.String())

			time.Sleep(Web3QBlockTime)
		}
	}
}

func CreateListeningW3qLatestBlockTask(w3q *ChainOperator, eth *ChainOperator, lightClientAddr common.Address, ctx context.Context) (*CommonListenTask, error) {

	subCtx, cancelFunc := context.WithCancel(ctx)
	headChan := make(chan interface{}, 0)

	// getLatestHead will be executed here
	subscription := event.NewSubscription(getLatestHeadLoop(headChan, w3q))

	// the blockNumber should be submited to LightClient Contract
	return NewCommonListenTask("Get W3qLatestBlock Task", subCtx, cancelFunc, headChan, subscription, eth.sendSubmitHeadTxOnce(w3q, lightClientAddr)), nil

}

func (task *CommonListenTask) running(co *ChainOperator) {
	for {
		select {
		case val := <-task.receiveChan:
			co.config.logger.Info("receive val", "task name", task.name, "val Info", val)
			if task.handleFunc != nil {
				task.handleFunc(val)
			}
		case err := <-task.sub.Err():
			co.config.logger.Error("receive error", "task name", task.name, "err", err)
			task.sub.Unsubscribe()
			return
		case <-task.ctx.Done():
			task.sub.Unsubscribe()
			//task.start = false
			co.config.logger.Info("listen task quit ", "task name", task.name)
			return
		default:
			co.config.logger.Info("listen task working", "task name", task.name)
			time.Sleep(10 * time.Second)

		}
	}
}

func (task *CommonListenTask) isStart() bool {
	return task.start
}

func (task *CommonListenTask) stop() {
	if !task.start {
		return
	}
	task.start = false
	if task.cancleFunc != nil {
		task.cancleFunc()
	}
}
