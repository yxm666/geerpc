package codec

import "io"

/*
	- 使用 encoding/gob 实现消息的编解码(序列化与😡序列化)
	- 实现了一个简易的服务端，仅接受消息，不处理
*/

// Header 客户端发送的请求包含服务名Arith，方法名 Mutiply,参数args 三个
// 服务端的响应包括错误 error 返回值 reply 2个
//将请求和响应的参数和返回值抽象为 body，剩余的信息放在 header中

// 定义了两种Codec
const (
	GobType  Type = "application/gob"
	JsonType Type = "application/json" // not implemented
)

var NewCodeFuncMap map[Type]NewCodeFunc

// Header 头信息
type Header struct {
	// ServiceMethod 服务名和方法名
	ServiceMethod string
	// Seq 请求的序号 也可以认为是某个请求的ID 用来区分不同的请求
	Seq uint64
	// Error 客户端置为空，服务端如果发生错误，将错误信息置于 Error 中
	Error string
}

//Codec 消息体
type Codec interface {
	io.Closer
	ReadHeader(header *Header) error
	ReadBody(interface{}) error
	Write(*Header, interface{}) error
}

type Type string

type NewCodeFunc func(closer io.ReadWriteCloser) Codec

// init Codec的构造函数
// 客户端和服务端可通过Codec的Type得到构造函数,从而创建Codec实例
func init() {
	NewCodeFuncMap = make(map[Type]NewCodeFunc)
	NewCodeFuncMap[GobType] = NewGobCodec
}
