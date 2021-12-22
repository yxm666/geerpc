package geerpc

import (
	"encoding/json"
	"fmt"
	"gee-rpc/codec"
	"io"
	"log"
	"net"
	"reflect"
	"sync"
)

const MagicNumber = 0x3bef5c

/*
	GeeRpc客户端固定采用JSON编码Option，后续的header和Body的编码方式由Option中的CodeType指定
	服务端首先使用JSON编码Option，然后通过Option的CodeType解码剩余的内容
	| Option{MagicNumber: xxx, CodecType: xxx} | Header{ServiceMethod ...} | Body interface{} |
	| <------      固定 JSON 编码      ------>  | <-------   编码方式由 CodeType 决定   ------->
	在一次链接中，Option固定在报文的最开始，Header和Body可以由多个
	| Option | Header1 | Body1 | Header2 | Body2 | ...
*/

// Option 消息的编解码方式
type Option struct {
	MagicNumber int
	CodeType    codec.Type
}

// Server 作为RPC的服务端
type Server struct{}

var (
	// DefaultServer 是默认的*Server实例 为了用户使用方便
	DefaultServer = NewServer()
	// DefaultOptions 是默认的Option实例
	DefaultOptions = &Option{
		MagicNumber: MagicNumber,
		CodeType:    codec.GobType,
	}
	// invalidRequest 是响应 argv 发生错误时的占位符
	invalidRequest = struct{}{}
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
		go server.ServerConn(conn)

	}
}

// Accept 为每一个连接的请求，接受侦听器上的连接并提供请求
func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

func (server *Server) ServerConn(conn io.ReadWriteCloser) {
	defer func() { _ = conn.Close() }()
	var opt Option
	//NewDecoder 返回一个新的解码器，该解码器从 r 读取数据
	// 首先使用json.NewDecoder 反序列化得到Option实例
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("rpc server option  error:", err)
	}
	// 检查MagicNumber 和 CodeType的值是否正确
	if opt.MagicNumber != MagicNumber {
		log.Printf("rpc server invalid magic number: %x", opt.MagicNumber)
	}
	f := codec.NewCodeFuncMap[opt.CodeType]
	if f == nil {
		log.Printf("rpc server: invalid codec type %s", opt.CodeType)
		return
	}
	// 接下来的处理交给 serveCodec
	server.serveCodec(f(conn))
}

// serveCodec 主要包含三个阶段
func (server *Server) serveCodec(cc codec.Codec) {
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
			// 回复请求 虽然出错了，但是还是有请求，所以需要做响应
			server.sendResponse(cc, req.h, invalidRequest, sending)
			continue
		}
		//main协程通过调用wg.Add(delta int)设置worker协程的个数，然后创建worker协程
		wg.Add(1)
		// 创建协程 处理请求
		go server.handleRequest(cc, req, sending, wg)
	}
	// main协程调用wg.Wait()且被block,直到所有的worker协程全部执行结束后返回
	wg.Wait()
	_ = cc.Close()
}

// readRequestHeader 读取请求
func (server *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Println("rpc server: read header error:", err)
		}
		return nil, err
	}
	return &h, nil
}

func (server *Server) readRequest(cc codec.Codec) (*request, error) {
	h, err := server.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}
	req := &request{h: h}
	// TODO: now we don't know the type of request argv
	// day 1, just suppose it's string
	req.argv = reflect.New(reflect.TypeOf(""))
	if err = cc.ReadBody(req.argv.Interface()); err != nil {
		log.Println("rpc server: read argv err:", err)
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
func (server *Server) handleRequest(cc codec.Codec, req *request, sending *sync.Mutex, wg *sync.WaitGroup) {
	// TODO, should call registered rpc methods to get the right replyv
	// day 1, just print argv and send a hello message
	// 在worker协程结束以后，都要调用wg.Done()
	defer wg.Done()
	log.Println(req.h, req.argv.Elem())
	req.replyv = reflect.ValueOf(fmt.Sprintf("geerpc resp %d", req.h.Seq))
	server.sendResponse(cc, req.h, req.replyv.Interface(), sending)
}

type request struct {
	h            *codec.Header
	argv, replyv reflect.Value
}
