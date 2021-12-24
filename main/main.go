package main

import (
	"fmt"
	geerpc "gee-rpc"
	"log"
	"net"
	"sync"
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
	log.SetFlags(0)
	addr := make(chan string)
	go startServer(addr)
	client, _ := geerpc.Dial("tcp", <-addr)
	// 创建个方法就为了接受Close错误，然后接受，但是不关注error，用_接受(只能在func里使用)
	defer func() { _ = client.Close() }()

	time.Sleep(time.Second)

	// 客户端首先发送Option进行协议交换
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := fmt.Sprintf("geerpc req %d", i)
			var reply string
			if err := client.Call("Foo.Sum", args, &reply); err != nil {
				log.Fatal("call Foo.Sum error:", err)
			}
			log.Println("reply:", reply)
		}(i)
	}
	wg.Wait()
}
