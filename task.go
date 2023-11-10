package radar

import (
	"context"
	"github.com/vela-ssoc/vela-kit/iputil"
	"github.com/vela-ssoc/vela-kit/kind"
	"github.com/vela-ssoc/vela-kit/lua"
	"github.com/vela-ssoc/vela-kit/thread"
	"github.com/vela-ssoc/vela-radar/host"
	"github.com/vela-ssoc/vela-radar/port"
	"github.com/vela-ssoc/vela-radar/port/syn"
	"github.com/vela-ssoc/vela-radar/port/tcp"
	"github.com/vela-ssoc/vela-radar/util"
	"net"
	"sync"
	"time"
)

type Dispatch interface {
	End()
	Callback(Tx)
	Catch(error)
}

type Worker struct {
	Ping        *thread.PoolWithFunc
	Scan        *thread.PoolWithFunc
	FingerPrint *thread.PoolWithFunc
}

type WaitGroup struct {
	Ping        sync.WaitGroup
	Scan        sync.WaitGroup
	FingerPrint sync.WaitGroup
}

func (wg *WaitGroup) Wait() {
	wg.Ping.Wait()
	wg.Scan.Wait()
	wg.FingerPrint.Wait()
}

type Pool struct {
	Ping   int
	Scan   int
	Finger int
}

type Option struct {
	ID       string
	Location string
	Mode     string
	Target   string
	Port     string
	Rate     int
	Timeout  int
	Httpx    bool
	Ping     bool
	Pool     Pool
	Ctime    time.Time
}

type Scanner interface {
	Scan(net.IP, uint16) error
	WaitLimiter() error
	Wait()
	Close()
}

type Task struct {
	Option    Option
	Dispatch  Dispatch
	Worker    Worker
	WaitGroup WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
}

func (t *Task) String() string                         { return "" }
func (t *Task) Type() lua.LValueType                   { return lua.LTObject }
func (t *Task) AssertFloat64() (float64, bool)         { return 0, false }
func (t *Task) AssertString() (string, bool)           { return "", false }
func (t *Task) AssertFunction() (*lua.LFunction, bool) { return nil, false }
func (t *Task) Peek() lua.LValue                       { return t }

func (t *Task) close() error {
	if t.cancel == nil {
		return nil
	}
	t.cancel()
	return nil
}

func (t *Task) info() []byte {
	enc := kind.NewJsonEncoder()
	enc.Tab("")
	enc.KV("id", t.Option.ID)
	enc.KV("status", "working")
	enc.KV("ctime", t.Option.Ctime)
	enc.End("}")
	return enc.Bytes()
}

func (t *Task) GenRun() {
	if t.Dispatch == nil {
		xEnv.Errorf("%s dispatch got nil")
		return
	}

	var ss Scanner
	var err error
	wg := new(WaitGroup)

	// parse ip
	it, startIp, err := iputil.NewIter(t.Option.Target)
	if err != nil {
		xEnv.Errorf("task ip range parse fail %v", err)
		return
	}

	// 解析端口字符串并且优先发送 TopTcpPorts 中的端口, eg: 1-65535,top1000
	ports, err := port.ShuffleParseAndMergeTopPorts(t.Option.Port)
	if err != nil {
		xEnv.Errorf("task port range parse fail %v", err)
		return
	}

	fingerPool, _ := thread.NewPoolWithFunc(t.Option.Pool.Finger, func(v interface{}) {
		entry := v.(port.OpenIpPort)
		t.Dispatch.Callback(Tx{Entry: entry, Param: t.Option})
		wg.FingerPrint.Done()
	})
	defer fingerPool.Release()

	call := func(v port.OpenIpPort) {
		wg.FingerPrint.Add(1)
		fingerPool.Invoke(v)
	}

	switch t.Option.Mode {
	case "syn":
		ss, err = syn.NewSynScanner(startIp, call, port.Option{
			Rate:    t.Option.Rate,
			Timeout: t.Option.Timeout,
		})
	default:
		ss, err = tcp.NewTcpScanner(call, port.Option{
			Rate:    t.Option.Rate,
			Timeout: t.Option.Timeout,
		})
	}

	// port scan func
	scanner := func(ip net.IP) {
		n := len(ports)
		if n == 1 {
			ss.Scan(ip, ports[0])
			return
		}

		for i := 0; i < n; i++ {
			ss.WaitLimiter() // limit rate
			ss.Scan(ip, ports[i])
		}
	}

	// host group scan func
	hostScan, _ := thread.NewPoolWithFunc(t.Option.Pool.Scan, func(v interface{}) {
		ip := v.(net.IP)
		scanner(ip)
		wg.Scan.Done()
	})
	defer hostScan.Release()

	// Pool - ping and port scan
	poolPing, _ := thread.NewPoolWithFunc(t.Option.Pool.Ping, func(v interface{}) {
		ip := v.(net.IP)
		if host.IsLive(ip.String(), false, 800*time.Millisecond) {
			wg.Scan.Add(1)
			hostScan.Invoke(ip)
		}
		wg.Ping.Done()
	})
	defer poolPing.Release()

	shuffle := util.NewShuffle(it.TotalNum())    // shuffle
	for i := uint64(0); i < it.TotalNum(); i++ { // ip index
		ip := make(net.IP, len(it.GetIpByIndex(0)))
		copy(ip, it.GetIpByIndex(shuffle.Get(i))) // Note: dup copy []byte when concurrent (GetIpByIndex not to do dup copy)
		if !t.Option.Ping {
			wg.Ping.Add(1)
			_ = poolPing.Invoke(ip)
		} else {
			wg.Scan.Add(1)
			_ = hostScan.Invoke(ip)
		}
	}

	wg.Wait()
	ss.Wait()
	ss.Close()
	t.Dispatch.End()
}
