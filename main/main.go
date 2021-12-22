package main

import (
	"encoding/json"
	"fmt"
	geerpc "gee-rpc"
	"gee-rpc/codec"
	"log"
	"net"
	"time"
)

// startServer 使用了通道 addr，确保了服务端口监听成功，客户端再发起请求
func startServer(addr chan string) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatal("networking", err)
	}
	log.Println("start rpc server on", l.Addr())
	addr <- l.Addr().String()
	geerpc.Accept(l)
}

func main() {
	addr := make(chan string)
	go startServer(addr)
	conn, _ := net.Dial("tcp", <-addr)
	// 创建个方法就为了接受Close错误，然后接受，但是不关注error，用_接受(只能在func里使用)
	defer func() { _ = conn.Close() }()

	time.Sleep(time.Second)

	// 客户端首先发送Option进行协议交换
	_ = json.NewEncoder(conn).Encode(geerpc.DefaultOptions)
	cc := codec.NewGobCodec(conn)
	for i := 0; i < 5; i++ {
		// 发送消息请求头
		h := &codec.Header{
			ServiceMethod: "Foo.Sum",
			Seq:           uint64(i),
		}
		//发送消息体
		_ = cc.Write(h, fmt.Sprintf("geerpc req %d", h.Seq))
		_ = cc.ReadHeader(h)
		var reply string
		// 解析服务端的响应reply，并打印
		_ = cc.ReadBody(&reply)
		log.Println("reply:", reply)
	}
}
