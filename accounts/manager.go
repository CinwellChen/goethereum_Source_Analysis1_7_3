// Copyright 2017 The go-ethereum Authors
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

package accounts

import (
	"reflect"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/event"
)

// Manager is an overarching account manager that can communicate with various
// backends for signing transactions.
// 账户管理工具，可以和所有的backends进行通信来签名交易
type Manager struct {
	// 已注册的后台服务
	backends map[reflect.Type][]Backend // Index of backends currently registered
	// 钱包管理相关事件
	updaters []event.Subscription       // Wallet update subscriptions for all backends
	updates  chan WalletEvent           // Subscription sink for backend wallet changes
	// 已经注册过的钱包缓存
	wallets  []Wallet                   // Cache of all wallets from all registered backends

	feed event.Feed // Wallet feed notifying of arrivals/departures

	quit chan chan error
	lock sync.RWMutex
}

// NewManager creates a generic account manager to sign transaction via various
// supported backends.
// 新建管理器对象
func NewManager(backends ...Backend) *Manager {
	// Retrieve the initial list of wallets from the backends and sort by URL
	var wallets []Wallet
	for _, backend := range backends {
		// 调用所有backend的wallet方法，合并成完整的钱包列表
		wallets = merge(wallets, backend.Wallets()...)
	}
	// Subscribe to wallet notifications from all backends
	updates := make(chan WalletEvent, 4*len(backends))

	subs := make([]event.Subscription, len(backends))
	for i, backend := range backends {
		// 注册update channel到后端服务中
		subs[i] = backend.Subscribe(updates)
	}
	// Assemble the account manager and return
	// 封装
	am := &Manager{
		backends: make(map[reflect.Type][]Backend),
		updaters: subs,
		updates:  updates,
		wallets:  wallets,
		quit:     make(chan chan error),
	}
	for _, backend := range backends {
		kind := reflect.TypeOf(backend)
		am.backends[kind] = append(am.backends[kind], backend)
	}

	// 另起协程，监听钱包事件
	go am.update()

	return am
}

// Close terminates the account manager's internal notification processes.
// 关闭账号管理器
func (am *Manager) Close() error {
	errc := make(chan error)
	am.quit <- errc
	return <-errc
}

// update is the wallet event loop listening for notifications from the backends
// and updating the cache of wallets.
// 钱包事件
func (am *Manager) update() {
	// Close all subscriptions when the manager terminates
	defer func() {
		am.lock.Lock()
		for _, sub := range am.updaters {
			sub.Unsubscribe()
		}
		am.updaters = nil
		am.lock.Unlock()
	}()

	// Loop until termination
	// 循环监听钱包相关事件
	for {
		select {
		case event := <-am.updates:
			// Wallet event arrived, update local cache
			am.lock.Lock()
			switch event.Kind {
			// 判断事件类型
			case WalletArrived:
				am.wallets = merge(am.wallets, event.Wallet)
			case WalletDropped:
				am.wallets = drop(am.wallets, event.Wallet)
			}
			am.lock.Unlock()

			// Notify any listeners of the event
			am.feed.Send(event)
		// 接收退出
		case errc := <-am.quit:
			// Manager terminating, return
			errc <- nil
			return
		}
	}
}

// Backends retrieves the backend(s) with the given type from the account manager.
// 返回指定的服务列表
func (am *Manager) Backends(kind reflect.Type) []Backend {
	return am.backends[kind]
}

// Wallets returns all signer accounts registered under this account manager.
// 返回该账号管理器下的所有签名账户
func (am *Manager) Wallets() []Wallet {
	am.lock.RLock()
	defer am.lock.RUnlock()

	cpy := make([]Wallet, len(am.wallets))
	copy(cpy, am.wallets)
	return cpy
}

// Wallet retrieves the wallet associated with a particular URL.
// 通过URL查找指定的钱包
func (am *Manager) Wallet(url string) (Wallet, error) {
	am.lock.RLock()
	defer am.lock.RUnlock()

	parsed, err := parseURL(url)
	if err != nil {
		return nil, err
	}
	for _, wallet := range am.Wallets() {
		if wallet.URL() == parsed {
			return wallet, nil
		}
	}
	return nil, ErrUnknownWallet
}

// Find attempts to locate the wallet corresponding to a specific account. Since
// accounts can be dynamically added to and removed from wallets, this method has
// a linear runtime in the number of wallets.
// 通过指定的ACCOUNT查找钱包
func (am *Manager) Find(account Account) (Wallet, error) {
	am.lock.RLock()
	defer am.lock.RUnlock()

	for _, wallet := range am.wallets {
		if wallet.Contains(account) {
			return wallet, nil
		}
	}
	return nil, ErrUnknownAccount
}

// Subscribe creates an async subscription to receive notifications when the
// manager detects the arrival or departure of a wallet from any of its backends.
// 订阅事件
func (am *Manager) Subscribe(sink chan<- WalletEvent) event.Subscription {
	return am.feed.Subscribe(sink)
}

// merge is a sorted analogue of append for wallets, where the ordering of the
// origin list is preserved by inserting new wallets at the correct position.
//
// The original slice is assumed to be already sorted by URL.
// 在指定的位置插入钱包，保证原来列表的顺序
func merge(slice []Wallet, wallets ...Wallet) []Wallet {
	for _, wallet := range wallets {
		n := sort.Search(len(slice), func(i int) bool { return slice[i].URL().Cmp(wallet.URL()) >= 0 })
		if n == len(slice) {
			slice = append(slice, wallet)
			continue
		}
		slice = append(slice[:n], append([]Wallet{wallet}, slice[n:]...)...)
	}
	return slice
}

// drop is the couterpart of merge, which looks up wallets from within the sorted
// cache and removes the ones specified.
// 删除钱包列表中指定的钱包
func drop(slice []Wallet, wallets ...Wallet) []Wallet {
	for _, wallet := range wallets {
		n := sort.Search(len(slice), func(i int) bool { return slice[i].URL().Cmp(wallet.URL()) >= 0 })
		if n == len(slice) {
			// Wallet not found, may happen during startup
			continue
		}
		slice = append(slice[:n], slice[n+1:]...)
	}
	return slice
}
