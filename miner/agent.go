// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"sync"

	"sync/atomic"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/log"
)
// 共识
type CpuAgent struct {
	mu sync.Mutex

	// 接收worker下发的任务
	workCh        chan *Work
	// 接收停止信息
	stop          chan struct{}
	quitCurrentOp chan struct{}
	// 反馈任务情况，挖出新的block
	returnCh      chan<- *Result

	// 访问本地节点的blockchain数据接口，获取各种信息
	chain  consensus.ChainReader
	// 共识引擎
	engine consensus.Engine

	isMining int32 // isMining indicates whether the agent is currently mining
}

// 创建一个新的CPUAGENT的对象
func NewCpuAgent(chain consensus.ChainReader, engine consensus.Engine) *CpuAgent {
	miner := &CpuAgent{
		chain:  chain,
		engine: engine,
		stop:   make(chan struct{}, 1),
		workCh: make(chan *Work, 1),
	}
	return miner
}

func (self *CpuAgent) Work() chan<- *Work            { return self.workCh }
func (self *CpuAgent) SetReturnCh(ch chan<- *Result) { self.returnCh = ch }

func (self *CpuAgent) Stop() {
	if !atomic.CompareAndSwapInt32(&self.isMining, 1, 0) {
		return // agent already stopped
	}
	self.stop <- struct{}{}
done:
	// Empty work channel
	for {
		select {
		case <-self.workCh:
		default:
			break done
		}
	}
}

// 启动agent工作线程
func (self *CpuAgent) Start() {
	if !atomic.CompareAndSwapInt32(&self.isMining, 0, 1) {
		return // agent already started
	}
	// 监听workCh和stop信道(linsten)
	go self.update()
}

func (self *CpuAgent) update() {
out:
	for {
		select {
		// 接收到了挖矿任务
		case work := <-self.workCh:
			self.mu.Lock()
			if self.quitCurrentOp != nil {
				close(self.quitCurrentOp)
			}
			self.quitCurrentOp = make(chan struct{})
			go self.mine(work, self.quitCurrentOp)
			self.mu.Unlock()
		case <-self.stop: //接收到停止
			self.mu.Lock()
			if self.quitCurrentOp != nil {
				close(self.quitCurrentOp)
				self.quitCurrentOp = nil
			}
			self.mu.Unlock()
			break out
		}
	}
}
// 进行挖矿
func (self *CpuAgent) mine(work *Work, stop <-chan struct{}) {
	if result, err := self.engine.Seal(self.chain, work.Block, stop); result != nil {
		log.Info("Successfully sealed new block", "number", result.Number(), "hash", result.Hash())
		self.returnCh <- &Result{work, result}
	} else {
		if err != nil {
			log.Warn("Block sealing failed", "err", err)
		}
		// 如果区块挖掘失败，返回一个空的结果
		self.returnCh <- nil
	}
}

func (self *CpuAgent) GetHashRate() int64 {
	if pow, ok := self.engine.(consensus.PoW); ok {
		return int64(pow.Hashrate())
	}
	return 0
}
