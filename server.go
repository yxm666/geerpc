package gee_rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"geerpc/codec"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

const (
	MagicNumber      = 0x3bef5c
	connected        = "200 Connected to Gee RPC"
	defaultRPCPath   = "/_geeprc_"
	defaultDebugPath = "/debug/geerpc"
)

/*
	RPC 服务端的实现
	GeeRpc客户端固定采用JSON编码Option，后续的header和Body的编码方式由Option中的CodeType指定
	服务端首先使用JSON编码Option，然后通过Option的CodeType解码剩余的内容
	| Option{MagicNumber: xxx, CodecType: xxx} | Header{ServiceMethod ...} | Body interface{} |
	| <------      固定 JSON 编码      ------>  | <-------   编码方式由 CodeType 决定   ------->
	在一次链接中，Option固定在报文的最开始，Header和Body可以由多个
	| Option | Header1 | Body1 | Header2 | Body2 | ...
*/

// Option 消息的编解码方式 默认使用JSON编码Option
type Option struct {
	MagicNumber int
	// 决定后续Header和 Body的编解码方式
	CodecType codec.Type
	// 默认值为10S
	ConnectTimeout time.Duration
	// 默认值为0，即不设限
	HandleTimeout time.Duration
}

// Server 作为RPC的服务端
type Server struct {
	serviceMap sync.Map
}

// request 存储一个请求的所有信息
type request struct {
	// Header 里包含的有服务名和方法名、seq(唯一标识请求)、Err
	h *codec.Header
	// 参数和
	argv, replyv reflect.Value
	mtype        *methodType
	svc          *service
}

var (
	// DefaultServer 是默认的*Server实例 为了用户使用方便
	DefaultServer = NewServer()
	// DefaultOption 是默认的Option实例
	DefaultOption = &Option{
		MagicNumber:    MagicNumber,
		CodecType:      codec.GobType,
		ConnectTimeout: time.Second * 10,
	}
)

// NewServer 返回一个新的server
func NewServer() *Server {
	return &Server{}
}

// Accept net.Listener作为参数，for循环等待socket连接建立
// 并开启子协程处理，处理过程交给了 ServerConn 方法
func (server *Server) Accept(lis net.Listener) {
	/*
		启动服务步骤:
			lis,_ := net.Listen("tcp","9999")
			geerpc.Accept(lis)
	*/
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Println("rpc server : accept error: ", err)
			return
		}
		// 实现了通信过程
		go server.ServeConn(conn)

	}
}

// Accept 为每一个连接的请求，接受侦听器上的连接并提供请求
func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

// ServerConn 实现通信过程
func (server *Server) ServeConn(conn io.ReadWriteCloser) {
	defer func() { _ = conn.Close() }()
	var opt Option
	//NewDecoder 返回一个新的解码器，该解码器从 r 读取数据
	// 首先使用json.NewDecoder 反序列化得到Option实例（option默认使用JSON进行解码）
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("rpc server option  error:", err)
	}
	// 检查MagicNumber 和 CodeType的值是否正确
	if opt.MagicNumber != MagicNumber {
		log.Printf("rpc server invalid magic number: %x", opt.MagicNumber)
	}
	// 根据CodecType 得到对应的消息编解码器
	f := codec.NewCodeFuncMap[opt.CodecType]
	if f == nil {
		log.Printf("rpc server: invalid codec type %s", opt.CodecType)
		return
	}
	// 接下来的处理交给 serveCodec
	server.serveCodec(f(conn), &opt)
}

// invalidRequest 是响应 argv 发生错误时的占位符
var invalidRequest = struct{}{}

// serveCodec 主要包含三个阶段
func (server *Server) serveCodec(cc codec.Codec, opt *Option) {
	// 确保发送完整的响应
	sending := new(sync.Mutex)
	// 等待，直到所有的请求都被处理
	wg := new(sync.WaitGroup)
	/* 在一次连接中，允许接受多个请求，即多个request header 和 request body，
	   使用for无限制等待请求的到来，直到发生错误(关闭or报文有问题)

	   注意:- handleRequest使用了协程并发执行请求
	   	-  处理请求是并发的，但是回复请求的报文必须是逐个发送的，并发容易导致多个回复报文交织在一期，客户端无法解析。使用锁(sending)保证
	       - 尽力而为，只有在 header解析失败时，才终止循环
	*/
	for {
		// 读取请求
		req, err := server.readRequest(cc)
		if err != nil {
			// header解析失败 退出循环
			if req == nil {
				break // it's not possible to recover, so close the connection
			}
			req.h.Error = err.Error()
			// 回复请求 虽然出错了，但是还是有请求，所以需要做响应，没出错的请求在handleRequest中互斥处理请求
			server.sendResponse(cc, req.h, invalidRequest, sending)
			continue
		}
		//main协程通过调用wg.Add(delta int)设置worker协程的个数，然后创建worker协程
		wg.Add(1)
		// 创建协程 处理请求
		go server.handleRequest(cc, req, sending, wg, opt.HandleTimeout)
	}
	// main协程调用wg.Wait()且被block,直到所有的worker协程全部执行结束后返回
	wg.Wait()
	_ = cc.Close()
}

// readRequestHeader 读取请求
func (server *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	//使用gob读取header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Println("rpc server: read header error:", err)
		}
		return nil, err
	}
	return &h, nil
}

func (server *Server) readRequest(cc codec.Codec) (*request, error) {
	// 读取请求的头
	h, err := server.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}
	//拼凑请求
	req := &request{h: h}
	req.svc, req.mtype, err = server.findService(h.ServiceMethod)
	if err != nil {
		return nil, err
	}
	// 创建两个入参实例
	req.argv = req.mtype.newArgv()
	req.replyv = req.mtype.newReplyv()
	// make sure that argvi is a pointer, ReadBody need a pointer as parameter
	argvi := req.argv.Interface()
	// argv同样需要是值还是指针，分别处理
	if req.argv.Type().Kind() != reflect.Ptr {
		argvi = req.argv.Addr().Interface()
	}
	if err = cc.ReadBody(argvi); err != nil {
		log.Println("rpc server: read body err:", err)
		return req, err
	}
	return req, nil
}

//  sendResponse 发送响应
func (server *Server) sendResponse(cc codec.Codec, h *codec.Header, body interface{}, sending *sync.Mutex) {
	sending.Lock()
	defer sending.Unlock()
	// 对response的body的Write需要互斥进行
	if err := cc.Write(h, body); err != nil {
		log.Println("rpc server: write response error:", err)
	}
}

// handleRequest 并发处理请求
func (server *Server) handleRequest(cc codec.Codec, req *request, sending *sync.Mutex, wg *sync.WaitGroup, timeout time.Duration) {
	// 在worker协程结束以后，都要调用wg.Done()
	defer wg.Done()
	called := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		err := req.svc.call(req.mtype, req.argv, req.replyv)
		called <- struct{}{}
		if err != nil {
			req.h.Error = err.Error()
			server.sendResponse(cc, req.h, invalidRequest, sending)
			sent <- struct{}{}
			return
		}
		server.sendResponse(cc, req.h, req.replyv.Interface(), sending)
		sent <- struct{}{}
	}()

	if timeout == 0 {
		<-called
		<-sent
		return
	}
	select {
	case <-time.After(timeout):
		req.h.Error = fmt.Sprintf("rpc server: request handle timeout: expect within %s", timeout)
		server.sendResponse(cc, req.h, invalidRequest, sending)
	case <-called:
		<-sent
	}
}

// Register publishes in the server the set of methods of the
func (server *Server) Register(rcvr interface{}) error {
	s := newService(rcvr)
	if _, dup := server.serviceMap.LoadOrStore(s.name, s); dup {
		return errors.New("rpc: service already defined: " + s.name)
	}
	return nil
}

// Register publishes the receiver's methods in the DefaultServer.
func Register(rcvr interface{}) error { return DefaultServer.Register(rcvr) }

// findService 通过 ServiceMethod 从 serviceMap 中找到对应的 service
func (server *Server) findService(serviceMethod string) (svc *service, mtype *methodType, err error) {
	// 将 ServiceMethod分割称两部分，第一部分是Service的名称，第二部分即方法名
	dot := strings.LastIndex(serviceMethod, ".")
	if dot < 0 {
		err = errors.New("rpc server: service/method request ill-formed: " + serviceMethod)
		return
	}
	serviceName, methodName := serviceMethod[:dot], serviceMethod[dot+1:]
	svci, ok := server.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("rpc server: can't find service " + serviceName)
		return
	}
	svc = svci.(*service)
	mtype = svc.method[methodName]
	if mtype == nil {
		err = errors.New("rpc server: can't find method " + methodName)
	}
	return
}

// ServeHTTP implements an http.Handler that answers RPC requests.
func (server *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "CONNECT" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "405 must CONNECT\n")
		return
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
	server.ServeConn(conn)
}

// HandleHTTP registers an HTTP handler for RPC messages on rpcPath.
// It is still necessary to invoke http.Serve(), typically in a go statement.
func (server *Server) HandleHTTP() {
	http.Handle(defaultRPCPath, server)
}

// HandleHTTP is a convenient approach for default server to register HTTP handlers
func HandleHTTP() {
	DefaultServer.HandleHTTP()
}
