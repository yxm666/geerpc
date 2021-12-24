package geerpc

import (
	"errors"
	"fmt"
	"gee-rpc/codec"
	"log"
	"net"
	"sync"
)

var (
	ErrShutdown = errors.New("connection is shut down")
)

// Call 表示一个活动的RPC
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

type Client struct {
	// cc 消息的编解码器，和服务端类似，用来序列化将要发出去的请求，以及反序列化接收到的响应
	cc  codec.Codec
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
	closing  bool // user has called Close
	shutdown bool // server has told us to stop
}

/*
	创建client实例时，首先需要完成一开始的协议交换，即发送option信息给服务端。
	协商好消息的编码方式之后，再创建一个子协程调用receive()接受响应
*/
func NewClient(conn net.Conn, opt *Option) (*Client, error) {
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
	go client.receive()
	return client
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing {
		return ErrShutdown
	}
	client.closing = true
	return client.cc.Close()
}

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
	call.Seq = client.seq
	client.pending[call.Seq] = call
	client.seq++
	return call.Seq, nil
}

func (client *Client) removeCall(seq uint64) *Call {
	client.mu.Lock()
	defer client.mu.Unlock()
	call := client.pending[seq]
	delete(client.pending, seq)
	return call
}

func (client *Client) terminateCalls(err error) {
	client.sending.Lock()
	defer client.sending.Unlock()
	client.mu.Lock()
	defer client.mu.Unlock()
	client.shutdown = true
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
		case call == nil:
			err = client.cc.ReadBody(nil)
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			err = client.cc.ReadBody(nil)
			call.done()
		default:
			err = client.cc.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}

	}
	client.terminateCalls(err)

}

func parseOptions(opts ...*Option) (*Option, error) {
	// if opts is nil or pass nil as parameter
	if len(opts) == 0 || opts[0] == nil {
		return DefaultOption, nil
	}
	if len(opts) != 1 {
		return nil, errors.New("number of options is more than 1")
	}
	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}
	return opt, nil
}

// Dial connects to an RPC server at the specified network address
func Dial(network, address string, opts ...*Option) (client *Client, err error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	// close the connection if client is nil
	defer func() {
		if client == nil {
			_ = conn.Close()
		}
	}()
	return NewClient(conn, opt)
}
func (client *Client) send(call *Call) {
	client.sending.Lock()
	defer client.sending.Unlock()
	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}
	// prepare request header
	client.header.ServiceMethod = call.ServiceMethod
	client.header.Seq = seq
	client.header.Error = ""
	// encode and send the request
	if err := client.cc.Write(&client.header, call.Args); err != nil {
		call := client.removeCall(seq)
		// call may be nil, it usually means that Write partially failed,
		// client has received the response and handled
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

// Go invokes the function asynchronously.
// It returns the Call structure representing the invocation.
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

// Call invokes the named function, waits for it to complete,
// and returns its error status.
func (client *Client) Call(serviceMethod string, args, reply interface{}) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	return call.Error
}

// 表示Client 实现了 io.Closer接口
//var _ io.Closer = (*Client)(nil)
