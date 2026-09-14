package main

import (
	"sync"
)

// subBuffer 是单个 SSE 订阅者的实时事件缓冲。
const subBuffer = 256

// Broker 把首次落库的事件扇出给所有在线 SSE 订阅者。
// 它只管实时分发；断线期间的补发始终以 SQLite 中按 seq 的查询为准，
// 因此即使实时通道异常，客户端重连后仍能从库中完整恢复。
type Broker struct {
	mu   sync.Mutex
	subs map[uint64]chan Event
	next uint64
}

func NewBroker() *Broker {
	return &Broker{subs: make(map[uint64]chan Event)}
}

// Subscribe 注册一个订阅者，返回事件通道与退订函数。
// 必须先订阅、再查库补发，二者交叠窗口内的事件靠 seq 去重。
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)
	b.mu.Lock()
	b.next++
	id := b.next
	b.subs[id] = ch
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

// Publish 向所有订阅者非阻塞投递。订阅者缓冲满时直接踢掉该连接，
// 迫使客户端带着最后已显示序号重连，从数据库完整补发，而不是悄悄丢事件。
func (b *Broker) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(b.subs, id)
		}
	}
}
