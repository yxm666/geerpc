package geerpc

import (
	"go/ast"
	"log"
	"reflect"
	"sync/atomic"
)

// methodType 使用反射将结构体与服务的映射关系
// 每一个 methodType 实例包含了一个方法的完整信息
type methodType struct {
	// method 方法本身
	method reflect.Method
	// ArgType 第一个参数类型
	ArgType reflect.Type
	// ReplyType 第二个参数类型
	ReplyType reflect.Type
	// numCalls 后续统计方法调用次数会用到
	numCalls uint64
}

func (m *methodType) NumCalls() uint64 {
	return atomic.LoadUint64(&m.numCalls)
}

// newArgval 创建对应类型的实例
func (m *methodType) newArgv() reflect.Value {
	// Value 是 Go 值的反射接口。
	var argv reflect.Value
	// 如果第一个参数是指针类型
	if m.ArgType.Kind() == reflect.Ptr {
		// 指针类型创建实例
		argv = reflect.New(m.ArgType.Elem())
	} else {
		// 值类型创建实例
		argv = reflect.New(m.ArgType).Elem()
	}
	return argv
}

// newReplyv 创建对应类型的实例
func (m *methodType) newReplyv() reflect.Value {
	// reply must be a pointer type
	replyv := reflect.New(m.ReplyType.Elem())
	switch m.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(m.ReplyType.Elem()))
	case reflect.Slice:
		replyv.Elem().Set(reflect.MakeSlice(m.ReplyType.Elem(), 0, 0))
	}
	return replyv
}

type service struct {
	// name 映射的结构体名称
	name string
	// typ 结构体类型
	typ reflect.Type
	// rcvr 结构体的实例本身，保存rcvr是因为在调用时需要rcvr作为第0个参数
	rcvr reflect.Value
	// method是map类型，存储映射的结构体的所有符合条件的方法
	method map[string]*methodType
}

// newService 构造函数，入参是任意需要映射为服务的结构体实例
func newService(rcvr interface{}) *service {
	s := new(service)
	s.rcvr = reflect.ValueOf(rcvr)
	s.name = reflect.Indirect(s.rcvr).Type().Name()
	s.typ = reflect.TypeOf(rcvr)
	if !ast.IsExported(s.name) {
		log.Fatalf("rpc server: %s is not a valid service name", s.name)
	}
	s.registerMethods()
	return s
}

// registerMethods 过滤出了符合条件的方法:
// 两个导出或内置类型的入参（反射时为3个，第0个是自身，类似与python的self，java的this)
// 返回值有且只有一个，类型为error
func (s *service) registerMethods() {
	s.method = make(map[string]*methodType)
	for i := 0; i < s.typ.NumMethod(); i++ {
		method := s.typ.Method(i)
		mType := method.Type
		if mType.NumIn() != 3 || mType.NumOut() != 1 {
			continue
		}
		if mType.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
			continue
		}
		argType, replyType := mType.In(1), mType.In(2)
		if !isExportedOrBuiltinType(argType) || !isExportedOrBuiltinType(replyType) {
			continue
		}
		s.method[method.Name] = &methodType{
			method:    method,
			ArgType:   argType,
			ReplyType: replyType,
		}
		log.Printf("rpc server: register %s.%s\n", s.name, method.Name)
	}
}

func isExportedOrBuiltinType(t reflect.Type) bool {
	return ast.IsExported(t.Name()) || t.PkgPath() == ""
}

// call 能够通过反射值调用方法
func (s *service) call(m *methodType, argv, replyv reflect.Value) error {
	atomic.AddUint64(&m.numCalls, 1)
	f := m.method.Func
	returnValues := f.Call([]reflect.Value{s.rcvr, argv, replyv})
	if errInter := returnValues[0].Interface(); errInter != nil {
		return errInter.(error)
	}
	return nil
}
