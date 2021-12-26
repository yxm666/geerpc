package codec

import (
	"bufio"
	"encoding/gob"
	"io"
	"log"
)

type GobCodec struct {
	// conn 由构造函数传入，通常是TCP/Unix建立Socket时得到的链接实例
	// ReadWriteCloser 是对基本的 Read、 Write 和 Close 方法进行分组的接口。
	conn io.ReadWriteCloser
	// buf 为了防止阻塞而创建的带缓冲的Writer
	buf *bufio.Writer
	// dec 对应gob的Decoder(解码器)
	dec *gob.Decoder
	// enc 对应gob的Encoder(编码器)
	enc *gob.Encoder
}

// NewGobCodec 构造方法，传入conn返回一个GobCodec指针
func NewGobCodec(conn io.ReadWriteCloser) Codec {
	// NewWriter 创建一个具有默认大小缓冲、写入w的*Writer。
	buf := bufio.NewWriter(conn)
	return &GobCodec{
		conn: conn,
		buf:  buf,
		dec:  gob.NewDecoder(conn),
		enc:  gob.NewEncoder(conn),
	}
}

/*
	Read and Write 都使用了 Gob的编码解码器
*/
func (c *GobCodec) Close() error {
	return c.conn.Close()
}

func (c *GobCodec) ReadHeader(header *Header) error {
	return c.dec.Decode(header)
}

func (c *GobCodec) ReadBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *GobCodec) Write(header *Header, body interface{}) (err error) {
	defer func() {
		_ = c.buf.Flush()
		if err != nil {
			_ = c.Close()
		}
	}()
	if err := c.enc.Encode(header); err != nil {
		log.Println("rpc codec: gob error encoding body:", err)
		return err
	}
	if err := c.enc.Encode(body); err != nil {
		log.Println("rpc codec: gob error encoding body:", err)
		return err
	}
	return nil
}

//以下划线开头的变量:_interface = (*struct)(nil) 表明结构体实现了接口
var _ Codec = (*GobCodec)(nil)
