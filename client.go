package gee_rpc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"geerpc/codec"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrShutdown = errors.New("connection is shut down")
)

// Call 承载一次RPC调用中所需要的信息
type Call struct {
	Seq uint64
	// ServiceMethod
	ServiceMethod string
	// Args 方法的参数
	Args interface{}
	// Reply 方法的回答
	Reply interface{}
	Error error
	// Done call结束时候的通知
	Done chan *Call
}

// done 支持异步调用，当调用结束时，会调用call.done()通知调用方
func (call *Call) done() {
	call.Done <- call
}

// Client 表示一个 RPC客户端。一个客户端可能有多个未完成的呼叫(Call)，
//一个客户端可能同时被多个 goroutine 使用，所以需要加互斥锁
type Client struct {
	// cc 消息的编解码器，和服务端类似，用来序列化将要发出去的请求，以及反序列化接收到的响应
	cc codec.Codec
	//头部 用JSON解码
	opt *Option
	// sending 互斥锁，为了保证请求的有序发送，防止多个请求报文混淆
	sending sync.Mutex // protect following
	// header 每个请求的消息头，header只有在请求发送时才需要，而请求发送是互斥的，因此每个客户端只需要一个，声明在Client结构体中可以复用
	header codec.Header
	mu     sync.Mutex // protect following
	// seq 用于给发送的请求编号，每个请求拥有唯一编号
	seq uint64
	// pending 存储未处理完的请求，键是编号，值是Call实例
	pending map[uint64]*Call
	// closing 和 shutdown任意一个值为true,则表示clinet处于不可用的状态
	// closing 是用户主动关闭的，即调用close方法，而shutdown置为true一般有错误发送
	closing  bool
	shutdown bool // server has told us to stop
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	// 已经关过了
	if client.closing {
		return ErrShutdown
	}
	client.closing = true
	return client.cc.Close()
}

// IsAvailable 判断当前client是否可用
func (client *Client) IsAvailable() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return !client.shutdown && !client.closing
}

// registerCall 将参数call添加到client.pending中，并更新client.seq
func (client *Client) registerCall(call *Call) (uint64, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing || client.shutdown {
		return 0, ErrShutdown
	}
	//可复用的Call,更新Seq
	call.Seq = client.seq
	client.pending[call.Seq] = call
	client.seq++
	return call.Seq, nil
}

// removeCall 在client的map中删除Call
func (client *Client) removeCall(seq uint64) *Call {
	client.mu.Lock()
	defer client.mu.Unlock()
	call := client.pending[seq]
	delete(client.pending, seq)
	return call
}

// terminateCalls 服务端或客户端发生错误时调用
func (client *Client) terminateCalls(err error) {
	client.sending.Lock()
	defer client.sending.Unlock()
	client.mu.Lock()
	defer client.mu.Unlock()
	// 将 shutdown 设置为 true
	client.shutdown = true
	// 将错误信息通知所有 pending 状态的 call。
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
}

/*
	对一个客户端来说，接受响应、发送请求是最重要的2个功能。那么首先实现接受功能，接收到的响应有三种情况:
	- call不存在，可能是请求没有发送完整，或者因为其他原因取消，但是服务器仍旧处理了
	- call存在，但服务器处理出错，即h.Error不为空
	- call存在，服务端处理正常，那么需要从body中读取Reply的值
*/
func (client *Client) receive() {
	var err error
	for err == nil {
		var h codec.Header
		if err = client.cc.ReadHeader(&h); err != nil {
			break
		}
		call := client.removeCall(h.Seq)
		switch {
		// call为nil，可能是请求没有发送完整，或者因为其他原因被取消，但是服务端仍旧处理了。
		case call == nil:
			err = client.cc.ReadBody(nil)
		//call存在，但服务端处理出错，即h.Error不为空
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			err = client.cc.ReadBody(nil)
			call.done()
		default:
			// 默认会读取call中的reply
			err = client.cc.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			//执行完通知调用方
			call.done()
		}

	}
	client.terminateCalls(err)

}

/*
	创建client实例时，首先需要完成一开始的协议交换，即发送option信息给服务端。
	协商好消息的编码方式之后，再创建一个子协程调用receive()接受响应
*/
func NewClient(conn net.Conn, opt *Option) (*Client, error) {
	//找到编解码器
	f := codec.NewCodeFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid code type %s", opt.CodecType)
		log.Println("rpc client:codec error:", err)
		return nil, err
	}
	return newClientCode(f(conn), opt), nil
}
func newClientCode(cc codec.Codec, opt *Option) *Client {
	client := &Client{
		seq:     1, // seq starts with 1, 0 means invalid call
		cc:      cc,
		opt:     opt,
		pending: make(map[uint64]*Call),
	}
	//启动一个协程进行receive处理
	go client.receive()
	return client
}

// parseOptions  连接时创建option，如果不传则调用默认option
func parseOptions(opts ...*Option) (*Option, error) {
	// if opts is nil or pass nil as parameter
	if len(opts) == 0 || opts[0] == nil {
		// 返回默认的消息编解码Option
		return DefaultOption, nil
	}
	if len(opts) != 1 {
		return nil, errors.New("number of options is more than 1")
	}
	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	//如果没设置编码方式，则选择默认的
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}
	return opt, nil
}

type clientResult struct {
	client *Client
	err    error
}

type newClientFunc func(conn net.Conn, opt *Option) (client *Client, err error)

// Dial 连接到指定网络地址的 RPC 服务器,使用编解码器 Option
func dialTimeout(f newClientFunc, network, address string, opts ...*Option) (client *Client, err error) {
	// 获得一个编解码器
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	//连接到指定网络上的地址
	conn, err := net.DialTimeout(network, address, opt.ConnectTimeout)

	if err != nil {
		return nil, err
	}
	// 最后关闭连接
	defer func() {
		if client == nil {
			_ = conn.Close()
		}
	}()
	ch := make(chan clientResult)
	go func() {
		client, err := f(conn, opt)
		ch <- clientResult{client: client, err: err}
	}()
	if opt.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}
	select {
	case <-time.After(opt.ConnectTimeout):
		return nil, fmt.Errorf("rpc client: connect timeout: expect within %s", opt.ConnectTimeout)
	case result := <-ch:
		return result.client, result.err
	}
}

// Dial connects to an RPC server at the specified network address
func Dial(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewClient, network, address, opts...)
}

func (client *Client) send(call *Call) {
	// 确保client发送完整的请求
	client.sending.Lock()
	defer client.sending.Unlock()
	// 注册 call
	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}
	// 准备请求的Header
	client.header.ServiceMethod = call.ServiceMethod
	client.header.Seq = seq
	client.header.Error = ""
	// encode and send the request
	if err := client.cc.Write(&client.header, call.Args); err != nil {
		//发送完清空
		call := client.removeCall(seq)
		// call may be nil, it usually means that Write partially failed,
		// client has received the response and handled
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

// Go 客户端暴露给用户的RPC调用接口，Go是一个异步接口
// 它返回表示调用的 Call 结构。
func (client *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("rpc client: done channel is unbuffered")
	}
	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

// Call 调用命名的函数，等待它完成,
// 并返回其错误状态。
func (client *Client) Call(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	select {
	case <-ctx.Done():
		client.removeCall(call.Seq)
		return errors.New("rpc client: call failed: " + ctx.Err().Error())
	case call := <-call.Done:
		return call.Error
	}
}

// NewHTTPClient new a Client instance via HTTP as transport protocol
func NewHTTPClient(conn net.Conn, opt *Option) (*Client, error) {
	_, _ = io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.0\n\n", defaultRPCPath))

	// Require successful HTTP response
	// before switching to RPC protocol.
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && resp.Status == connected {
		return NewClient(conn, opt)
	}
	if err == nil {
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	return nil, err
}

// DialHTTP connects to an HTTP RPC server at the specified network address
// listening on the default HTTP RPC path.
func DialHTTP(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewHTTPClient, network, address, opts...)
}

// 表示Client 实现了 io.Closer接口
//var _ io.Closer = (*Client)(nil)

// XDial calls different functions to connect to a RPC server
// according the first parameter rpcAddr.
// rpcAddr is a general format (protocol@addr) to represent a rpc server
// eg, http@10.0.0.1:7001, tcp@10.0.0.1:9999, unix@/tmp/geerpc.sock
func XDial(rpcAddr string, opts ...*Option) (*Client, error) {
	parts := strings.Split(rpcAddr, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("rpc client err: wrong format '%s', expect protocol@addr", rpcAddr)
	}
	protocol, addr := parts[0], parts[1]
	switch protocol {
	case "http":
		return DialHTTP("tcp", addr, opts...)
	default:
		// tcp, unix or other transport protocol
		return Dial(protocol, addr, opts...)
	}
}
