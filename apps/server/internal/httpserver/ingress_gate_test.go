package httpserver

import (
	"testing"
	"time"
)

func TestIngressGateConcurrentSealAndAdmission(t *testing.T) {
	for range 32 {
		var gate IngressGate
		start := make(chan struct{})
		admitted := make(chan bool, 1)
		go func() {
			<-start
			admitted <- gate.enter()
		}()
		close(start)
		drained := gate.Seal()
		if gate.Seal() != drained {
			t.Error("重复封口应返回同一个完成信号")
		}
		if gate.enter() {
			gate.leave()
			t.Error("封口后不得准入新请求")
		}
		if <-admitted {
			select {
			case <-drained:
				t.Error("已准入的请求尚未释放，完成信号却已关闭")
			default:
			}
			gate.leave()
		}
		select {
		case <-drained:
		case <-time.After(time.Second):
			t.Fatal("封口后在途计数归零，完成信号未关闭")
		}
	}
}
