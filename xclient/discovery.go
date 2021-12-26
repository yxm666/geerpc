package xclient

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	//RandomSelect 随机选择策略
	RandomSelect SelectMode = iota
	// RoundRobinSelect 轮询算法
	RoundRobinSelect
)

type SelectMode int

// Discovery 是一个接口类型，包含了服务发现所需要的最基本的接口
type Discovery interface {
	// Refresh 从注册中心更新服务列表
	Refresh() error
	// update 手动更新服务列表
	update(server []string) error
	// Get 根据负载均衡策略，选择一个服务实例
	Get(mode SelectMode) (string, error)
	// GetAll 返回所有的服务实例
	GetAll() ([]string, error)
}

type MultiServersDiscovery struct {
	// r 一个产生随机数的实例，初始化使用时间戳设定随机数种子，避免每次产生相同的随机数序列
	r       *rand.Rand
	mu      sync.RWMutex
	servers []string
	// index 记录 轮询算法已经轮询到的位置，为了避免每次从0开始，初始化时随机设定一个值
	index int
}

// NewMultiServerDiscovery creates a MultiServersDiscovery instance
func NewMultiServerDiscovery(servers []string) *MultiServersDiscovery {
	d := &MultiServersDiscovery{
		servers: servers,
		r:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	d.index = d.r.Intn(math.MaxInt32 - 1)
	return d
}

// Refresh doesn't make sense for MultiServersDiscovery, so ignore it
func (d *MultiServersDiscovery) Refresh() error {
	return nil
}

// Update the servers of discovery dynamically if needed
func (d *MultiServersDiscovery) Update(servers []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.servers = servers
	return nil
}

// Get a server according to mode
func (d *MultiServersDiscovery) Get(mode SelectMode) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.servers)
	if n == 0 {
		return "", errors.New("rpc discovery: no available servers")
	}
	switch mode {
	case RandomSelect:
		return d.servers[d.r.Intn(n)], nil
	case RoundRobinSelect:
		s := d.servers[d.index%n] // servers could be updated, so mode n to ensure safety
		d.index = (d.index + 1) % n
		return s, nil
	default:
		return "", errors.New("rpc discovery: not supported select mode")
	}
}

// returns all servers in discovery
func (d *MultiServersDiscovery) GetAll() ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// return a copy of d.servers
	servers := make([]string, len(d.servers), len(d.servers))
	copy(servers, d.servers)
	return servers, nil
}
