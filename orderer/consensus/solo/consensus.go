/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package solo

import (
	"time"

	"github.com/hyperledger/fabric/orderer/common/msgprocessor"
	"github.com/hyperledger/fabric/orderer/consensus"
	cb "github.com/hyperledger/fabric/protos/common"
	"github.com/op/go-logging"
)

var logger = logging.MustGetLogger("orderer/solo")

type consenter struct{}

type chain struct {
	support  consensus.ConsenterSupport
	sendChan chan *cb.Envelope
	exitChan chan struct{}
}

// New creates a new consenter for the solo consensus scheme.
// The solo consensus scheme is very simple, and allows only one consenter for a given chain (this process).
// It accepts messages being delivered via Enqueue, orders them, and then uses the blockcutter to form the messages
// into blocks before writing to the given ledger
func New() consensus.Consenter {
	return &consenter{}
}

func (solo *consenter) HandleChain(support consensus.ConsenterSupport, metadata *cb.Metadata) (consensus.Chain, error) {
	return newChain(support), nil
}

func newChain(support consensus.ConsenterSupport) *chain {
	return &chain{
		support:  support,
		sendChan: make(chan *cb.Envelope),
		exitChan: make(chan struct{}),
	}
}

func (ch *chain) Start() {
	go ch.main()
}

func (ch *chain) Halt() {
	select {
	case <-ch.exitChan:
		// Allow multiple halts without panic
	default:
		close(ch.exitChan)
	}
}

// Enqueue accepts a message and returns true on acceptance, or false on shutdown
func (ch *chain) Enqueue(env *cb.Envelope) bool {
	select {
	case ch.sendChan <- env:
		return true
	case <-ch.exitChan:
		return false
	}
}

// Errored only closes on exit
func (ch *chain) Errored() <-chan struct{} {
	return ch.exitChan
}

func (ch *chain) main() {
	var timer <-chan time.Time

	for {
		select {
		case msg := <-ch.sendChan:
			class, err := ch.support.ClassifyMsg(msg)
			if err != nil {
				logger.Panicf("If a message has arrived to this point, it should already have been classified once")
			}
			switch class {
			case msgprocessor.ConfigUpdateMsg:
				batch := ch.support.BlockCutter().Cut()
				if batch != nil {
					block := ch.support.CreateNextBlock(batch)
					ch.support.WriteBlock(block, nil)
				}

				_, err := ch.support.ProcessNormalMsg(msg)
				if err != nil {
					logger.Warningf("Discarding bad config message: %s", err)
					continue
				}
				block := ch.support.CreateNextBlock([]*cb.Envelope{msg})
				ch.support.WriteConfigBlock(block, nil)
				timer = nil
			case msgprocessor.NormalMsg:
				batches, ok := ch.support.BlockCutter().Ordered(msg)
				if ok && len(batches) == 0 && timer == nil {
					timer = time.After(ch.support.SharedConfig().BatchTimeout())
					continue
				}
				for _, batch := range batches {
					block := ch.support.CreateNextBlock(batch)
					ch.support.WriteBlock(block, nil)
				}
				if len(batches) > 0 {
					timer = nil
				}
			default:
				logger.Panicf("Unsupported msg classification: %v", class)
			}
		case <-timer:
			//clear the timer
			timer = nil

			batch := ch.support.BlockCutter().Cut()
			if len(batch) == 0 {
				logger.Warningf("Batch timer expired with no pending requests, this might indicate a bug")
				continue
			}
			logger.Debugf("Batch timer expired, creating block")
			block := ch.support.CreateNextBlock(batch)
			ch.support.WriteBlock(block, nil)
		case <-ch.exitChan:
			logger.Debugf("Exiting")
			return
		}
	}
}
